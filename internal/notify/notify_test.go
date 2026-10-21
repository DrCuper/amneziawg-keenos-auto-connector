package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

func noSleep(context.Context, time.Duration) error { return nil }

func TestWebhookSignatureAndRetry(t *testing.T) {
	var calls atomic.Int32
	var gotSig, gotEvent string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		gotSig, gotEvent = r.Header.Get("X-Awgkeen-Signature"), r.Header.Get("X-Awgkeen-Event")
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	w := &Webhook{URL: srv.URL + "/hook/s3cr3t-path", Secret: "k", HTTPOptions: HTTPOptions{Retries: 2, Sleep: noSleep}}
	if err := w.Send(context.Background(), &Message{Event: EventFiring, Alert: AlertSyncFailed, Title: "Sync failed"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls=%d", calls.Load())
	}
	if gotEvent != EventFiring || gotSig != Signature("k", gotBody) {
		t.Errorf("event=%q sig=%q", gotEvent, gotSig)
	}
	var m Message
	if err := json.Unmarshal(gotBody, &m); err != nil || m.Alert != AlertSyncFailed {
		t.Errorf("body %s: %v", gotBody, err)
	}
}

func TestWebhookNoRetryOn4xxAndNoURLInError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	w := &Webhook{URL: srv.URL + "/hook/s3cr3t-path", HTTPOptions: HTTPOptions{Retries: 3, Sleep: noSleep}}
	err := w.Send(context.Background(), &Message{Event: EventTest})
	if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
	// Network error: the URL (with its secret path) must not leak either.
	w.URL = "http://127.0.0.1:1/hook/s3cr3t-path"
	w.Retries = 0
	if err := w.Send(context.Background(), &Message{Event: EventTest}); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("network err: %v", err)
	}
}

func TestTelegram(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	tg := &Telegram{Token: "123:ABC", ChatID: "-100", APIURL: srv.URL}
	since := time.Now().Add(-time.Hour)
	if err := tg.Send(context.Background(), &Message{Event: EventFiring, Title: "Router unreachable", Text: strings.Repeat("x", 5000), Since: &since, Failures: 2, Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if path != "/bot123:ABC/sendMessage" || got["chat_id"] != "-100" {
		t.Errorf("path=%s body=%v", path, got)
	}
	if text := got["text"].(string); len([]rune(text)) > telegramMaxText || !strings.HasPrefix(text, "🔴 Router unreachable") {
		t.Errorf("text len %d: %.40q", len([]rune(text)), text)
	}
}

func TestTelegramErrorHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
	}))
	defer srv.Close()
	tg := &Telegram{Token: "123:SECRET", ChatID: "1", APIURL: srv.URL, HTTPOptions: HTTPOptions{Retries: 2, Sleep: noSleep}}
	err := tg.Send(context.Background(), &Message{Event: EventTest, Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err: %v", err)
	}
	tg.APIURL = "http://127.0.0.1:1"
	if err := tg.Send(context.Background(), &Message{Event: EventTest, Title: "t"}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("network err: %v", err)
	}
}

type recorder struct {
	mu   sync.Mutex
	msgs []*Message
	fail bool
}

func (r *recorder) Name() string { return "rec" }
func (r *recorder) Send(_ context.Context, m *Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("down")
	}
	r.msgs = append(r.msgs, m)
	return nil
}
func (r *recorder) events() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.msgs {
		out = append(out, m.Event+":"+m.Key)
	}
	return strings.Join(out, " ")
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newAlerter(t *testing.T, rec *recorder, c *clock) *Alerter {
	return &Alerter{Channels: []Channel{rec}, StatePath: filepath.Join(t.TempDir(), "alerts.json"), RepeatInterval: 6 * time.Hour, Now: c.now}
}

var errDown = errors.New("connection refused")

func TestAlerterDedupReminderResolve(t *testing.T) {
	rec := &recorder{}
	c := &clock{t: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}
	a := newAlerter(t, rec, c)
	ctx := context.Background()
	bad := Signal{Alert: AlertRouterUnreachable, Err: errDown}
	for i := 0; i < 3; i++ {
		a.Update(ctx, bad)
		c.t = c.t.Add(20 * time.Minute)
	}
	if rec.events() != "firing:router_unreachable" {
		t.Fatalf("after 3 failures: %s", rec.events())
	}
	c.t = c.t.Add(6 * time.Hour)
	a.Update(ctx, bad)
	if !strings.HasSuffix(rec.events(), "reminder:router_unreachable") || rec.msgs[1].Failures != 4 {
		t.Fatalf("reminder: %s", rec.events())
	}
	a.Update(ctx, Signal{Alert: AlertRouterUnreachable})
	if !strings.HasSuffix(rec.events(), "resolved:router_unreachable") {
		t.Fatalf("resolved: %s", rec.events())
	}
	if !strings.Contains(rec.msgs[2].Plain(), "Duration: 7h0m0s") {
		t.Errorf("resolved text: %s", rec.msgs[2].Plain())
	}
	// Healthy again: further successes are silent, the next failure fires again.
	a.Update(ctx, Signal{Alert: AlertRouterUnreachable})
	a.Update(ctx, bad)
	if n := len(rec.msgs); n != 4 || rec.msgs[3].Event != EventFiring {
		t.Fatalf("refire: %s", rec.events())
	}
}

func TestAlerterThresholdAndRestart(t *testing.T) {
	rec := &recorder{}
	c := &clock{t: time.Now()}
	a := newAlerter(t, rec, c)
	a.FailThreshold = 3
	ctx := context.Background()
	bad := Signal{Alert: AlertSyncFailed, Err: errDown}
	a.Update(ctx, bad)
	a.Update(ctx, bad)
	if rec.events() != "" {
		t.Fatalf("below threshold: %s", rec.events())
	}
	a.Update(ctx, bad)
	if rec.events() != "firing:sync_failed" {
		t.Fatalf("at threshold: %s", rec.events())
	}
	// "Restart": a new Alerter (another process) over the same state file.
	b := &Alerter{Channels: []Channel{rec}, StatePath: a.StatePath, FailThreshold: 3, RepeatInterval: time.Hour, Now: c.now}
	b.Update(ctx, bad)
	if rec.events() != "firing:sync_failed" {
		t.Fatalf("after restart: %s", rec.events())
	}
	b.Update(ctx, Signal{Alert: AlertSyncFailed})
	if rec.events() != "firing:sync_failed resolved:sync_failed" {
		t.Fatalf("resolve from other process: %s", rec.events())
	}
	if st, _ := a.States(); len(st) != 0 {
		t.Errorf("state not cleared: %+v", st)
	}
}

func TestAlerterRetriesUndeliveredFiring(t *testing.T) {
	rec := &recorder{fail: true}
	c := &clock{t: time.Now()}
	a := newAlerter(t, rec, c)
	a.RepeatInterval = 0
	bad := Signal{Alert: AlertAWGDown, Check: "awg", Err: errDown}
	a.Update(context.Background(), bad)
	rec.fail = false
	a.Update(context.Background(), bad)
	a.Update(context.Background(), bad)
	if rec.events() != "firing:awg_down:awg" {
		t.Fatalf("events: %s", rec.events())
	}
	st, _ := a.States()
	if len(st) != 1 || !st[0].Active || !st[0].Notified || st[0].Failures != 3 || st[0].Check != "awg" {
		t.Errorf("state: %+v", st)
	}
}

func TestClassify(t *testing.T) {
	te := fmt.Errorf("apply: %w", fmt.Errorf("running-config failed after 4 attempt(s): %w", &router.TransportError{Op: "running-config", Err: errDown}))
	ce := fmt.Errorf("apply: %w", &router.CommandError{Command: "x", Message: "bad"})
	withApply := &syncer.Result{Apply: &router.ApplyResult{}}
	cases := []struct {
		name string
		r    syncer.Report
		want string
	}{
		{"ok", syncer.Report{}, "router_unreachable=ok sync_failed=ok"},
		{"transport", syncer.Report{Err: te}, "router_unreachable=bad"},
		{"locked", syncer.Report{Err: fmt.Errorf("apply: %w", router.ErrLocked)}, ""},
		{"cancel", syncer.Report{Err: context.Canceled}, ""},
		{"rejected", syncer.Report{Err: ce, Result: withApply}, "router_unreachable=ok sync_failed=bad"},
		{"groups only", syncer.Report{Err: syncer.ErrGroupsFailed, Result: &syncer.Result{}}, "sync_failed=bad"},
	}
	for _, c := range cases {
		var got []string
		for _, s := range Classify(&c.r) {
			v := "ok"
			if s.Err != nil {
				v = "bad"
			}
			got = append(got, s.Key()+"="+v)
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: got %q want %q", c.name, strings.Join(got, " "), c.want)
		}
	}
}

func TestObserveSkipsDryRunAndManual(t *testing.T) {
	rec := &recorder{}
	a := newAlerter(t, rec, &clock{t: time.Now()})
	a.OnApply = true
	te := &router.TransportError{Op: "x", Err: errDown}
	a.Observe(context.Background(), &syncer.Report{Kind: syncer.KindSync, DryRun: true, Err: te})
	a.Observe(context.Background(), &syncer.Report{Kind: syncer.KindManual})
	if st, _ := a.States(); len(st) != 0 || rec.events() != "" {
		t.Fatalf("dry-run/manual changed alerts: %+v %s", st, rec.events())
	}
	res := &syncer.Result{
		Groups: []syncer.GroupResult{{Name: "a", Connection: "awg", Version: 2, PrevApplied: 1, Entries: 10, Added: 3}},
		Apply:  &router.ApplyResult{Applied: 15, Saved: true},
	}
	a.Observe(context.Background(), &syncer.Report{Kind: syncer.KindSync, Result: res})
	if rec.events() != "info:" || !strings.Contains(rec.msgs[0].Text, "a → awg: v2, 10 entries, +3/-0") || !strings.Contains(rec.msgs[0].Text, "15 command(s)") {
		t.Fatalf("info: %s %+v", rec.events(), rec.msgs)
	}
}
