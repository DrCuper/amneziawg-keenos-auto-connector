// Package app wires configuration, transport and client together for the
// command-line entry point (and, later, for the service/HTTP layer).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/api"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/config"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/domains"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/health"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/remote"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/rci"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/sshx"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/source"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// App holds the wired components.
type App struct {
	Config config.Config
	// ConfigDir is the directory of the loaded config file ("" when built
	// from an in-memory config); relative paths in the config resolve to it.
	ConfigDir string
	Transport router.Transport
	Client    *router.Client
	Logger    *slog.Logger

	journal *journal.Journal
	alerter *notify.Alerter
	tunnels *awg.Manager
	servers map[string]*server.Manager
	smu     sync.Mutex
	// NewRunner overrides how server runners are built (tests).
	NewRunner func(server.Config) (remote.Runner, error)
}

// New loads the config file and builds the router client.
func New(cfgPath string) (*App, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	a, err := FromConfig(cfg)
	if err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(cfgPath); err == nil {
		a.ConfigDir = filepath.Dir(abs)
	}
	return a, nil
}

// FromConfig builds the app from an already loaded configuration.
func FromConfig(cfg config.Config) (*App, error) {
	logger := newLogger(cfg.Log)
	t, err := NewTransport(cfg.Router)
	if err != nil {
		return nil, err
	}
	a := &App{Config: cfg, Transport: t, Logger: logger}
	a.Client = router.NewClient(t, cli.Planner{}, a.options(true))
	return a, nil
}

// WithoutSave returns a client that does not save the configuration.
func (a *App) WithoutSave() *router.Client {
	return router.NewClient(a.Transport, cli.Planner{}, a.options(false))
}

func (a *App) options(save bool) router.Options {
	r := a.Config.Router
	if r.SaveConfig != nil && !*r.SaveConfig {
		save = false
	}
	return router.Options{
		Timeout:     r.Timeout,
		Retries:     r.Retries,
		RetryDelay:  r.RetryDelay,
		LockFile:    r.LockFile,
		LockTimeout: r.LockTimeout,
		BackupDir:   r.BackupDir,
		SaveConfig:  save,
		Limits:      router.Limits{MaxEntries: r.MaxEntries},
		Logger:      a.Logger,
	}
}

// Close releases the transport and server connections.
func (a *App) Close() error {
	a.smu.Lock()
	defer a.smu.Unlock()
	for _, m := range a.servers {
		_ = m.Runner.Close()
	}
	return a.Transport.Close()
}

// OpenStore opens the file store under state_dir.
func (a *App) OpenStore() (*store.File, error) {
	return store.NewFile(a.Config.StateDir)
}

// NewSyncer wires store, fetcher and router client for `sync`, `serve` and
// `groups`, with the journal and the alerter as observers. The domains file
// path is resolved relative to the config file's directory when not absolute.
func (a *App) NewSyncer(st store.Store) *syncer.Syncer {
	sc := a.Config.Sync
	var observers []syncer.Observer
	if j := a.Journal(); j != nil {
		observers = append(observers, &journal.Observer{J: j, MaxChanges: a.Config.Journal.MaxChanges})
	}
	observers = append(observers, a.Alerter())
	return &syncer.Syncer{
		ResolveTunnel: a.Tunnels().Resolve,
		Observers:     observers,
		SpecPath:      a.DomainsFile(),
		Store:         st,
		Fetcher: &source.Fetcher{
			HTTP:      &http.Client{Timeout: sc.SourceTimeout},
			Cache:     st,
			MaxBytes:  sc.SourceMaxBytes,
			UserAgent: sc.UserAgent,
		},
		Router:       a.Client,
		Limits:       domains.Limits{MaxEntries: a.Config.Router.MaxEntries, OnLimit: domains.OnLimit(sc.OnLimit)},
		MaxShrink:    sc.MaxShrink,
		KeepVersions: sc.KeepVersions,
		Logger:       a.Logger,
	}
}

// Journal opens the change journal once; nil when it is disabled or cannot
// be opened (a warning is logged: the journal must not block syncs).
func (a *App) Journal() *journal.Journal {
	jc := a.Config.Journal
	if a.journal != nil || !jc.IsEnabled() {
		return a.journal
	}
	path := jc.File
	if path == "" {
		path = filepath.Join(a.Config.StateDir, "journal.jsonl")
	}
	j, err := journal.Open(path)
	if err != nil {
		a.Logger.Warn("journal disabled", "err", err)
		return nil
	}
	j.KeepDays, j.MaxEntries, j.Logger = jc.KeepDays, jc.MaxEntries, a.Logger
	a.journal = j
	return j
}

// Record appends an entry built outside the syncer (`router apply`).
func (a *App) Record(e *journal.Entry) {
	if j := a.Journal(); j != nil {
		if _, err := j.Append(e); err != nil {
			a.Logger.Warn("journal: cannot record operation", "kind", e.Kind, "err", err)
		}
	}
}

// Alerter builds the alerter with the configured channels once. Its state is
// <state_dir>/alerts.json, shared with other processes.
func (a *App) Alerter() *notify.Alerter {
	if a.alerter != nil {
		return a.alerter
	}
	n := a.Config.Notify
	ho := notify.HTTPOptions{Client: &http.Client{}, Timeout: n.Timeout, Retries: n.Retries}
	var chans []notify.Channel
	if n.Telegram.Enabled() {
		chans = append(chans, &notify.Telegram{Token: n.Telegram.BotToken, ChatID: n.Telegram.ChatID, APIURL: n.Telegram.APIURL, HTTPOptions: ho})
	}
	for _, w := range n.Webhooks {
		chans = append(chans, &notify.Webhook{Label: w.Name, URL: w.URL, Secret: w.Secret, Headers: w.Headers, HTTPOptions: ho})
	}
	host := n.Host
	if host == "" {
		host, _ = os.Hostname()
	}
	a.alerter = &notify.Alerter{
		Channels:       chans,
		StatePath:      filepath.Join(a.Config.StateDir, "alerts.json"),
		FailThreshold:  n.FailThreshold,
		RepeatInterval: n.RepeatInterval,
		OnApply:        n.OnApply,
		Host:           host,
		Logger:         a.Logger,
	}
	return a.alerter
}

// Tunnels returns the AWG tunnel manager (state in <state_dir>/awg.json):
// the tunnels of the config file plus the keys imported with `awg import`.
func (a *App) Tunnels() *awg.Manager {
	if a.tunnels == nil {
		a.tunnels = &awg.Manager{
			Router:    a.Client,
			Tunnels:   append([]awg.Tunnel(nil), a.Config.AWG.Tunnels...),
			BaseDir:   a.ConfigDir,
			StatePath: filepath.Join(a.Config.StateDir, "awg.json"),
			Logger:    a.Logger,
		}
		imported, err := a.Keys().Tunnels()
		if err != nil {
			a.Logger.Warn("awg: cannot read imported keys", "err", err)
		}
		for _, t := range imported {
			if err := a.tunnels.Put(t); err != nil {
				a.Logger.Warn("awg: imported key ignored", "tunnel", t.Name, "err", err)
			}
		}
	}
	return a.tunnels
}

// Keys is the store of imported client keys (<state_dir>/tunnels).
func (a *App) Keys() awg.KeyStore {
	return awg.KeyStore{Dir: filepath.Join(a.Config.StateDir, "tunnels")}
}

// ServerNames lists the configured servers in config order, then the ones
// imported from Amnezia admin keys.
func (a *App) ServerNames() []string {
	out := make([]string, 0, len(a.Config.Servers))
	seen := map[string]bool{}
	for _, s := range a.Config.Servers {
		out = append(out, s.Name)
		seen[s.Name] = true
	}
	for _, s := range a.importedServers() {
		if !seen[s.Name] {
			out = append(out, s.Name)
		}
	}
	return out
}

// ServerConfig finds a server in the config file or among imported ones.
func (a *App) ServerConfig(name string) (server.Config, bool) {
	if c, ok := a.Config.Server(name); ok {
		c.KeyFile = a.resolvePath(c.KeyFile)
		c.KnownHostsFile = a.resolvePath(c.KnownHostsFile)
		return c, true
	}
	for _, c := range a.importedServers() {
		if c.Name == name {
			return c.WithDefaults(), true
		}
	}
	return server.Config{}, false
}

// Server returns the manager of a server, built once. Its state is
// <state_dir>/servers/<name>.json; relative key and known_hosts paths
// resolve against the config file's directory.
func (a *App) Server(name string) (*server.Manager, error) {
	a.smu.Lock()
	defer a.smu.Unlock()
	if m, ok := a.servers[name]; ok {
		return m, nil
	}
	cfg, ok := a.ServerConfig(name)
	if !ok {
		return nil, fmt.Errorf("unknown server %q (known: %s)", name, strings.Join(a.ServerNames(), ", "))
	}
	newRunner := a.NewRunner
	if newRunner == nil {
		newRunner = NewRunner
	}
	r, err := newRunner(cfg)
	if err != nil {
		return nil, fmt.Errorf("server %s: %w", name, err)
	}
	m := &server.Manager{
		Config: cfg,
		Store:  server.NewStore(a.serversDir(), name),
		Runner: r,
		Logger: a.Logger,
	}
	if a.servers == nil {
		a.servers = map[string]*server.Manager{}
	}
	a.servers[name] = m
	return m, nil
}

// dropServer forgets a built manager (after its registration changed).
func (a *App) dropServer(name string) {
	a.smu.Lock()
	defer a.smu.Unlock()
	if m, ok := a.servers[name]; ok {
		_ = m.Runner.Close()
		delete(a.servers, name)
	}
}

// DefaultServer resolves an empty name to the only known server.
func (a *App) DefaultServer(name string) (string, error) {
	if name != "" {
		return name, nil
	}
	names := a.ServerNames()
	switch len(names) {
	case 0:
		return "", errors.New("no servers (add a `servers:` entry to the config or import an Amnezia admin key with `awgkeen awg import`)")
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("several servers, pick one with --server (%s)", strings.Join(names, ", "))
	}
}

// NewRunner builds the local or SSH runner of a server.
func NewRunner(c server.Config) (remote.Runner, error) {
	if c.Local {
		return remote.Local{Sudo: c.Sudo}, nil
	}
	return remote.NewSSH(remote.SSHConfig{
		Host: c.Host, User: c.User, Password: c.Password, KeyFile: c.KeyFile,
		KnownHostsFile: c.KnownHostsFile, InsecureHostKey: c.InsecureHostKey, TrustOnFirstUse: c.TrustOnFirstUse, Sudo: c.Sudo,
	})
}

func (a *App) resolvePath(p string) string {
	if p != "" && !filepath.IsAbs(p) && a.ConfigDir != "" {
		return filepath.Join(a.ConfigDir, p)
	}
	return p
}

// failingChecker reports a server whose runner cannot be built.
type failingChecker struct{ err error }

func (f failingChecker) Check(context.Context) error { return f.err }

// HealthChecks returns the configured checks: the router first, then AWG
// tunnels, then managed servers, then hooks.
func (a *App) HealthChecks() []health.Check {
	h := a.Config.Health
	var out []health.Check
	if h.RouterEnabled() {
		out = append(out, health.RouterCheck{Router: a.Client})
	}
	for _, t := range a.Tunnels().List() {
		if t.Check.IsEnabled() {
			out = append(out, health.AWGCheck{Tunnel: t, Tunnels: a.Tunnels()})
		}
	}
	for _, name := range a.ServerNames() {
		sc, _ := a.ServerConfig(name)
		if !sc.CheckEnabled() {
			continue
		}
		var chk health.ServerChecker
		if m, err := a.Server(sc.Name); err != nil {
			chk = failingChecker{err}
		} else {
			chk = m
		}
		out = append(out, health.ServerCheck{Name: sc.Name, Server: chk})
	}
	for _, c := range h.Checks {
		out = append(out, health.CommandCheck{Name: c.Name, Alert: c.Alert, Command: c.Command, Timeout: c.Timeout})
	}
	return out
}

// NewAPI builds the HTTP API over the same store, syncer and lock the
// daemon uses. checks returns the latest health results.
func (a *App) NewAPI(st store.Store, s *syncer.Syncer, ops *sync.Mutex, checks func() []health.Result, version string) *api.Server {
	apiSyncer := *s
	apiSyncer.DefaultTrigger = syncer.TriggerAPI
	srv := &api.Server{
		Syncer:     &apiSyncer,
		Store:      st,
		SpecPath:   a.DomainsFile(),
		Journal:    a.Journal(),
		Alerter:    a.Alerter(),
		Router:     a.Client,
		Checks:     checks,
		Record:     a.Record,
		MaxChanges: a.Config.Journal.MaxChanges,
		Token:      a.Config.API.Token,
		ReadToken:  a.Config.API.ReadToken,
		UI:         a.Config.API.UIEnabled(),
		Ops:        ops,
		Version:    version,
		Logger:     a.Logger,
	}
	srv.Tunnels = a.Tunnels()
	srv.ImportKey, srv.ForgetKey = a.ImportKey, a.ForgetTunnel
	srv.Servers = a
	return srv
}

// DomainsFile returns the absolute path of sync.domains_file.
func (a *App) DomainsFile() string {
	p := a.Config.Sync.DomainsFile
	if !filepath.IsAbs(p) && a.ConfigDir != "" {
		p = filepath.Join(a.ConfigDir, p)
	}
	return p
}

// NewTransport picks the transport from the configuration.
func NewTransport(r config.Router) (router.Transport, error) {
	switch r.Transport {
	case "rci":
		return rci.New(rci.Config{URL: r.URL, Username: r.Username, Password: r.Password})
	case "ssh":
		return sshx.New(sshx.Config{
			Host: r.Host, Username: r.Username, Password: r.Password, KeyFile: r.KeyFile,
			KnownHostsFile: r.KnownHostsFile, InsecureHostKey: r.InsecureHostKey, DialTimeout: r.Timeout,
		})
	default:
		return nil, fmt.Errorf("unsupported transport %q", r.Transport)
	}
}

// LoadDesired reads a desired-state YAML file (see examples/groups.yaml).
func LoadDesired(path string) (*router.Desired, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("read desired state: %w", err)
	}
	var d router.Desired
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &d, nil
}

// FormatState renders the state for `router show`.
func FormatState(st *router.State) string {
	var b strings.Builder
	if len(st.Groups) == 0 {
		b.WriteString("no object-group fqdn on the router\n")
	}
	routes := map[string][]router.DNSRoute{}
	for _, r := range st.Routes {
		routes[r.Group] = append(routes[r.Group], r)
	}
	for _, g := range st.Groups {
		fmt.Fprintf(&b, "object-group fqdn %s", g.Name)
		if g.Description != "" {
			fmt.Fprintf(&b, "  (%s)", g.Description)
		}
		fmt.Fprintf(&b, "  include=%d exclude=%d\n", len(g.Include), len(g.Exclude))
		for _, r := range routes[g.Name] {
			fmt.Fprintf(&b, "    route -> %s", strings.TrimPrefix(cli.AddRoute(r), "dns-proxy route object-group "+g.Name+" "))
			b.WriteString("\n")
		}
		delete(routes, g.Name)
	}
	for g, rs := range routes {
		for _, r := range rs {
			fmt.Fprintf(&b, "orphan route for unknown group %s -> %s\n", g, r.Destination())
		}
	}
	return b.String()
}

func newLogger(l config.Log) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(l.Level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if l.Format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
