// Package cli knows the KeeneticOS CLI dialect: how running-config is laid
// out and which commands change it. It is pure and transport-agnostic.
package cli

import (
	"strings"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// Parse extracts FQDN object groups and dns-proxy routes from running-config.
//
// The format is a tree of indented lines:
//
//	object-group fqdn test
//	    description "my list"
//	    include a.com
//	    exclude x.a.com
//	dns-proxy
//	    route object-group test Wireguard2 auto
//
// Unknown sections and lines are ignored; indentation width does not matter,
// only whether a line is indented or not. Comment lines (`!`) are skipped.
func Parse(text string) *router.State {
	st := &router.State{Raw: text}
	var (
		curGroup *router.FQDNGroup
		inDNS    bool
	)
	flush := func() {
		if curGroup != nil {
			st.Groups = append(st.Groups, *curGroup)
			curGroup = nil
		}
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" {
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		fields := Fields(strings.TrimSpace(line))
		if len(fields) == 0 || fields[0] == "!" {
			continue
		}
		if !indented {
			flush()
			inDNS = false
			switch {
			case len(fields) >= 3 && fields[0] == "object-group" && fields[1] == "fqdn":
				g := router.FQDNGroup{Name: fields[2]}
				if len(fields) > 3 {
					// one-line form: object-group fqdn NAME include X
					applyGroupLine(&g, fields[3:])
					st.Groups = append(st.Groups, g)
				} else {
					curGroup = &g
				}
			case fields[0] == "dns-proxy":
				if len(fields) > 1 {
					applyDNSLine(st, fields[1:])
				} else {
					inDNS = true
				}
			}
			continue
		}
		switch {
		case curGroup != nil:
			applyGroupLine(curGroup, fields)
		case inDNS:
			applyDNSLine(st, fields)
		}
	}
	flush()
	return st
}

func applyGroupLine(g *router.FQDNGroup, f []string) {
	if len(f) < 2 {
		return
	}
	switch f[0] {
	case "description":
		g.Description = strings.Join(f[1:], " ")
	case "include":
		g.Include = append(g.Include, strings.ToLower(f[1]))
	case "exclude":
		g.Exclude = append(g.Exclude, strings.ToLower(f[1]))
	}
}

// applyDNSLine handles `route object-group G DEST [via] [auto] [reject]`.
func applyDNSLine(st *router.State, f []string) {
	if len(f) < 4 || f[0] != "route" || f[1] != "object-group" {
		return
	}
	r := router.DNSRoute{Group: f[2]}
	rest := f[3:]
	var dest []string
	for _, t := range rest {
		switch t {
		case "auto":
			r.Auto = true
		case "reject":
			r.Reject = true
		default:
			dest = append(dest, t)
		}
	}
	if len(dest) == 0 {
		return
	}
	if isIPLike(dest[0]) {
		r.Gateway = dest[0]
		if len(dest) > 1 {
			r.Via = dest[1]
		}
	} else {
		r.Interface = dest[0]
	}
	st.Routes = append(st.Routes, r)
}

// Fields splits a CLI line on whitespace, honouring double quotes
// (`description "my list"` → [description, my list]).
func Fields(s string) []string {
	var (
		out   []string
		cur   strings.Builder
		inQ   bool
		token bool
	)
	for _, c := range s {
		switch {
		case c == '"':
			inQ = !inQ
			token = true
		case !inQ && (c == ' ' || c == '\t'):
			if token {
				out = append(out, cur.String())
				cur.Reset()
				token = false
			}
		default:
			cur.WriteRune(c)
			token = true
		}
	}
	if token {
		out = append(out, cur.String())
	}
	return out
}

func isIPLike(s string) bool {
	for _, c := range s {
		hexDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !hexDigit && c != '.' && c != ':' {
			return false
		}
	}
	return strings.ContainsAny(s, ".:")
}
