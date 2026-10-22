package awg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// RouterAPI is the part of router.Client the manager uses.
type RouterAPI interface {
	ReadState(ctx context.Context) (*router.State, error)
	Apply(ctx context.Context, want *router.Desired, dryRun bool) (*router.ApplyResult, error)
	InterfaceStatus(ctx context.Context, name string) (*router.WGStatus, error)
}

// Record is what the service remembers about an imported tunnel.
type Record struct {
	Interface string `json:"interface"`
	// KeyHash fingerprints the keys last applied successfully; keys are
	// re-sent when it changes (the router never shows them back).
	KeyHash   string    `json:"key_hash,omitempty"`
	ConfHash  string    `json:"conf_hash,omitempty"`
	AppliedAt time.Time `json:"applied_at,omitzero"`
}

// Manager imports tunnels onto the router.
type Manager struct {
	Router  RouterAPI
	Tunnels []Tunnel
	// BaseDir resolves relative conf_file paths.
	BaseDir string
	// StatePath is awg.json under state_dir; "" keeps records in memory.
	StatePath string
	Logger    *slog.Logger

	mu  sync.Mutex
	mem map[string]Record
}

// ApplyOptions tune Apply.
type ApplyOptions struct {
	DryRun bool
	// ForceKeys re-sends private/preshared keys even if unchanged.
	ForceKeys bool
}

// Item is the per-tunnel part of a result.
type Item struct {
	Tunnel    string `json:"tunnel"`
	Interface string `json:"interface"`
	// Created is true when the interface does not exist on the router yet.
	Created  bool     `json:"created"`
	Warnings []string `json:"warnings,omitempty"`
}

// Result reports what Apply or Remove did.
type Result struct {
	Items  []Item
	Apply  *router.ApplyResult
	DryRun bool
}

// Tunnel returns the configured tunnel by name.
func (m *Manager) Tunnel(name string) (Tunnel, bool) {
	for _, t := range m.Tunnels {
		if t.Name == name {
			return t, true
		}
	}
	return Tunnel{}, false
}

// Records returns the stored records by tunnel name.
func (m *Manager) Records() (map[string]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

// Resolve returns the interface of a tunnel: the configured one, or the one
// picked at first import for interface: auto.
func (m *Manager) Resolve(name string) (string, error) {
	t, ok := m.Tunnel(name)
	if !ok {
		return "", fmt.Errorf("unknown awg tunnel %q", name)
	}
	if !t.AutoInterface() {
		return t.Interface, nil
	}
	recs, err := m.Records()
	if err != nil {
		return "", err
	}
	if r, ok := recs[name]; ok && r.Interface != "" {
		return r.Interface, nil
	}
	return "", fmt.Errorf("awg tunnel %q is not imported yet (run `awgkeen awg apply`)", name)
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Manager) selectTunnels(names []string) ([]Tunnel, error) {
	if len(names) == 0 {
		if len(m.Tunnels) == 0 {
			return nil, errors.New("no awg tunnels configured (awg.tunnels in the config)")
		}
		return m.Tunnels, nil
	}
	var out []Tunnel
	for _, n := range names {
		t, ok := m.Tunnel(n)
		if !ok {
			return nil, fmt.Errorf("unknown awg tunnel %q", n)
		}
		out = append(out, t)
	}
	return out, nil
}

// Apply imports the tunnels (all when names is empty) in one router apply.
func (m *Manager) Apply(ctx context.Context, names []string, o ApplyOptions) (*Result, error) {
	tunnels, err := m.selectTunnels(names)
	if err != nil {
		return nil, err
	}
	st, err := m.Router.ReadState(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	recs, err := m.load()
	if err != nil {
		return nil, err
	}
	res := &Result{DryRun: o.DryRun}
	want := &router.Desired{}
	keyHashes := map[string]string{}
	confHashes := map[string]string{}
	var picked []string
	for _, t := range tunnels {
		c, err := t.Load(m.BaseDir)
		if err != nil {
			return nil, err
		}
		iface, err := m.resolveFor(t, st, recs, picked)
		if err != nil {
			return nil, err
		}
		picked = append(picked, iface)
		w := t.Desired(iface, c)
		kh := keyHash(c)
		w.SetKeys = o.ForceKeys || recs[t.Name].KeyHash != kh
		want.Interfaces = append(want.Interfaces, w)
		keyHashes[t.Name], confHashes[t.Name] = kh, confHash(w)
		_, exists := st.Interface(iface)
		res.Items = append(res.Items, Item{Tunnel: t.Name, Interface: iface, Created: !exists, Warnings: c.Warnings})
		m.logger().Debug("awg: tunnel resolved", "tunnel", t.Name, "interface", iface, "create", !exists, "send_keys", w.SetKeys)
	}
	if !o.DryRun {
		// Reserve the interfaces first: if the apply fails half-way, the
		// next run must reuse them rather than pick new free names.
		for _, it := range res.Items {
			r := recs[it.Tunnel]
			r.Interface = it.Interface
			recs[it.Tunnel] = r
		}
		if err := m.save(recs); err != nil {
			return nil, err
		}
	}
	ar, err := m.Router.Apply(ctx, want, o.DryRun)
	res.Apply = ar
	if err != nil || o.DryRun {
		return res, err
	}
	now := time.Now().UTC()
	for _, it := range res.Items {
		recs[it.Tunnel] = Record{Interface: it.Interface, KeyHash: keyHashes[it.Tunnel], ConfHash: confHashes[it.Tunnel], AppliedAt: now}
	}
	return res, m.save(recs)
}

// resolveFor picks the interface for a tunnel and refuses to take over an
// interface the service did not create unless adopt is set.
func (m *Manager) resolveFor(t Tunnel, st *router.State, recs map[string]Record, picked []string) (string, error) {
	owner := map[string]string{}
	for name, r := range recs {
		if r.Interface != "" {
			owner[strings.ToLower(r.Interface)] = name
		}
	}
	if !t.AutoInterface() {
		k := strings.ToLower(t.Interface)
		if o, ok := owner[k]; ok && o != t.Name {
			return "", fmt.Errorf("awg tunnel %q: interface %s belongs to tunnel %q (awgkeen awg remove %s first)", t.Name, t.Interface, o, o)
		}
		if _, exists := st.Interface(t.Interface); exists && owner[k] != t.Name && !t.Adopt {
			return "", fmt.Errorf("awg tunnel %q: interface %s already exists and was not created by awgkeen; set adopt: true to manage it (its peers will be replaced)", t.Name, t.Interface)
		}
		return t.Interface, nil
	}
	if r := recs[t.Name]; r.Interface != "" {
		return r.Interface, nil
	}
	taken := append([]string(nil), picked...)
	for k := range owner {
		taken = append(taken, k)
	}
	for _, other := range m.Tunnels {
		if !other.AutoInterface() {
			taken = append(taken, other.Interface)
		}
	}
	return st.FreeWireguardName(taken...), nil
}

// Remove deletes the tunnel's interface from the router and forgets it.
// Only interfaces created (or adopted) by the service are removed.
func (m *Manager) Remove(ctx context.Context, name string, dryRun bool) (*Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	recs, err := m.load()
	if err != nil {
		return nil, err
	}
	r, ok := recs[name]
	if !ok || r.Interface == "" {
		return nil, fmt.Errorf("awg tunnel %q was never imported by awgkeen; nothing to remove", name)
	}
	res := &Result{DryRun: dryRun, Items: []Item{{Tunnel: name, Interface: r.Interface}}}
	ar, err := m.Router.Apply(ctx, &router.Desired{RemoveInterfaces: []string{r.Interface}}, dryRun)
	res.Apply = ar
	if err != nil || dryRun {
		return res, err
	}
	delete(recs, name)
	return res, m.save(recs)
}

// Status is the health view of one tunnel.
type Status struct {
	Tunnel    string
	Interface string
	Router    *router.WGStatus
	// Err is nil when the tunnel is healthy.
	Err error
}

// Statuses reads the status of the tunnels (all when names is empty).
func (m *Manager) Statuses(ctx context.Context, names []string) ([]Status, error) {
	tunnels, err := m.selectTunnels(names)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(tunnels))
	for _, t := range tunnels {
		out = append(out, m.Status(ctx, t))
	}
	return out, nil
}

// Status reads and evaluates one tunnel.
func (m *Manager) Status(ctx context.Context, t Tunnel) Status {
	s := Status{Tunnel: t.Name}
	iface, err := m.Resolve(t.Name)
	if err != nil {
		s.Err = err
		return s
	}
	s.Interface = iface
	rs, err := m.Router.InterfaceStatus(ctx, iface)
	if err != nil {
		s.Err = err
		return s
	}
	s.Router = rs
	s.Err = Evaluate(rs, t.IsUp(), t.Check.MaxAge())
	return s
}

// Evaluate judges an interface status: it must exist, be up and have at
// least one peer with a handshake younger than maxAge. A tunnel configured
// as disabled is healthy when it exists.
func Evaluate(s *router.WGStatus, wantUp bool, maxAge time.Duration) error {
	switch {
	case s == nil || !s.Exists:
		return errors.New("interface not found on the router")
	case !wantUp:
		return nil
	case !s.Up:
		return fmt.Errorf("interface %s is down", s.Name)
	case len(s.Peers) == 0:
		return fmt.Errorf("interface %s has no peers", s.Name)
	}
	best := time.Duration(-1)
	var endpoints []string
	for _, p := range s.Peers {
		if p.LastHandshake >= 0 && (best < 0 || p.LastHandshake < best) {
			best = p.LastHandshake
		}
		if p.Endpoint != "" {
			endpoints = append(endpoints, p.Endpoint)
		}
	}
	sort.Strings(endpoints)
	ep := ""
	if len(endpoints) > 0 {
		ep = " with " + strings.Join(endpoints, ", ")
	}
	switch {
	case best < 0:
		return fmt.Errorf("interface %s: no handshake%s yet", s.Name, ep)
	case best > maxAge:
		return fmt.Errorf("interface %s: last handshake%s %s ago (limit %s)", s.Name, ep, best.Round(time.Second), maxAge)
	}
	return nil
}

func keyHash(c *Conf) string {
	h := sha256.New()
	h.Write([]byte(c.PrivateKey))
	for _, p := range c.Peers {
		h.Write([]byte("|" + p.PublicKey + ":" + p.PresharedKey))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// confHash fingerprints the non-secret desired settings (for `awg list`).
func confHash(w router.WGInterface) string {
	w.PrivateKey = ""
	w.Peers = append([]router.WGPeer(nil), w.Peers...) // do not clear the caller's keys
	for i := range w.Peers {
		w.Peers[i].PresharedKey = ""
	}
	b, _ := json.Marshal(w)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])[:16]
}

func (m *Manager) load() (map[string]Record, error) {
	if m.StatePath == "" {
		if m.mem == nil {
			m.mem = map[string]Record{}
		}
		out := make(map[string]Record, len(m.mem))
		for k, v := range m.mem {
			out[k] = v
		}
		return out, nil
	}
	recs := map[string]Record{}
	data, err := os.ReadFile(m.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return recs, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("%s: %w", m.StatePath, err)
	}
	return recs, nil
}

func (m *Manager) save(recs map[string]Record) error {
	if m.StatePath == "" {
		m.mem = recs
		return nil
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.StatePath), 0o750); err != nil {
		return err
	}
	tmp := m.StatePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.StatePath)
}
