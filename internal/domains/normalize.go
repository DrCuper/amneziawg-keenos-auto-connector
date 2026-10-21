package domains

import (
	"net/netip"
	"sort"
	"strings"

	"golang.org/x/net/idna"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// Normalize lowercases, trims, converts IDN to punycode, drops service names
// and invalid entries and deduplicates, keeping first occurrence. It returns
// the valid entries and the number of entries dropped as invalid.
func Normalize(in []string) (out []string, dropped int) {
	seen := map[string]bool{}
	for _, raw := range in {
		e, ok := normalizeOne(raw)
		if !ok {
			dropped++
			continue
		}
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out, dropped
}

func normalizeOne(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	e = strings.TrimPrefix(e, "*.")
	e = strings.TrimPrefix(e, ".")
	e = strings.TrimSuffix(e, ".")
	if e == "" {
		return "", false
	}
	if isIPOrCIDR(e) {
		if isServiceAddr(e) {
			return "", false
		}
		return e, true
	}
	if !strings.Contains(e, ".") {
		// localhost, local, bare TLDs, "*"
		return "", false
	}
	if p, err := idna.Lookup.ToASCII(e); err == nil {
		e = p
	} else if p, err := idna.Punycode.ToASCII(e); err == nil {
		e = p
	}
	if router.ValidateEntry(e) != nil {
		return "", false
	}
	return e, true
}

func isIPOrCIDR(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	_, err := netip.ParsePrefix(s)
	return err == nil
}

func isServiceAddr(s string) bool {
	switch s {
	case "0.0.0.0", "127.0.0.1", "::1", "::", "0.0.0.0/0", "::/0":
		return true
	}
	return false
}

// Collapse removes entries whose parent domain is also present, so that
// {example.com, api.example.com, cdn.example.com} becomes {example.com}.
// IP/CIDR entries are kept untouched. Order of the survivors is preserved.
func Collapse(in []string) (out []string, removed int) {
	set := map[string]bool{}
	for _, e := range in {
		set[e] = true
	}
	for _, e := range in {
		if isIPOrCIDR(e) || !hasParentIn(e, set) {
			out = append(out, e)
		} else {
			removed++
		}
	}
	return out, removed
}

func hasParentIn(domain string, set map[string]bool) bool {
	for {
		i := strings.IndexByte(domain, '.')
		if i < 0 {
			return false
		}
		domain = domain[i+1:]
		if !strings.Contains(domain, ".") {
			// a bare TLD is never a listed parent
			return false
		}
		if set[domain] {
			return true
		}
	}
}

// Covered reports whether entry is already matched by the set (exact match or
// a listed parent domain). Used to decide whether a manual `remove` is
// effective and for `groups show --check`.
func Covered(entry string, set map[string]bool) bool {
	return set[entry] || (!isIPOrCIDR(entry) && hasParentIn(entry, set))
}

// Diff returns entries present in b but not in a (added) and in a but not in
// b (removed), both sorted.
func Diff(a, b []string) (added, removed []string) {
	as := map[string]bool{}
	for _, e := range a {
		as[e] = true
	}
	bs := map[string]bool{}
	for _, e := range b {
		bs[e] = true
		if !as[e] {
			added = append(added, e)
		}
	}
	for _, e := range a {
		if !bs[e] {
			removed = append(removed, e)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}
