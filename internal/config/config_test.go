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
