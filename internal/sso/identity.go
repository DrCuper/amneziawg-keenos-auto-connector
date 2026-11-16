// Package sso accepts identities only from the local SSO gate.
package sso

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

type Identity struct {
	ID         int64
	Name, Role string
}

// ParseProxies parses exact gate addresses or CIDRs. Never trust a whole
// container network when only its gateway needs access.
func ParseProxies(values []string) ([]netip.Prefix, error) {
	if len(values) == 0 {
		values = []string{"127.0.0.1", "::1"}
	}
	var out []netip.Prefix
	for _, value := range values {
		value = strings.TrimSpace(value)
		if ip, err := netip.ParseAddr(value); err == nil {
			ip = ip.Unmap()
			out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q", value)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func Read(r *http.Request, proxies []netip.Prefix) (Identity, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid peer")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid peer")
	}
	if len(proxies) == 0 {
		proxies, _ = ParseProxies(nil)
	}
	trusted := false
	for _, p := range proxies {
		if p.Contains(ip.Unmap()) {
			trusted = true
			break
		}
	}
	if !trusted {
		return Identity{}, fmt.Errorf("untrusted peer")
	}
	// Reject duplicated identity headers rather than choosing an ambiguous user.
	if len(r.Header.Values("X-User")) != 1 || len(r.Header.Values("X-User-Role")) != 1 {
		return Identity{}, fmt.Errorf("missing identity")
	}
	id, err := strconv.ParseInt(r.Header.Get("X-User"), 10, 64)
	role := r.Header.Get("X-User-Role")
	if err != nil || id <= 0 || (role != "viewer" && role != "admin") {
		return Identity{}, fmt.Errorf("invalid identity")
	}
	name, err := url.PathUnescape(r.Header.Get("X-User-Name"))
	if err != nil {
		return Identity{}, fmt.Errorf("invalid name")
	}
	return Identity{ID: id, Name: name, Role: role}, nil
}

// SameOrigin uses the configured origin, never forwarded headers. SSO cookies
// are shared by sibling domains, so SameSite alone cannot protect mutations.
func SameOrigin(r *http.Request, origin string) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	return origin != "" && r.Header.Get("Origin") == strings.TrimRight(origin, "/")
}

func ValidateOrigin(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("want an https origin without a path")
	}
	return nil
}
