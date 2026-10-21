package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/config"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
)

func TestLoadDesired(t *testing.T) {
	d, err := LoadDesired(filepath.Join("..", "..", "examples", "groups.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Groups) == 0 || d.Groups[0].Name == "" {
		t.Errorf("desired %+v", d)
	}
	if err := d.Normalize(router.Limits{MaxEntries: 2000}); err != nil {
		t.Errorf("example does not normalize: %v", err)
	}
}

func TestLoadDesiredUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.yaml")
	_ = os.WriteFile(p, []byte("groups:\n  - name: a\n    includes: [x.com]\n"), 0o600)
	if _, err := LoadDesired(p); err == nil {
		t.Error("expected error for typo'd field")
	}
}

func TestNewTransportFromConfig(t *testing.T) {
	if _, err := NewTransport(config.Router{Transport: "rci", URL: "http://r", Username: "admin", Password: "x"}); err != nil {
		t.Error(err)
	}
	if _, err := NewTransport(config.Router{Transport: "ssh", Host: "r", Username: "admin", Password: "x", InsecureHostKey: true}); err != nil {
		t.Error(err)
	}
	if _, err := NewTransport(config.Router{Transport: "nope"}); err == nil {
		t.Error("expected error")
	}
}

func TestFormatState(t *testing.T) {
	st := cli.Parse("object-group fqdn vpn\n    description x\n    include a.com\ndns-proxy\n    route object-group vpn Wireguard2 auto\n    route object-group ghost Wireguard0\n")
	out := FormatState(st)
	for _, want := range []string{"object-group fqdn vpn  (x)  include=1 exclude=0", "route -> Wireguard2 auto", "orphan route for unknown group ghost"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
