package app

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/api"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/config"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/remote"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
)

// amneziaRunner answers the adoption scripts of an Amnezia server.
type amneziaRunner struct{ cfg server.Config }

func (r *amneziaRunner) Run(_ context.Context, script string, _ []byte) (string, error) {
	switch {
	case strings.HasPrefix(script, "docker ps"):
		return "amnezia-awg\n", nil
	case strings.Contains(script, "ls /opt/amnezia/awg"):
		return "/opt/amnezia/awg/wg0.conf\n", nil
	case strings.Contains(script, "cat '\\''/opt/amnezia/awg/wg0.conf'\\''"):
		return "[Interface]\nPrivateKey = YUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=\nAddress = 10.8.1.0/24\nListenPort = 40000\nJc = 3\nH1 = 5\nH2 = 6\nH3 = 7\nH4 = 8\n", nil
	}
	return "", &remote.ExitError{Status: 1, Stderr: script}
}

func (r *amneziaRunner) Close() error { return nil }

func vpnDoc(doc map[string]any) []byte {
	raw, _ := json.Marshal(doc)
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(raw)))
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return []byte("vpn://" + base64.RawURLEncoding.EncodeToString(buf.Bytes()))
}

func TestImportAdminKey(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = dir
	cfg.Health.Router = new(bool)
	f := fake.New("")
	a := &App{Config: cfg, Transport: f, Logger: newLogger(config.Log{Level: "error"})}
	a.Client = router.NewClient(f, cli.Planner{}, router.Options{Timeout: time.Second, SaveConfig: true})
	var got server.Config
	a.NewRunner = func(c server.Config) (remote.Runner, error) { got = c; return &amneziaRunner{c}, nil }
	ctx := context.Background()

	key := vpnDoc(map[string]any{"hostName": "198.51.100.5", "userName": "ubuntu", "password": "s3cret", "port": 22})
	res, err := a.ImportKey(ctx, "cli", "am", key, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != api.KeyServer || !res.Adopted || res.Endpoint != "198.51.100.5:40000" {
		t.Fatalf("result %+v", res)
	}
	if !got.Amnezia || !got.Sudo || !got.TrustOnFirstUse || got.Password != "s3cret" || got.KnownHostsFile != filepath.Join(dir, "servers", "am.known_hosts") {
		t.Errorf("runner config %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "servers", "am.server.yaml"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("registration %v %v", info, err)
	}
	if n := a.ServerNames(); len(n) != 1 || n[0] != "am" {
		t.Errorf("servers %v", n)
	}
	if def, _ := a.DefaultServer(""); def != "am" {
		t.Errorf("default server %q", def)
	}
	if _, err := a.ImportKey(ctx, "cli", "am", key, false, false); err == nil {
		t.Error("second import without replace accepted")
	}
	if checks := a.HealthChecks(); len(checks) != 1 || checks[0].Signal().Key() != "server_down:am" {
		t.Errorf("checks %v", checks)
	}
	if r := a.Config.Redacted(); len(r.Servers) != 0 {
		t.Error("imported servers leaked into the config")
	}

	// A client key is only stored; the router stays untouched.
	res, err = a.ImportKey(ctx, "cli", "nl", []byte(testConf), false, false)
	if err != nil || res.Kind != api.KeyTunnel || res.Applied {
		t.Fatalf("client key %+v %v", res, err)
	}
	if cur, _ := a.Client.ReadState(ctx); len(cur.Interfaces) != 0 {
		t.Errorf("router changed: %+v", cur.Interfaces)
	}

	if err := a.ForgetServer("am"); err != nil {
		t.Fatal(err)
	}
	if len(a.ServerNames()) != 0 || a.Imported("am") {
		t.Error("server not forgotten")
	}
}
