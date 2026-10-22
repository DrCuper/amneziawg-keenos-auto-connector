package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/domains"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/health"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/source"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

const (
	adminToken = "admin-token-0123456789"
	readToken  = "read-token-0123456789"
	privKey    = "YUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="
)

const conf = `[Interface]
PrivateKey = ` + privKey + `
Address = 10.8.1.2/32
[Peer]
PublicKey = QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=
Endpoint = 203.0.113.1:51820
AllowedIPs = 0.0.0.0/0
`

type env struct {
	srv  *Server
	ts   *httptest.Server
	fake *fake.Transport
	ops  *sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "domains.yaml"), []byte("connections:\n  awg:\n    routes: [{tunnel: main}]\ngroups:\n  - name: a\n    connection: awg\n    sources: [{file: a.txt}]\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a1.com\na2.com\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "main.conf"), []byte(conf), 0o600)
	st, err := store.NewFile(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New("")
	client := router.NewClient(f, cli.Planner{}, router.Options{Timeout: time.Second, SaveConfig: true})
	j, err := journal.Open(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tunnels := &awg.Manager{Router: client, Tunnels: []awg.Tunnel{{Name: "main", ConfFile: "main.conf"}}, BaseDir: dir}
	s := &syncer.Syncer{
		SpecPath: filepath.Join(dir, "domains.yaml"), Store: st, Fetcher: &source.Fetcher{Cache: st},
		Router: client, Limits: domains.Limits{MaxEntries: 100, OnLimit: domains.OnLimitError}, KeepVersions: 5,
		Observers: []syncer.Observer{&journal.Observer{J: j}}, ResolveTunnel: tunnels.Resolve,
		DefaultTrigger: syncer.TriggerAPI,
	}
	ops := &sync.Mutex{}
	srv := &Server{
		Syncer: s, Store: st, SpecPath: s.SpecPath, Journal: j, Alerter: &notify.Alerter{}, Tunnels: tunnels, Router: client,
		Checks: func() []health.Result {
			return []health.Result{{Signal: notify.Signal{Alert: notify.AlertAWGDown, Check: "main"}}}
		},
		Record: func(e *journal.Entry) { _, _ = j.Append(e) },
		Token:  adminToken, ReadToken: readToken, UI: true, Ops: ops, Version: "test",
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{srv: srv, ts: ts, fake: f, ops: ops}
}

func (e *env) do(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.do(t, "GET", "/api/v1/health", "", nil); code != 200 {
		t.Errorf("health without token: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/v1/groups", "", nil); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/v1/groups", "wrong-token-0123456789", nil); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/v1/groups", readToken, nil); code != 200 {
		t.Errorf("read token GET: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/v1/sync", readToken, map[string]any{"dry_run": true}); code != 403 {
		t.Errorf("read token POST: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/v1/nope", adminToken, nil); code != 404 {
		t.Errorf("unknown endpoint: %d", code)
	}
}

func TestTunnelSyncAndJournalFlow(t *testing.T) {
	e := newEnv(t)
	// Sync before the tunnel is imported: the group fails.
	code, body := e.do(t, "POST", "/api/v1/sync", adminToken, map[string]any{})
	if code != 422 || !strings.Contains(string(body), "not imported yet") {
		t.Fatalf("sync before import: %d %s", code, body)
	}
	// Plan and apply the tunnel; keys never appear in the answer.
	code, body = e.do(t, "POST", "/api/v1/tunnels/apply", adminToken, map[string]any{"dry_run": true})
	if code != 200 || strings.Contains(string(body), privKey) || !strings.Contains(string(body), "<secret>") {
		t.Fatalf("tunnel plan: %d %s", code, body)
	}
	if code, body = e.do(t, "POST", "/api/v1/tunnels/apply", adminToken, map[string]any{}); code != 200 {
		t.Fatalf("tunnel apply: %d %s", code, body)
	}
	// Now the sync works.
	code, body = e.do(t, "POST", "/api/v1/sync", adminToken, map[string]any{})
	var op OpResult
	_ = json.Unmarshal(body, &op)
	if code != 200 || !op.OK || op.Entry.Applied == 0 {
		t.Fatalf("sync: %d %s", code, body)
	}
	// Manual add with apply.
	code, body = e.do(t, "POST", "/api/v1/groups/a/manual", adminToken, map[string]any{"add": []string{"x.example"}, "apply": true})
	if code != 200 || !strings.Contains(string(body), "include x.example") {
		t.Fatalf("manual: %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/api/v1/groups/a", readToken, nil)
	var gd GroupDetail
	_ = json.Unmarshal(body, &gd)
	if code != 200 || gd.Latest != 2 || gd.Applied != 2 || len(gd.Version.Entries) != 3 {
		t.Fatalf("group: %d %s", code, body)
	}
	// Rollback to v1 (dry-run first).
	if code, body = e.do(t, "POST", "/api/v1/groups/a/rollback", adminToken, map[string]any{"to": 1, "dry_run": true}); code != 200 {
		t.Fatalf("rollback dry: %d %s", code, body)
	}
	if code, body = e.do(t, "POST", "/api/v1/groups/a/rollback", adminToken, map[string]any{"to": 1}); code != 200 {
		t.Fatalf("rollback: %d %s", code, body)
	}
	_, body = e.do(t, "GET", "/api/v1/groups/a/versions", readToken, nil)
	var vs []VersionInfo
	_ = json.Unmarshal(body, &vs)
	if len(vs) != 2 || !vs[1].Pinned || !vs[1].Applied {
		t.Errorf("versions: %s", body)
	}
	// Journal shows the operations, triggered by the API.
	code, body = e.do(t, "GET", "/api/v1/journal?limit=20", readToken, nil)
	var es []*journal.Entry
	_ = json.Unmarshal(body, &es)
	if code != 200 || len(es) < 5 {
		t.Fatalf("journal: %d %s", code, body)
	}
	kinds := map[string]bool{}
	for _, x := range es {
		kinds[x.Kind] = true
		if x.Trigger != syncer.TriggerAPI {
			t.Errorf("entry %d trigger %q", x.ID, x.Trigger)
		}
	}
	for _, k := range []string{"sync", "manual", "rollback", "awg-apply"} {
		if !kinds[k] {
			t.Errorf("journal lacks %s: %v", k, kinds)
		}
	}
	if code, _ := e.do(t, "GET", "/api/v1/journal/"+jsonID(es[0]), readToken, nil); code != 200 {
		t.Errorf("journal entry: %d", code)
	}
	// Router state hides keys.
	code, body = e.do(t, "GET", "/api/v1/router", readToken, nil)
	if code != 200 || strings.Contains(string(body), privKey) || !strings.Contains(string(body), "Wireguard0") {
		t.Errorf("router: %d %s", code, body)
	}
	// Tunnel status.
	code, body = e.do(t, "GET", "/api/v1/tunnels/status", readToken, nil)
	if code != 200 || !strings.Contains(string(body), "no handshake") {
		t.Errorf("tunnel status: %d %s", code, body)
	}
}

func jsonID(e *journal.Entry) string { b, _ := json.Marshal(e.ID); return string(b) }

func TestBusy(t *testing.T) {
	e := newEnv(t)
	e.ops.Lock()
	defer e.ops.Unlock()
	if code, _ := e.do(t, "POST", "/api/v1/sync", adminToken, map[string]any{"dry_run": true}); code != 409 {
		t.Errorf("busy: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/v1/groups", adminToken, nil); code != 200 {
		t.Errorf("reads must not wait for the lock: %d", code)
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		method, path string
		body         any
		code         int
	}{
		{"POST", "/api/v1/sync", map[string]any{"bogus": 1}, 400},
		{"POST", "/api/v1/groups/nope/manual", map[string]any{"add": []string{"x.com"}}, 404},
		{"POST", "/api/v1/groups/a/manual", map[string]any{}, 400},
		{"POST", "/api/v1/groups/a/manual", map[string]any{"add": []string{"bad domain!"}}, 400},
		{"GET", "/api/v1/groups/a?version=x", nil, 400},
		{"GET", "/api/v1/journal?since=yesterday", nil, 400},
		{"GET", "/api/v1/journal/999", nil, 404},
	} {
		if code, body := e.do(t, c.method, c.path, adminToken, c.body); code != c.code {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
}

func TestStatusMetricsUI(t *testing.T) {
	e := newEnv(t)
	code, body := e.do(t, "GET", "/api/v1/status", readToken, nil)
	var st Status
	_ = json.Unmarshal(body, &st)
	if code != 200 || st.Groups != 1 || st.Tunnels != 1 || len(st.Checks) != 1 || st.Version != "test" {
		t.Errorf("status: %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/metrics", readToken, nil)
	for _, s := range []string{"awgkeen_up 1", `awgkeen_check_ok{check="awg_down:main"} 1`, `awgkeen_group_entries{group="a"} 0`} {
		if !strings.Contains(string(body), s) {
			t.Errorf("metrics lack %q: %s", s, body)
		}
	}
	_ = code
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(page), "<title>awgkeen</title>") || resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("ui: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestNoTokenMode(t *testing.T) {
	e := newEnv(t)
	e.srv.Token, e.srv.ReadToken = "", ""
	if code, _ := e.do(t, "POST", "/api/v1/sync", "", map[string]any{"dry_run": true}); code == 401 || code == 403 {
		t.Errorf("no-token mode rejected: %d", code)
	}
	_ = context.Background
}
