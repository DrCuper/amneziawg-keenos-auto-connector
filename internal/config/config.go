// Package config loads the service's YAML configuration and applies secrets
// from the environment on top of it.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
)

// Environment variables that override secrets in the YAML file.
const (
	EnvRouterPassword = "AWGKEEN_ROUTER_PASSWORD"
	EnvRouterKeyFile  = "AWGKEEN_ROUTER_KEY_FILE"

	EnvTelegramBotToken = "AWGKEEN_TELEGRAM_BOT_TOKEN"
	EnvTelegramChatID   = "AWGKEEN_TELEGRAM_CHAT_ID"
	// EnvWebhookURL / EnvWebhookSecret apply to the first webhook (one is
	// created when notify.webhooks is empty and the URL is set).
	EnvWebhookURL    = "AWGKEEN_WEBHOOK_URL"
	EnvWebhookSecret = "AWGKEEN_WEBHOOK_SECRET"
)

// Config is the root of the YAML file.
type Config struct {
	Router Router `yaml:"router"`
	Log    Log    `yaml:"log"`
	// StateDir holds the service's own state: group versions, manual edits,
	// source cache.
	StateDir string  `yaml:"state_dir"`
	Sync     Sync    `yaml:"sync"`
	Journal  Journal `yaml:"journal"`
	Notify   Notify  `yaml:"notify"`
	Health   Health  `yaml:"health"`
	AWG      AWG     `yaml:"awg"`
}

// AWG lists AmneziaWG tunnels to import onto the router (stage 4).
type AWG struct {
	Tunnels []awg.Tunnel `yaml:"tunnels"`
	// ApplyOnStart: `serve` imports the tunnels before the first sync
	// (default true).
	ApplyOnStart *bool `yaml:"apply_on_start"`
}

// ApplyAtStart reports whether serve imports tunnels at startup.
func (a AWG) ApplyAtStart() bool { return a.ApplyOnStart == nil || *a.ApplyOnStart }

// Journal configures the change journal (stage 3).
type Journal struct {
	Enabled *bool  `yaml:"enabled"` // default true
	File    string `yaml:"file"`    // default <state_dir>/journal.jsonl
	// KeepDays and MaxEntries bound the journal on prune (0 = unlimited).
	KeepDays   int `yaml:"keep_days"`
	MaxEntries int `yaml:"max_entries"`
	// MaxChanges caps diff lines stored per entry (0 = all).
	MaxChanges int `yaml:"max_changes"`
}

// IsEnabled reports whether the journal is on.
func (j Journal) IsEnabled() bool { return j.Enabled == nil || *j.Enabled }

// Notify configures alert delivery (stage 3).
type Notify struct {
	Telegram Telegram  `yaml:"telegram"`
	Webhooks []Webhook `yaml:"webhooks"`
	// RepeatInterval between reminders of an active alert; 0 = never.
	RepeatInterval time.Duration `yaml:"repeat_interval"`
	// FailThreshold is how many consecutive failures fire an alert.
	FailThreshold int  `yaml:"fail_threshold"`
	OnApply       bool `yaml:"on_apply"` // info message after a sync changed the router
	// Timeout and Retries apply to every delivery attempt.
	Timeout time.Duration `yaml:"timeout"`
	Retries int           `yaml:"retries"`
	// Host labels messages; default is the machine's hostname.
	Host string `yaml:"host"`
}

// Telegram is the Bot API channel; enabled when token and chat_id are set.
type Telegram struct {
	BotToken     string `yaml:"bot_token"`
	BotTokenFile string `yaml:"bot_token_file"`
	ChatID       string `yaml:"chat_id"`
	APIURL       string `yaml:"api_url"`
}

// Enabled reports whether the channel is configured.
func (t Telegram) Enabled() bool { return t.BotToken != "" && t.ChatID != "" }

// Webhook is a generic JSON POST channel.
type Webhook struct {
	Name       string            `yaml:"name"`
	URL        string            `yaml:"url"`
	URLFile    string            `yaml:"url_file"`
	Secret     string            `yaml:"secret"`
	SecretFile string            `yaml:"secret_file"`
	Headers    map[string]string `yaml:"headers"`
}

// Health configures periodic checks in `serve`.
type Health struct {
	Interval time.Duration `yaml:"interval"` // 0 disables
	Router   *bool         `yaml:"router"`   // check router reachability, default true
	Checks   []Check       `yaml:"checks"`
}

// RouterEnabled reports whether the router check is on.
func (h Health) RouterEnabled() bool { return h.Router == nil || *h.Router }

// Check is an external hook command.
type Check struct {
	Name    string        `yaml:"name"`
	Alert   string        `yaml:"alert"` // default awg_down
	Command []string      `yaml:"command"`
	Timeout time.Duration `yaml:"timeout"`
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
		Journal: Journal{KeepDays: 90, MaxEntries: 5000, MaxChanges: 500},
		Notify: Notify{
			RepeatInterval: 6 * time.Hour,
			FailThreshold:  1,
			Timeout:        10 * time.Second,
			Retries:        2,
		},
		Health: Health{Interval: 5 * time.Minute},
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
	if err := cfg.ResolveFiles(os.ReadFile); err != nil {
		return err
	}
	cfg.ApplyEnv(os.Getenv)
	return cfg.Validate()
}

// ResolveFiles reads secrets referenced by *_file options. Values from files
// replace YAML values; environment variables (ApplyEnv) win over both.
func (c *Config) ResolveFiles(read func(string) ([]byte, error)) error {
	load := func(field, path string, dst *string) error {
		if path == "" {
			return nil
		}
		data, err := read(path)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		*dst = strings.TrimSpace(string(data))
		return nil
	}
	n := &c.Notify
	errs := []error{load("notify.telegram.bot_token_file", n.Telegram.BotTokenFile, &n.Telegram.BotToken)}
	for i := range n.Webhooks {
		w := &n.Webhooks[i]
		errs = append(errs,
			load(fmt.Sprintf("notify.webhooks[%d].url_file", i), w.URLFile, &w.URL),
			load(fmt.Sprintf("notify.webhooks[%d].secret_file", i), w.SecretFile, &w.Secret))
	}
	return errors.Join(errs...)
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
	n := &c.Notify
	if v := getenv(EnvTelegramBotToken); v != "" {
		n.Telegram.BotToken = v
	}
	if v := getenv(EnvTelegramChatID); v != "" {
		n.Telegram.ChatID = v
	}
	if v := getenv(EnvWebhookURL); v != "" {
		if len(n.Webhooks) == 0 {
			n.Webhooks = []Webhook{{}}
		}
		n.Webhooks[0].URL = v
	}
	if v := getenv(EnvWebhookSecret); v != "" && len(n.Webhooks) > 0 {
		n.Webhooks[0].Secret = v
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
	errs = append(errs, c.validateStage3()...)
	if err := awg.ValidateTunnels(c.AWG.Tunnels); err != nil {
		errs = append(errs, err)
	}
	for _, t := range c.AWG.Tunnels {
		for _, ch := range c.Health.Checks {
			if ch.Name == t.Name && (ch.Alert == "" || ch.Alert == "awg_down") {
				errs = append(errs, fmt.Errorf("health.checks %q and awg tunnel %q would share the alert awg_down:%s; rename one", ch.Name, t.Name, t.Name))
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Config) validateStage3() []error {
	var errs []error
	j := &c.Journal
	if j.KeepDays < 0 || j.MaxEntries < 0 || j.MaxChanges < 0 {
		errs = append(errs, errors.New("journal.keep_days, max_entries and max_changes must be >= 0"))
	}
	n := &c.Notify
	if (n.Telegram.BotToken == "") != (n.Telegram.ChatID == "") {
		errs = append(errs, fmt.Errorf("notify.telegram needs both bot_token and chat_id (or %s / %s)", EnvTelegramBotToken, EnvTelegramChatID))
	}
	for i, w := range n.Webhooks {
		u, err := url.Parse(w.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			// Do not echo the URL: it may carry a token.
			errs = append(errs, fmt.Errorf("notify.webhooks[%d].url must be an http(s) URL", i))
		}
	}
	if n.RepeatInterval < 0 {
		errs = append(errs, errors.New("notify.repeat_interval must be >= 0"))
	}
	if n.FailThreshold < 1 {
		errs = append(errs, errors.New("notify.fail_threshold must be >= 1"))
	}
	if n.Timeout <= 0 {
		errs = append(errs, errors.New("notify.timeout must be positive"))
	}
	if n.Retries < 0 {
		errs = append(errs, errors.New("notify.retries must be >= 0"))
	}
	h := &c.Health
	if h.Interval < 0 {
		errs = append(errs, errors.New("health.interval must be >= 0"))
	}
	seen := map[string]bool{}
	for i, ch := range h.Checks {
		switch {
		case !checkName.MatchString(ch.Name):
			errs = append(errs, fmt.Errorf("health.checks[%d].name %q must match %s", i, ch.Name, checkName))
		case seen[ch.Name]:
			errs = append(errs, fmt.Errorf("health.checks[%d].name %q is used twice", i, ch.Name))
		}
		seen[ch.Name] = true
		if len(ch.Command) == 0 || ch.Command[0] == "" {
			errs = append(errs, fmt.Errorf("health.checks[%d].command is required", i))
		}
		if ch.Alert != "" && !alertName.MatchString(ch.Alert) {
			errs = append(errs, fmt.Errorf("health.checks[%d].alert %q must match %s", i, ch.Alert, alertName))
		}
		if ch.Timeout < 0 {
			errs = append(errs, fmt.Errorf("health.checks[%d].timeout must be >= 0", i))
		}
	}
	return errs
}

var (
	checkName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	alertName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// Redacted returns a copy safe for logging.
func (c Config) Redacted() Config {
	if c.Router.Password != "" {
		c.Router.Password = "***"
	}
	if c.Notify.Telegram.BotToken != "" {
		c.Notify.Telegram.BotToken = "***"
	}
	if len(c.Notify.Webhooks) > 0 {
		ws := append([]Webhook(nil), c.Notify.Webhooks...)
		for i := range ws {
			if ws[i].URL != "" {
				ws[i].URL = "***"
			}
			if ws[i].Secret != "" {
				ws[i].Secret = "***"
			}
		}
		c.Notify.Webhooks = ws
	}
	if len(c.AWG.Tunnels) > 0 {
		ts := append([]awg.Tunnel(nil), c.AWG.Tunnels...)
		for i := range ts {
			if ts[i].Conf != "" {
				ts[i].Conf = "***"
			}
		}
		c.AWG.Tunnels = ts
	}
	return c
}
