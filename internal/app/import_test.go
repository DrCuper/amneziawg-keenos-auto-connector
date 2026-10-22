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
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// TestImportKeys: keys of two foreign servers become two tunnels, groups
// route to them by name, and a forgotten key leaves the router.
func TestImportKeys(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "domains.yaml"), []byte("connections:\n  nl:\n    routes: [{tunnel: nl}, {tunnel: de}]\ngroups:\n  - name: a\n    connection: nl\n    include: [a1.com]\n"), 0o600)
	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.Sync.DomainsFile = filepath.Join(dir, "domains.yaml")
	cfg.Health.Router = new(bool)
	cfg.AWG.Tunnels = []awg.Tunnel{{Name: "main", ConfFile: "main.conf"}}
	_ = os.WriteFile(filepath.Join(dir, "main.conf"), []byte(testConf), 0o600)
	f := fake.New("interface Wireguard0\n    up\n")
	newApp := func() *App {
		a := &App{Config: cfg, ConfigDir: dir, Transport: f, Logger: newLogger(config.Log{Level: "error"})}
		a.Client = router.NewClient(f, cli.Planner{}, router.Options{Timeout: time.Second, SaveConfig: true})
		return a
	}
	a := newApp()
	ctx := context.Background()

	// A vpn:// key as issued by a server (AmneziaVPN uses the same format).
	m := &server.Manager{Config: server.Config{Name: "de", Host: "h", Password: "p", InsecureHostKey: true, Endpoint: "de.example.com"}.WithDefaults(),
		Store: server.NewStore(t.TempDir(), "de"), Runner: &okRunner{}}
	_, _ = m.Init(ctx, false)
	_, _ = m.AddPeer(ctx, "router")
	vpn, _ := m.VPNKey("router")

	if _, err := a.ImportTunnel(ctx, "cli", "main", []byte(testConf), true, false); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Fatalf("config name: %v", err)
	}
	if _, err := a.ImportTunnel(ctx, "cli", "bad", []byte("garbage"), true, false); err == nil {
		t.Fatal("garbage key accepted")
	}
	if _, err := a.ImportTunnel(ctx, "cli", "nl", []byte(testConf), true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ImportTunnel(ctx, "cli", "de", []byte(vpn), true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ImportTunnel(ctx, "cli", "de", []byte(vpn), true, false); err == nil {
		t.Error("duplicate import without replace accepted")
	}
	if _, err := a.ImportTunnel(ctx, "cli", "de", []byte(vpn), true, true); err != nil {
		t.Errorf("replace: %v", err)
	}
	info, err := os.Stat(filepath.Join(cfg.StateDir, "tunnels", "de.conf"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v", info, err)
	}

	// A new process sees the imported keys; groups route to both tunnels.
	a = newApp()
	names := []string{}
	for _, tn := range a.Tunnels().List() {
		names = append(names, tn.Name)
	}
	if strings.Join(names, ",") != "main,de,nl" {
		t.Fatalf("tunnels %v", names)
	}
	st, _ := a.OpenStore()
	if _, err := a.NewSyncer(st).Run(ctx, syncer.Options{}); err != nil {
		t.Fatal(err)
	}
	cur, _ := a.Client.ReadState(ctx)
	var ifaces []string
	for _, r := range cur.Routes {
		ifaces = append(ifaces, r.Interface)
	}
	if strings.Join(ifaces, ",") != "Wireguard1,Wireguard2" {
		t.Errorf("routes %v", ifaces)
	}
	if n := len(a.HealthChecks()); n != 3 {
		t.Errorf("checks %d, want main, de and nl", n)
	}

	if _, err := a.ForgetTunnel(ctx, "cli", "main"); err == nil {
		t.Error("forgetting a config tunnel accepted")
	}
	if _, err := a.ForgetTunnel(ctx, "cli", "de"); err != nil {
		t.Fatal(err)
	}
	cur, _ = a.Client.ReadState(ctx)
	if _, ok := cur.Interface("Wireguard2"); ok || a.Keys().Has("de") {
		t.Error("de still on the router or in the key store")
	}
	if _, ok := a.Tunnels().Tunnel("de"); ok {
		t.Error("de still listed")
	}
}
