package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSORolesAndMachineTokens(t *testing.T) {
	e := newEnv(t)
	e.srv.SSO = true
	e.srv.PublicURL = "https://awgkeen.drcuper.com"
	h := e.srv.Handler()
	call := func(method, path, remote, role, origin, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://awgkeen.drcuper.com"+path, strings.NewReader(`{"add":["sso.example"],"apply":false}`))
		r.RemoteAddr = remote
		if role != "" {
			r.Header.Set("X-User", "42")
			r.Header.Set("X-User-Role", role)
		}
		r.Header.Set("Origin", origin)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/", "/api/v1/groups"} {
		for _, remote := range []string{"127.0.0.1:4", "203.0.113.3:4"} {
			role := ""
			if strings.HasPrefix(remote, "203") {
				role = "admin"
			}
			if w := call("GET", path, remote, role, "", ""); w.Code != 403 {
				t.Fatalf("bypass %s: %d", path, w.Code)
			}
		}
	}
	if w := call("GET", "/api/v1/groups", "127.0.0.1:4", "viewer", "", ""); w.Code != 200 {
		t.Fatalf("viewer: %d", w.Code)
	}
	for _, tc := range []struct{ role, origin string }{{"viewer", e.srv.PublicURL}, {"admin", "https://spotify.drcuper.com"}, {"admin", ""}} {
		if w := call("POST", "/api/v1/groups/a/manual", "127.0.0.1:4", tc.role, tc.origin, ""); w.Code != 403 {
			t.Fatalf("mutation accepted: %+v %d", tc, w.Code)
		}
	}
	if w := call("POST", "/api/v1/groups/a/manual", "127.0.0.1:4", "admin", e.srv.PublicURL, ""); w.Code != 200 {
		t.Fatalf("admin mutation: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/v1/groups", "203.0.113.3:4", "", "", readToken); w.Code != 200 {
		t.Fatalf("machine token: %d", w.Code)
	}
	if w := call("POST", "/api/v1/groups/a/manual", "203.0.113.3:4", "", "", readToken); w.Code != 403 {
		t.Fatal("read token mutation")
	}
}
