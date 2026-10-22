package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/lock"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// Alert names.
const (
	AlertRouterUnreachable = "router_unreachable"
	AlertSyncFailed        = "sync_failed"
	AlertAWGDown           = "awg_down"
	AlertServerDown        = "server_down"
)

// maxDetail bounds the error text kept in state and sent in messages.
const maxDetail = 1500

// Signal is one observation for an alert: Err == nil means "healthy".
type Signal struct {
	Alert string // AlertRouterUnreachable, ...
	// Check distinguishes several alerts of one kind (awg_down:<check>).
	Check string
	Err   error
}

// Key is the alert's state key.
func (s Signal) Key() string {
	if s.Check == "" {
		return s.Alert
	}
	return s.Alert + ":" + s.Check
}

// State is the persisted state of one alert key.
type State struct {
	Alert    string    `json:"alert"`
	Check    string    `json:"check,omitempty"`
	Active   bool      `json:"active"`
	Failures int       `json:"failures"`
	Since    time.Time `json:"since,omitzero"` // first failure of the streak
	LastSent time.Time `json:"last_sent,omitzero"`
	Notified bool      `json:"notified,omitempty"` // firing was delivered
	Detail   string    `json:"detail,omitempty"`
}

// Alerter turns signals into messages without spamming. State lives in a
// JSON file shared (under a file lock) by the daemon and CLI runs.
type Alerter struct {
	Channels []Channel
	// StatePath is alerts.json; empty keeps state in memory only.
	StatePath string
	// FailThreshold is how many consecutive failures fire an alert (min 1).
	FailThreshold int
	// RepeatInterval between reminders of an active alert; 0 = never.
	RepeatInterval time.Duration
	// OnApply sends an info message after every real sync that changed the router.
	OnApply bool
	Host    string
	Logger  *slog.Logger
	Now     func() time.Time

	lock *lock.Lock
	mem  map[string]*State
}

func (a *Alerter) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

func (a *Alerter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Alerter) acquire(ctx context.Context) (func(), error) {
	if a.lock == nil {
		path := ""
		if a.StatePath != "" {
			path = a.StatePath + ".lock"
		}
		a.lock = lock.New(path)
	}
	return a.lock.Acquire(ctx, 2*time.Minute)
}

func (a *Alerter) load() (map[string]*State, error) {
	if a.StatePath == "" {
		if a.mem == nil {
			a.mem = map[string]*State{}
		}
		return a.mem, nil
	}
	st := map[string]*State{}
	data, err := os.ReadFile(a.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		// A corrupt file must not silence alerts forever: start over.
		a.logger().Warn("notify: alert state unreadable, starting fresh", "path", a.StatePath, "err", err)
		return map[string]*State{}, nil
	}
	return st, nil
}

func (a *Alerter) save(st map[string]*State) error {
	if a.StatePath == "" {
		a.mem = st
		return nil
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.StatePath), 0o750); err != nil {
		return err
	}
	tmp := a.StatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.StatePath)
}

// Update applies signals and sends whatever messages they cause.
func (a *Alerter) Update(ctx context.Context, signals ...Signal) {
	if len(signals) == 0 {
		return
	}
	release, err := a.acquire(ctx)
	if err != nil {
		a.logger().Warn("notify: cannot lock alert state", "err", err)
		return
	}
	defer release()
	st, err := a.load()
	if err != nil {
		a.logger().Warn("notify: cannot read alert state", "err", err)
		return
	}
	for _, s := range signals {
		key := s.Key()
		cur := st[key]
		if cur == nil {
			cur = &State{Alert: s.Alert, Check: s.Check}
		}
		a.step(ctx, key, cur, s)
		if cur.Failures == 0 && !cur.Active {
			delete(st, key)
		} else {
			st[key] = cur
		}
	}
	if err := a.save(st); err != nil {
		a.logger().Warn("notify: cannot save alert state", "err", err)
	}
}

func (a *Alerter) step(ctx context.Context, key string, cur *State, s Signal) {
	now := a.now().UTC()
	if s.Err == nil {
		if cur.Active && cur.Notified {
			since := cur.Since
			a.send(ctx, &Message{Event: EventResolved, Alert: s.Alert, Key: key, Title: title(s.Alert, s.Check) + " resolved", Since: &since, Failures: cur.Failures, Time: now})
		}
		*cur = State{Alert: s.Alert, Check: s.Check}
		return
	}
	cur.Failures++
	if cur.Failures == 1 {
		cur.Since = now
	}
	cur.Detail = truncate(s.Err.Error(), maxDetail)
	threshold := max(a.FailThreshold, 1)
	if cur.Failures < threshold {
		return
	}
	since := cur.Since
	msg := &Message{Alert: s.Alert, Key: key, Title: title(s.Alert, s.Check), Text: cur.Detail, Since: &since, Failures: cur.Failures, Time: now}
	switch {
	case !cur.Active || !cur.Notified:
		cur.Active = true
		msg.Event = EventFiring
	case a.RepeatInterval > 0 && now.Sub(cur.LastSent) >= a.RepeatInterval:
		msg.Event = EventReminder
		msg.Title += " (still failing)"
	default:
		return
	}
	if a.send(ctx, msg) {
		cur.Notified = true
		cur.LastSent = now
	}
}

// send delivers to every channel; true when at least one succeeded (or there
// are no channels — then there is nobody to retry for).
func (a *Alerter) send(ctx context.Context, m *Message) bool {
	if m.Host == "" {
		m.Host = a.Host
	}
	if len(a.Channels) == 0 {
		a.logger().Info("notify: no channels configured", "event", m.Event, "title", m.Title)
		return true
	}
	ok := false
	for _, c := range a.Channels {
		if err := c.Send(ctx, m); err != nil {
			a.logger().Warn("notify: delivery failed", "channel", c.Name(), "event", m.Event, "key", m.Key, "err", err)
			continue
		}
		a.logger().Info("notify: sent", "channel", c.Name(), "event", m.Event, "key", m.Key)
		ok = true
	}
	return ok
}

// Send delivers one message to every channel and returns per-channel errors
// (nil entries for success), for `notify test`.
func (a *Alerter) Send(ctx context.Context, m *Message) map[string]error {
	if m.Host == "" {
		m.Host = a.Host
	}
	if m.Time.IsZero() {
		m.Time = a.now().UTC()
	}
	out := map[string]error{}
	for _, c := range a.Channels {
		out[c.Name()] = c.Send(ctx, m)
	}
	return out
}

// States returns the alert states (active and pending), sorted by key.
func (a *Alerter) States() ([]State, error) {
	st, err := a.load()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]State, 0, len(keys))
	for _, k := range keys {
		out = append(out, *st[k])
	}
	return out, nil
}

func title(alert, check string) string {
	t := map[string]string{
		AlertRouterUnreachable: "Router unreachable",
		AlertSyncFailed:        "Sync failed",
		AlertAWGDown:           "AWG down",
		AlertServerDown:        "AWG server down",
	}[alert]
	if t == "" {
		t = alert
	}
	if check != "" {
		t += ": " + check
	}
	return t
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Observe implements syncer.Observer: real syncs and rollbacks update the
// router_unreachable and sync_failed alerts (see design, decision 6).
func (a *Alerter) Observe(ctx context.Context, r *syncer.Report) {
	if r.DryRun || (r.Kind != syncer.KindSync && r.Kind != syncer.KindRollback) {
		return
	}
	a.Update(ctx, Classify(r)...)
	if a.OnApply && r.Err == nil && r.Result != nil && r.Result.Apply != nil && r.Result.Apply.Applied > 0 {
		a.send(ctx, &Message{Event: EventInfo, Title: "Sync applied", Text: applySummary(r.Result), Time: a.now().UTC()})
	}
}

var _ syncer.Observer = (*Alerter)(nil)

// Classify maps a sync report to alert signals.
func Classify(r *syncer.Report) []Signal {
	ok := func(alert string) Signal { return Signal{Alert: alert} }
	bad := func(alert string, err error) Signal { return Signal{Alert: alert, Err: err} }
	var te *router.TransportError
	switch err := r.Err; {
	case err == nil:
		return []Signal{ok(AlertRouterUnreachable), ok(AlertSyncFailed)}
	case errors.Is(err, router.ErrLocked), errors.Is(err, context.Canceled):
		return nil
	case errors.As(err, &te):
		return []Signal{bad(AlertRouterUnreachable, err)}
	case r.Result != nil && r.Result.Apply != nil:
		return []Signal{ok(AlertRouterUnreachable), bad(AlertSyncFailed, err)}
	default:
		return []Signal{bad(AlertSyncFailed, err)}
	}
}

func applySummary(res *syncer.Result) string {
	var b strings.Builder
	for _, g := range res.Groups {
		if g.Err != nil {
			continue
		}
		if g.Added == 0 && g.Removed == 0 && g.PrevApplied == g.Version {
			continue
		}
		fmt.Fprintf(&b, "%s → %s: v%d, %d entries, +%d/-%d\n", g.Name, g.Connection, g.Version, g.Entries, g.Added, g.Removed)
	}
	fmt.Fprintf(&b, "%d command(s) sent", res.Apply.Applied)
	if res.Apply.Saved {
		b.WriteString(", configuration saved")
	}
	return b.String()
}
