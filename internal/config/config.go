// Package config loads the service's YAML configuration and applies secrets
// from the environment on top of it.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Environment variables that override secrets in the YAML file.
const (
	EnvRouterPassword = "AWGKEEN_ROUTER_PASSWORD"
	EnvRouterKeyFile  = "AWGKEEN_ROUTER_KEY_FILE"
)

// Config is the root of the YAML file.
type Config struct {
	Router Router `yaml:"router"`
	Log    Log    `yaml:"log"`
	// StateDir holds the service's own state: group versions, manual edits,
	// source cache.
	StateDir string `yaml:"state_dir"`
	Sync     Sync   `yaml:"sync"`
}

// Sync configures domain-group synchronization (stage 2).
type Sync struct {
	DomainsFile string `yaml:"domains_file"` // groups + connections + sources
	Schedule    string `yaml:"schedule"`     // cron: 5 fields, @hourly, @every 6h
	RunOnStart  *bool  `yaml:"run_on_start"` // serve: sync at startup, default true
	// MaxShrink blocks a group that shrank by more than this fraction versus
	// the applied version (0.5 = half); 0 disables the guard.
	MaxShrink float64 `yaml:"max_shrink"`
	// OnLimit is "error" or "truncate" when a group exceeds max_entries.
	OnLimit        string        `yaml:"on_limit"`
	KeepVersions   int           `yaml:"keep_versions"`
	SourceTimeout  time.Duration `yaml:"source_timeout"`
	SourceMaxBytes int64         `yaml:"source_max_bytes"`
	UserAgent      string        `yaml:"user_agent"`
}

// Log configures slog.
type Log struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // text|json
}

// Router configures the Keenetic client.
type Router struct {
	// Transport is "rci" or "ssh".
	Transport string `yaml:"transport"`

	// RCI settings.
	URL string `yaml:"url"` // e.g. http://192.168.1.1
	// SSH settings.
	Host           string `yaml:"host"` // host[:port], default port 22
	KeyFile        string `yaml:"key_file"`
	KnownHostsFile string `yaml:"known_hosts_file"`
	// InsecureHostKey disables host-key verification for SSH. Off by default.
	InsecureHostKey bool `yaml:"insecure_host_key"`

	Username string `yaml:"username"`
	Password string `yaml:"password"`

	Timeout     time.Duration `yaml:"timeout"`      // per transport operation
	Retries     int           `yaml:"retries"`      // transport error retries
	RetryDelay  time.Duration `yaml:"retry_delay"`  // first backoff step
	LockFile    string        `yaml:"lock_file"`    // empty = in-process only
	LockTimeout time.Duration `yaml:"lock_timeout"` // 0 = fail immediately
	BackupDir   string        `yaml:"backup_dir"`   // empty = no backups
	SaveConfig  *bool         `yaml:"save_config"`  // default true
	MaxEntries  int           `yaml:"max_entries"`  // per group, default 2000
}

// Default returns the configuration defaults.
func Default() Config {
	t := true
	return Config{
		Router: Router{
			Transport:   "rci",
			Username:    "admin",
			Timeout:     15 * time.Second,
			Retries:     3,
			RetryDelay:  500 * time.Millisecond,
			LockTimeout: 0,
			SaveConfig:  &t,
			MaxEntries:  2000,
		},
		Log:      Log{Level: "info", Format: "text"},
		StateDir: "/var/lib/awgkeen",
		Sync: Sync{
			DomainsFile:    "domains.yaml",
			Schedule:       "0 */6 * * *",
			RunOnStart:     &t,
			MaxShrink:      0.5,
			OnLimit:        "error",
			KeepVersions:   10,
			SourceTimeout:  30 * time.Second,
			SourceMaxBytes: 10 << 20,
			UserAgent:      "awgkeen (+https://github.com/DrCuper/amneziawg-keenos-auto-connector)",
		},
	}
}

// Load reads the YAML file, overlays environment secrets and validates.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := Parse(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Parse unmarshals YAML into cfg (which should start as Default()), applies
// environment overrides and validates.
func Parse(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, errEOF) && err.Error() != "EOF" {
		return fmt.Errorf("parse config: %w", err)
	}
	cfg.ApplyEnv(os.Getenv)
	return cfg.Validate()
}

var errEOF = errors.New("EOF")

// ApplyEnv overlays secrets from the environment. Environment wins over YAML.
func (c *Config) ApplyEnv(getenv func(string) string) {
	if v := getenv(EnvRouterPassword); v != "" {
		c.Router.Password = v
	}
	if v := getenv(EnvRouterKeyFile); v != "" {
		c.Router.KeyFile = v
	}
}

// Validate checks the configuration for completeness.
func (c *Config) Validate() error {
	r := &c.Router
	var errs []error
	switch r.Transport {
	case "rci":
		if r.URL == "" {
			errs = append(errs, errors.New("router.url is required for rci transport"))
		}
		if r.Password == "" {
			errs = append(errs, fmt.Errorf("router.password is required for rci transport (or set %s)", EnvRouterPassword))
		}
	case "ssh":
		if r.Host == "" {
			errs = append(errs, errors.New("router.host is required for ssh transport"))
		}
		if r.Password == "" && r.KeyFile == "" {
			errs = append(errs, fmt.Errorf("router.password or router.key_file is required for ssh transport (or set %s / %s)", EnvRouterPassword, EnvRouterKeyFile))
		}
		if r.KnownHostsFile == "" && !r.InsecureHostKey {
			errs = append(errs, errors.New("router.known_hosts_file is required for ssh transport unless insecure_host_key is true"))
		}
	default:
		errs = append(errs, fmt.Errorf("router.transport %q is not supported (rci, ssh)", r.Transport))
	}
	if r.Username == "" {
		errs = append(errs, errors.New("router.username is required"))
	}
	if r.Timeout <= 0 {
		errs = append(errs, errors.New("router.timeout must be positive"))
	}
	if r.Retries < 0 {
		errs = append(errs, errors.New("router.retries must be >= 0"))
	}
	s := &c.Sync
	if s.MaxShrink < 0 || s.MaxShrink >= 1 {
		errs = append(errs, errors.New("sync.max_shrink must be in [0, 1)"))
	}
	if s.OnLimit != "error" && s.OnLimit != "truncate" {
		errs = append(errs, fmt.Errorf("sync.on_limit %q is not supported (error, truncate)", s.OnLimit))
	}
	if s.KeepVersions < 1 {
		errs = append(errs, errors.New("sync.keep_versions must be >= 1"))
	}
	if s.SourceTimeout <= 0 {
		errs = append(errs, errors.New("sync.source_timeout must be positive"))
	}
	if s.SourceMaxBytes <= 0 {
		errs = append(errs, errors.New("sync.source_max_bytes must be positive"))
	}
	return errors.Join(errs...)
}

// Redacted returns a copy safe for logging.
func (c Config) Redacted() Config {
	if c.Router.Password != "" {
		c.Router.Password = "***"
	}
	return c
}
