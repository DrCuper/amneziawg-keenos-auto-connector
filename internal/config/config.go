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
		Log: Log{Level: "info", Format: "text"},
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
	return errors.Join(errs...)
}

// Redacted returns a copy safe for logging.
func (c Config) Redacted() Config {
	if c.Router.Password != "" {
		c.Router.Password = "***"
	}
	return c
}
