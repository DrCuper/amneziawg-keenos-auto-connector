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
