package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/config"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/health"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/remote"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
)

type okRunner struct{ up bool }

func (r *okRunner) Run(_ context.Context, script string, _ []byte) (string, error) {
	switch {
	case strings.HasPrefix(script, "ip -4 route"):
		return "eth0\n", nil
	case strings.HasPrefix(script, "if awg show"):
		r.up = true
	case strings.HasPrefix(script, "awg show"):
		if !r.up {
			return "", &remote.ExitError{Status: 1}
		}
		return "PRIV\tPUB\t51820\toff\n", nil
	}
	return "", nil
}

func (r *okRunner) Close() error { return nil }

// TestServerOps: peer operations deploy, land in the journal and the
// server gets a server_down check.
func TestServerOps(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = dir
	cfg.Health.Router = new(bool)
	cfg.Servers = []server.Config{{Name: "vps", Host: "h", KeyFile: "id", InsecureHostKey: true, Endpoint: "1.2.3.4"}}
	f := fake.New("")
	a := &App{Config: cfg, ConfigDir: "/etc/awgkeen", Transport: f, Logger: newLogger(config.Log{Level: "error"})}
	runner := &okRunner{}
	var got server.Config
	a.NewRunner = func(c server.Config) (remote.Runner, error) { got = c; return runner, nil }
	ctx := context.Background()

	if n, err := a.DefaultServer(""); err != nil || n != "vps" {
		t.Fatalf("default server %q %v", n, err)
	}
	if _, err := a.Server("nope"); err == nil {
		t.Fatal("unknown server accepted")
	}
	if _, err := a.InitServer(ctx, "cli", "vps", false); err != nil {
		t.Fatal(err)
	}
	if got.KeyFile != "/etc/awgkeen/id" || got.Interface != "awg0" {
		t.Errorf("runner config %+v", got)
	}
	m, _ := a.Server("vps")
	if m.Store.Path != filepath.Join(dir, "servers", "vps.json") {
		t.Errorf("state path %s", m.Store.Path)
	}

	checks := a.HealthChecks()
	if len(checks) != 1 || checks[0].Signal().Key() != "server_down:vps" {
		t.Fatalf("checks %+v", checks)
	}
	if res := health.RunAll(ctx, checks); res[0].Signal.Err == nil {
		t.Error("check must fail before deploy")
	}

	p, plan, err := a.PeerOp(ctx, "cli", "vps", server.PeerAdd, "phone", true)
	if err != nil || p == nil || plan == nil || !plan.Applied {
		t.Fatalf("add: %+v %+v %v", p, plan, err)
	}
	if res := health.RunAll(ctx, checks); res[0].Signal.Err != nil {
		t.Errorf("check after deploy: %v", res[0].Signal.Err)
	}
	if _, _, err := a.PeerOp(ctx, "cli", "vps", server.PeerRemove, "ghost", false); err == nil {
		t.Error("removing unknown peer succeeded")
	}

	es, err := a.Journal().Query(journal.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range es {
		kinds = append(kinds, e.Kind+":"+e.Status)
		if e.Server != "vps" {
			t.Errorf("entry without server: %+v", e)
		}
	}
	if strings.Join(kinds, " ") != "peer-remove:failed server-deploy:ok peer-add:ok server-init:ok" {
		t.Errorf("journal %v", kinds)
	}
	if es[1].Changes[0].Kind != "server" || len(es[1].Changes) != 2 {
		t.Errorf("deploy changes %+v", es[1].Changes)
	}
}

// TestIssuedKeyImports: a client issued by a managed server parses as a
// tunnel for the router import (.conf and vpn:// key).
func TestIssuedKeyImports(t *testing.T) {
	ctx := context.Background()
	cfg := server.Config{Name: "vps", Host: "h", Password: "p", InsecureHostKey: true, Endpoint: "vpn.example.com"}.WithDefaults()
	m := &server.Manager{Config: cfg, Store: server.NewStore(t.TempDir(), "vps"), Runner: &okRunner{}}
	if _, err := m.Init(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddPeer(ctx, "keenetic"); err != nil {
		t.Fatal(err)
	}
	st, _ := m.Load()
	conf, _ := m.ClientConf("keenetic")
	key, _ := m.VPNKey("keenetic")
	for _, src := range []string{conf, key} {
		c, err := awg.Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if c.PrivateKey != st.Peers[0].Keys.Private || len(c.Addresses) != 1 || c.Addresses[0] != "10.8.1.2/32" || c.ASC["H1"] != st.Params.H1 || c.Peers[0].PresharedKey != st.Peers[0].PSK || len(c.Peers) != 1 ||
			c.Peers[0].PublicKey != st.Keys.Public || c.Peers[0].Endpoint != "vpn.example.com:51820" {
			t.Errorf("parsed %+v", c)
		}
	}
}
