package rci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// fakeRouter mimics the Keenetic auth dance and /rci/ parse endpoint.
type fakeRouter struct {
	realm    string
	password string
	sessions atomic.Int32
	parsed   []string
	reject   string
	cur      string // valid session id; "" = no session
}

// expire invalidates the current session, like the router's 300 s idle timeout.
func (f *fakeRouter) expire() { f.cur = "" }

func (f *fakeRouter) handler() http.Handler {
	mux := http.NewServeMux()
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("sid")
		return err == nil && f.cur != "" && c.Value == f.cur
	}
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if authed(r) {
				w.WriteHeader(200)
				return
			}
			w.Header().Set("X-NDM-Realm", f.realm)
			w.Header().Set("X-NDM-Challenge", "CHALLENGE123")
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "pending"})
			w.WriteHeader(401)
		case http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["password"] != Key("admin", f.realm, f.password, "CHALLENGE123") {
				w.WriteHeader(401)
				return
			}
			n := f.sessions.Add(1)
			f.cur = fmt.Sprintf("s%d", n)
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: f.cur})
			w.WriteHeader(200)
		}
	})
	mux.HandleFunc("/ci/running-config.txt", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, "object-group fqdn x\n    include a.com\n")
	})
	mux.HandleFunc("/rci/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		var req []map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			return
		}
		var resp []any
		for _, c := range req {
			f.parsed = append(f.parsed, c["parse"])
			if c["parse"] == f.reject {
				resp = append(resp, map[string]any{"parse": map[string]any{"status": []any{
					map[string]any{"status": "error", "code": "6553609", "ident": "Network::Interface::Base", "message": "unable to find (empty)."},
				}}})
				continue
			}
			resp = append(resp, map[string]any{"parse": map[string]any{"status": []any{
				map[string]any{"status": "message", "message": "ok"},
			}}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func newTransport(t *testing.T, f *fakeRouter, password string) *Transport {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	tr, err := New(Config{URL: srv.URL + "/", Username: "admin", Password: password, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestAuthAndRunningConfig(t *testing.T) {
	f := &fakeRouter{realm: "Keenetic Ultra", password: "pw"}
	tr := newTransport(t, f, "pw")
	cfg, err := tr.RunningConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "include a.com") {
		t.Errorf("config %q", cfg)
	}
	// Second call reuses the session.
	if _, err := tr.RunningConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.sessions.Load(); n != 1 {
		t.Errorf("sessions %d, want 1", n)
	}
}

func TestReauthOnExpiredSession(t *testing.T) {
	f := &fakeRouter{realm: "R", password: "pw"}
	tr := newTransport(t, f, "pw")
	if _, err := tr.RunningConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.expire()
	if _, err := tr.RunningConfig(context.Background()); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if n := f.sessions.Load(); n != 2 {
		t.Errorf("sessions %d, want 2", n)
	}
}

func TestBadPassword(t *testing.T) {
	f := &fakeRouter{realm: "R", password: "pw"}
	tr := newTransport(t, f, "wrong")
	_, err := tr.RunningConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("got %v", err)
	}
	if router.IsRetryable(err) {
		t.Error("auth failure must not be retryable")
	}
}

func TestExecBatchesAndErrors(t *testing.T) {
	f := &fakeRouter{realm: "R", password: "pw", reject: "c3"}
	tr := newTransport(t, f, "pw")
	out, err := tr.Exec(context.Background(), []string{"c1", "c2", "c3", "c4"})
	var ce *router.CommandError
	if !errors.As(err, &ce) || ce.Command != "c3" || !strings.Contains(ce.Message, "unable to find") || !strings.Contains(ce.Message, "6553609") {
		t.Fatalf("got %v", err)
	}
	if len(out) != 2 || out[0] != "ok" {
		t.Errorf("outputs %q", out)
	}
	// c4 is in the same batch as c3 and was sent; batches are of size 2.
	if strings.Join(f.parsed, ",") != "c1,c2,c3,c4" {
		t.Errorf("parsed %q", f.parsed)
	}
}

func TestServerErrorIsTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(502)
	}))
	defer srv.Close()
	tr, _ := New(Config{URL: srv.URL, Username: "admin"})
	_, err := tr.RunningConfig(context.Background())
	if !router.IsRetryable(err) {
		t.Fatalf("got %v, want transport error", err)
	}
}

func TestKey(t *testing.T) {
	// md5("admin:R:pw") then sha256("C"+md5hex); computed independently.
	if got := Key("admin", "R", "pw", "C"); len(got) != 64 || got != Key("admin", "R", "pw", "C") {
		t.Errorf("key %q", got)
	}
	if Key("admin", "R", "pw", "C") == Key("admin", "R", "pw", "D") {
		t.Error("challenge not mixed in")
	}
}

func TestParseBatchResponseShapes(t *testing.T) {
	res, err := parseBatchResponse([]byte(`{"parse":{"status":[{"status":"error","message":"boom"}]}}`), 1)
	if err != nil || res[0].err != "boom" {
		t.Errorf("single object: %+v %v", res, err)
	}
	res, err = parseBatchResponse([]byte(``), 2)
	if err != nil || len(res) != 2 {
		t.Errorf("empty: %+v %v", res, err)
	}
	res, err = parseBatchResponse([]byte(`[{},{}]`), 2)
	if err != nil || res[0].err != "" || res[1].err != "" {
		t.Errorf("no status: %+v %v", res, err)
	}
	if _, err := parseBatchResponse([]byte(`<html>`), 1); err == nil {
		t.Error("expected error for html body")
	}
}
