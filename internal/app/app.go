// Package app wires configuration, transport and client together for the
// command-line entry point (and, later, for the service/HTTP layer).
package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/config"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/domains"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/health"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/rci"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/sshx"
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

// Close releases the transport.
func (a *App) Close() error { return a.Transport.Close() }

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
		Observers: observers,
		SpecPath:  a.DomainsFile(),
		Store:     st,
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

// HealthChecks returns the configured checks: the router first, then hooks.
func (a *App) HealthChecks() []health.Check {
	h := a.Config.Health
	var out []health.Check
	if h.RouterEnabled() {
		out = append(out, health.RouterCheck{Router: a.Client})
	}
	for _, c := range h.Checks {
		out = append(out, health.CommandCheck{Name: c.Name, Alert: c.Alert, Command: c.Command, Timeout: c.Timeout})
	}
	return out
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
