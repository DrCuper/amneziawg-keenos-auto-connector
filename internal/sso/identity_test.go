package sso

import (
	"net/http/httptest"
	"testing"
)

func TestIdentityTrustBoundary(t *testing.T) {
	proxies, err := ParseProxies([]string{"172.30.81.1", "::1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, peer, id, role, nameHeader string
		duplicate, allowed               bool
	}{
		{name: "gateway", peer: "172.30.81.1:10", id: "42", role: "viewer", nameHeader: "%D0%90%D0%BD%D1%8F", allowed: true},
		{name: "IPv6", peer: "[::1]:10", id: "42", role: "admin", allowed: true},
		{name: "mapped IPv4", peer: "[::ffff:172.30.81.1]:10", id: "42", role: "viewer", allowed: true},
		{name: "neighbor container", peer: "172.30.81.2:10", id: "42", role: "admin"},
		{name: "forwarded public peer", peer: "203.0.113.4:10", id: "42", role: "admin"},
		{name: "invalid peer", peer: "not-an-address", id: "42", role: "admin"},
		{name: "missing user", peer: "172.30.81.1:10", role: "admin"},
		{name: "negative user", peer: "172.30.81.1:10", id: "-42", role: "admin"},
		{name: "overflow", peer: "172.30.81.1:10", id: "9223372036854775808", role: "admin"},
		{name: "unknown role", peer: "172.30.81.1:10", id: "42", role: "owner"},
		{name: "duplicate user", peer: "172.30.81.1:10", id: "42", role: "admin", duplicate: true},
		{name: "invalid encoding", peer: "172.30.81.1:10", id: "42", role: "admin", nameHeader: "%GG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://service.drcuper.com/", nil)
			r.RemoteAddr = tc.peer
			// Neither forwarded header may make a peer trusted.
			r.Header.Set("X-Forwarded-For", "172.30.81.1")
			r.Header.Set("CF-Connecting-IP", "172.30.81.1")
			if tc.id != "" {
				r.Header.Set("X-User", tc.id)
			}
			r.Header.Set("X-User-Role", tc.role)
			r.Header.Set("X-User-Name", tc.nameHeader)
			if tc.duplicate {
				r.Header.Add("X-User", "43")
			}
			id, err := Read(r, proxies)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v identity=%+v error=%v", tc.allowed, id, err)
			}
			if tc.name == "gateway" && id.Name != "Аня" {
				t.Fatalf("name=%q", id.Name)
			}
		})
	}
	if _, err := ParseProxies([]string{"172.30.81.1", "bad"}); err == nil {
		t.Fatal("invalid proxy list accepted")
	}
}
