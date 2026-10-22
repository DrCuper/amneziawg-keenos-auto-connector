package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/remote"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
)

// hostRunner accepts every deploy script; the interface is never up.
type hostRunner struct{}

func (hostRunner) Run(_ context.Context, script string, _ []byte) (string, error) {
	switch {
	case strings.HasPrefix(script, "ip -4 route"):
		return "eth0\n", nil
	case strings.HasPrefix(script, "awg show"):
		return "", &remote.ExitError{Status: 1}
	}
	return "", nil
}

func (hostRunner) Close() error { return nil }

// testServers implements ServerOps over one manager without a journal.
type testServers struct{ m *server.Manager }

func (s testServers) ServerNames() []string { return []string{"vps"} }

func (s testServers) Server(name string) (*server.Manager, error) {
	if name != "vps" {
		return nil, fmt.Errorf("unknown server %q", name)
	}
	return s.m, nil
}

func (s testServers) PeerOp(ctx context.Context, _, _, op, peer string, deploy bool) (*server.Peer, *server.DeployPlan, error) {
	var p *server.Peer
	var err error
	switch op {
	case server.PeerAdd:
		p, err = s.m.AddPeer(ctx, peer)
	case server.PeerRemove:
		err = s.m.RemovePeer(ctx, peer)
	case server.PeerRotate:
		p, err = s.m.RotatePeer(ctx, peer)
	default:
		err = s.m.SetDisabled(ctx, peer, op == server.PeerDisable)
	}
	if err != nil || !deploy {
		return p, nil, err
	}
	plan, err := s.m.Deploy(ctx, false)
	return p, plan, err
}

func (s testServers) DeployServer(ctx context.Context, _, _ string, dryRun bool) (*server.DeployPlan, error) {
	return s.m.Deploy(ctx, dryRun)
}

func TestServerEndpoints(t *testing.T) {
	e := newEnv(t)
	cfg := server.Config{Name: "vps", Host: "h", Password: "p", InsecureHostKey: true, Endpoint: "vpn.example.com"}.WithDefaults()
	m := &server.Manager{Config: cfg, Store: server.NewStore(t.TempDir(), "vps"), Runner: hostRunner{}}
	e.srv.Servers = testServers{m}

	code, body := e.do(t, "GET", "/api/v1/servers", readToken, nil)
	if code != 200 || !strings.Contains(string(body), `"initialized": false`) {
		t.Fatalf("list %d %s", code, body)
	}
	if code, body = e.do(t, "POST", "/api/v1/servers/vps/peers", adminToken, map[string]any{"name": "phone"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("add before init %d %s", code, body)
	}
	if _, err := m.Init(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if code, _ = e.do(t, "POST", "/api/v1/servers/vps/peers", readToken, map[string]any{"name": "phone"}); code != http.StatusForbidden {
		t.Fatalf("read token add %d", code)
	}
	code, body = e.do(t, "POST", "/api/v1/servers/vps/peers", adminToken, map[string]any{"name": "phone"})
	var res ServerOpResult
	_ = json.Unmarshal(body, &res)
	if code != http.StatusCreated || !res.OK || res.Peer == nil || res.Peer.Address != "10.8.1.2/32" || res.Plan == nil || !res.Plan.Applied {
		t.Fatalf("add %d %s", code, body)
	}
	if strings.Contains(string(body), "private") {
		t.Errorf("add leaks keys: %s", body)
	}
	code, body = e.do(t, "GET", "/api/v1/servers/vps/peers", readToken, nil)
	if code != 200 || !strings.Contains(string(body), `"phone"`) || strings.Contains(string(body), "private") || strings.Contains(string(body), "preshared") {
		t.Fatalf("peers %d %s", code, body)
	}

	// Client configurations need the full token.
	for _, p := range []string{"conf", "qr.png", "vpn"} {
		if code, _ = e.do(t, "GET", "/api/v1/servers/vps/peers/phone/"+p, readToken, nil); code != http.StatusForbidden {
			t.Errorf("%s with read token: %d", p, code)
		}
	}
	code, body = e.do(t, "GET", "/api/v1/servers/vps/peers/phone/conf", adminToken, nil)
	if code != 200 || !strings.Contains(string(body), "Endpoint = vpn.example.com:51820") {
		t.Fatalf("conf %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/api/v1/servers/vps/peers/phone/qr.png", adminToken, nil)
	if code != 200 || !strings.HasPrefix(string(body), "\x89PNG") {
		t.Fatalf("qr %d", code)
	}
	code, body = e.do(t, "GET", "/api/v1/servers/vps/peers/phone/vpn", adminToken, nil)
	if code != 200 || !strings.HasPrefix(string(body), "vpn://") {
		t.Fatalf("vpn %d %s", code, body)
	}
	if code, _ = e.do(t, "GET", "/api/v1/servers/vps/peers/nobody/conf", adminToken, nil); code != 404 {
		t.Errorf("unknown peer %d", code)
	}
	if code, _ = e.do(t, "GET", "/api/v1/servers/other/peers", adminToken, nil); code != 404 {
		t.Errorf("unknown server %d", code)
	}

	code, body = e.do(t, "POST", "/api/v1/servers/vps/peers/phone/disable", adminToken, nil)
	if code != 200 || !strings.Contains(string(body), `"applied": true`) {
		t.Fatalf("disable %d %s", code, body)
	}
	code, body = e.do(t, "POST", "/api/v1/servers/vps/deploy", adminToken, map[string]any{"dry_run": true})
	if code != 200 || !strings.Contains(string(body), `"ok": true`) {
		t.Fatalf("deploy %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/api/v1/servers/vps/status", readToken, nil)
	if code != 200 || !strings.Contains(string(body), `"up": false`) {
		t.Fatalf("status %d %s", code, body)
	}
	if code, body = e.do(t, "DELETE", "/api/v1/servers/vps/peers/phone?deploy=false", adminToken, nil); code != 200 {
		t.Fatalf("delete %d %s", code, body)
	}
	if st, _ := m.Load(); len(st.Peers) != 0 {
		t.Errorf("peers left %+v", st.Peers)
	}

	e.ops.Lock()
	if code, _ = e.do(t, "POST", "/api/v1/servers/vps/deploy", adminToken, nil); code != http.StatusConflict {
		t.Errorf("busy deploy %d", code)
	}
	e.ops.Unlock()
}

func TestTunnelImportEndpoints(t *testing.T) {
	e := newEnv(t)
	var got []string
	e.srv.ImportKey = func(_ context.Context, trigger, name string, key []byte, apply, replace bool) (*awg.Result, error) {
		got = append(got, fmt.Sprintf("%s %s %v %v %v", trigger, name, apply, replace, strings.HasPrefix(string(key), "vpn://")))
		return &awg.Result{Items: []awg.Item{{Tunnel: name, Interface: "Wireguard1"}}}, nil
	}
	e.srv.ForgetKey = func(_ context.Context, _, name string) (*awg.Result, error) {
		return nil, fmt.Errorf("awg tunnel %q is not an imported key", name)
	}
	if code, _ := e.do(t, "POST", "/api/v1/tunnels/import", readToken, map[string]any{"name": "nl", "key": "vpn://x"}); code != http.StatusForbidden {
		t.Errorf("read token %d", code)
	}
	code, body := e.do(t, "POST", "/api/v1/tunnels/import", adminToken, map[string]any{"name": "nl", "key": "vpn://x"})
	if code != 200 || !strings.Contains(string(body), "Wireguard1") || len(got) != 1 || got[0] != "api nl true false true" {
		t.Fatalf("import %d %s %v", code, body, got)
	}
	if code, _ = e.do(t, "POST", "/api/v1/tunnels/import", adminToken, map[string]any{"name": "nl", "key": "x", "apply": false, "replace": true}); code != 200 || got[1] != "api nl false true false" {
		t.Errorf("import options %v", got)
	}
	if code, body = e.do(t, "DELETE", "/api/v1/tunnels/main", adminToken, nil); code != http.StatusUnprocessableEntity {
		t.Errorf("forget %d %s", code, body)
	}
}
