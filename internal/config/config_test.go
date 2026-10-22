package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRCI(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("router:\n  url: http://10.0.0.1\n  password: yamlpw\n  timeout: 3s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvRouterPassword, "")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Router.Transport != "rci" || cfg.Router.Username != "admin" || cfg.Router.Password != "yamlpw" || cfg.Router.Timeout != 3*time.Second || cfg.Router.Retries != 3 {
		t.Errorf("unexpected config %+v", cfg.Router)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	t.Setenv(EnvRouterPassword, "envpw")
	cfg := Default()
	if err := Parse([]byte("router:\n  url: http://r\n  password: yamlpw\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Router.Password != "envpw" {
		t.Errorf("password %q, want env value", cfg.Router.Password)
	}
}

func TestMissingPassword(t *testing.T) {
	t.Setenv(EnvRouterPassword, "")
	cfg := Default()
	err := Parse([]byte("router:\n  url: http://r\n"), &cfg)
	if err == nil || !strings.Contains(err.Error(), EnvRouterPassword) {
		t.Errorf("got %v", err)
	}
}

func TestUnknownTransport(t *testing.T) {
	cfg := Default()
	err := Parse([]byte("router:\n  transport: telnet\n"), &cfg)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("got %v", err)
	}
}

func TestSSHRequiresHostKeyPolicy(t *testing.T) {
	t.Setenv(EnvRouterPassword, "")
	t.Setenv(EnvRouterKeyFile, "/k")
	cfg := Default()
	err := Parse([]byte("router:\n  transport: ssh\n  host: r\n"), &cfg)
	if err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("got %v", err)
	}
	cfg = Default()
	if err := Parse([]byte("router:\n  transport: ssh\n  host: r\n  insecure_host_key: true\n"), &cfg); err != nil {
		t.Errorf("got %v", err)
	}
	if cfg.Router.KeyFile != "/k" {
		t.Errorf("key file from env not applied: %q", cfg.Router.KeyFile)
	}
}

func TestUnknownField(t *testing.T) {
	cfg := Default()
	if err := Parse([]byte("router:\n  url: http://r\n  password: x\n  bogus: 1\n"), &cfg); err == nil {
		t.Error("expected error for unknown field")
	}
}

func TestRedacted(t *testing.T) {
	c := Config{Router: Router{Password: "secret"}}
	if c.Redacted().Router.Password != "***" || c.Router.Password != "secret" {
		t.Error("redaction wrong")
	}
}

func TestSyncDefaultsAndValidation(t *testing.T) {
	t.Setenv(EnvRouterPassword, "pw")
	cfg := Default()
	if err := Parse([]byte("router:\n  url: http://r\nsync:\n  schedule: '@hourly'\n  on_limit: truncate\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.Schedule != "@hourly" || cfg.Sync.OnLimit != "truncate" || cfg.Sync.KeepVersions != 10 || cfg.Sync.MaxShrink != 0.5 || *cfg.Sync.RunOnStart != true || cfg.StateDir == "" {
		t.Errorf("unexpected sync config %+v", cfg.Sync)
	}
	for _, bad := range []string{"  max_shrink: 1.5\n", "  on_limit: drop\n", "  keep_versions: 0\n", "  source_timeout: 0s\n"} {
		cfg := Default()
		err := Parse([]byte("router:\n  url: http://r\nsync:\n"+bad), &cfg)
		if err == nil || !strings.Contains(err.Error(), "sync.") {
			t.Errorf("%q: want sync validation error, got %v", bad, err)
		}
	}
}

func TestNotifySecretsPrecedence(t *testing.T) {
	t.Setenv(EnvRouterPassword, "pw")
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "tg")
	_ = os.WriteFile(tokenFile, []byte("file-token\n"), 0o600)
	yaml := "router:\n  url: http://r\nnotify:\n  telegram:\n    bot_token: yaml-token\n    bot_token_file: " + tokenFile + "\n    chat_id: '42'\n"

	t.Setenv(EnvTelegramBotToken, "")
	cfg := Default()
	if err := Parse([]byte(yaml), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Telegram.BotToken != "file-token" {
		t.Errorf("file must win over YAML: %q", cfg.Notify.Telegram.BotToken)
	}
	t.Setenv(EnvTelegramBotToken, "env-token")
	cfg = Default()
	if err := Parse([]byte(yaml), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Telegram.BotToken != "env-token" || !cfg.Notify.Telegram.Enabled() {
		t.Errorf("env must win: %q", cfg.Notify.Telegram.BotToken)
	}
	if r := cfg.Redacted(); r.Notify.Telegram.BotToken != "***" || cfg.Notify.Telegram.BotToken != "env-token" {
		t.Error("token redaction wrong")
	}
}

func TestWebhookFromEnv(t *testing.T) {
	t.Setenv(EnvRouterPassword, "pw")
	t.Setenv(EnvWebhookURL, "https://hooks.example/abc")
	t.Setenv(EnvWebhookSecret, "s")
	cfg := Default()
	if err := Parse([]byte("router:\n  url: http://r\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notify.Webhooks) != 1 || cfg.Notify.Webhooks[0].URL != "https://hooks.example/abc" || cfg.Notify.Webhooks[0].Secret != "s" {
		t.Errorf("webhooks %+v", cfg.Notify.Webhooks)
	}
}

func TestStage3Defaults(t *testing.T) {
	t.Setenv(EnvRouterPassword, "pw")
	cfg := Default()
	if err := Parse([]byte("router:\n  url: http://r\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Journal.IsEnabled() || cfg.Journal.KeepDays != 90 || cfg.Notify.RepeatInterval != 6*time.Hour || cfg.Notify.FailThreshold != 1 ||
		cfg.Health.Interval != 5*time.Minute || !cfg.Health.RouterEnabled() || cfg.Notify.Telegram.Enabled() {
		t.Errorf("defaults: %+v %+v %+v", cfg.Journal, cfg.Notify, cfg.Health)
	}
}

func TestStage3Validation(t *testing.T) {
	t.Setenv(EnvRouterPassword, "pw")
	t.Setenv(EnvTelegramBotToken, "")
	t.Setenv(EnvTelegramChatID, "")
	t.Setenv(EnvWebhookURL, "")
	for _, c := range []struct{ yaml, want string }{
		{"notify:\n  telegram:\n    bot_token: x\n", "chat_id"},
		{"notify:\n  webhooks:\n    - url: ftp://secret-host/x\n", "webhooks[0].url"},
		{"notify:\n  fail_threshold: 0\n", "fail_threshold"},
		{"health:\n  checks:\n    - name: awg\n", "command is required"},
		{"health:\n  checks:\n    - {name: awg, command: [x]}\n    - {name: awg, command: [y]}\n", "used twice"},
		{"health:\n  checks:\n    - {name: 'a b', command: [x]}\n", "name"},
		{"health:\n  checks:\n    - {name: a, alert: AWG-Down, command: [x]}\n", "alert"},
	} {
		cfg := Default()
		err := Parse([]byte("router:\n  url: http://r\n"+c.yaml), &cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want %q, got %v", c.yaml, c.want, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret-host") {
			t.Errorf("webhook URL leaked in error: %v", err)
		}
	}
}

func TestAWGValidation(t *testing.T) {
	cfg := Default()
	yml := `
router: {url: http://r, password: p}
health:
  checks: [{name: main, command: [/bin/true]}]
awg:
  tunnels:
    - {name: main, conf_file: main.conf}
    - {name: x, conf: "[Interface]", conf_file: y}
`
	err := Parse([]byte(yml), &cfg)
	if err == nil || !strings.Contains(err.Error(), "awg_down:main") || !strings.Contains(err.Error(), "exactly one of conf_file") {
		t.Fatalf("err %v", err)
	}
	if r := cfg.Redacted(); r.AWG.Tunnels[1].Conf != "***" {
		t.Errorf("inline conf not redacted: %q", r.AWG.Tunnels[1].Conf)
	}
}
