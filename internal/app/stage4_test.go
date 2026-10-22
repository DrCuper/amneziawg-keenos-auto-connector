package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/config"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/health"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

const testConf = `[Interface]
PrivateKey = YUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=
Address = 10.8.1.2/32
Jc = 4
Jmin = 40
Jmax = 70
[Peer]
PublicKey = QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=
Endpoint = 203.0.113.1:51820
AllowedIPs = 0.0.0.0/0
`

// TestTunnelRoutes: a connection routed via `tunnel: main` fails until the
// tunnel is imported, then points at the tunnel's auto-picked interface.
func TestTunnelRoutes(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "domains.yaml"), []byte("connections:\n  awg:\n    routes: [{tunnel: main}]\ngroups:\n  - name: a\n    connection: awg\n    include: [a1.com]\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "main.conf"), []byte(testConf), 0o600)

	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.Sync.DomainsFile = filepath.Join(dir, "domains.yaml")
	cfg.Health.Router = new(bool)
	cfg.AWG.Tunnels = []awg.Tunnel{{Name: "main", ConfFile: "main.conf"}}
	f := fake.New("interface Wireguard0\n    up\n")
	a := &App{Config: cfg, ConfigDir: dir, Transport: f, Logger: newLogger(config.Log{Level: "error"})}
	a.Client = router.NewClient(f, cli.Planner{}, router.Options{Timeout: time.Second, SaveConfig: true})
	st, err := a.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := a.NewSyncer(st)

	if _, err := s.Run(ctx, syncer.Options{}); err == nil || !strings.Contains(err.Error(), "not imported yet") {
		t.Fatalf("sync before import: %v", err)
	}
	if _, err := a.Tunnels().Apply(ctx, nil, awg.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, syncer.Options{}); err != nil {
		t.Fatal(err)
	}
	cur, _ := a.Client.ReadState(ctx)
	if len(cur.Routes) != 1 || cur.Routes[0].Interface != "Wireguard1" {
		t.Errorf("routes %+v", cur.Routes)
	}

	checks := a.HealthChecks()
	if len(checks) != 1 || checks[0].Signal().Key() != "awg_down:main" {
		t.Fatalf("checks %+v", checks)
	}
	res := health.RunAll(ctx, checks)
	if res[0].Signal.Err == nil || !strings.Contains(res[0].Signal.Err.Error(), "no handshake") {
		t.Errorf("awg check: %v", res[0].Signal.Err)
	}
}
