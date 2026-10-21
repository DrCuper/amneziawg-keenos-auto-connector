// Package app wires configuration, transport and client together for the
// command-line entry point (and, later, for the service/HTTP layer).
package app

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/config"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/rci"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/sshx"
)

// App holds the wired components.
type App struct {
	Config    config.Config
	Transport router.Transport
	Client    *router.Client
	Logger    *slog.Logger
}

// New loads the config file and builds the router client.
func New(cfgPath string) (*App, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	return FromConfig(cfg)
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
