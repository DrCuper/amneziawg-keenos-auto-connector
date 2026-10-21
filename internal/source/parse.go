// Package source loads domain lists from URLs and local files and parses the
// list formats used by community repositories (plain, hosts, dnsmasq, v2fly,
// Clash). Parsing is pure; fetching lives in fetch.go.
package source

import (
	"bufio"
	"fmt"
	"strings"
	"unicode"
)

// Format is a list format. Auto detects the format line by line, so one file
// may mix plain domains with hosts or dnsmasq lines.
type Format string

const (
	FormatAuto    Format = "auto"
	FormatPlain   Format = "plain"
	FormatHosts   Format = "hosts"
	FormatDnsmasq Format = "dnsmasq"
	FormatV2fly   Format = "v2fly"
	FormatClash   Format = "clash"
)

// ParseFormat validates a format name ("" means auto).
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(strings.TrimSpace(s))); f {
	case "", FormatAuto:
		return FormatAuto, nil
	case FormatPlain, FormatHosts, FormatDnsmasq, FormatV2fly, FormatClash:
		return f, nil
	default:
		return "", fmt.Errorf("unknown list format %q (auto, plain, hosts, dnsmasq, v2fly, clash)", s)
	}
}

// Stats counts what happened to the lines of one list.
type Stats struct {
	Lines   int `json:"lines"`   // non-empty, non-comment lines
	Parsed  int `json:"parsed"`  // lines that yielded an entry
	Skipped int `json:"skipped"` // lines that were not understood or intentionally ignored
}

// Parse extracts raw entries (domains, wildcards stripped, or IP/CIDR) from
// the list text. Entries are returned in file order and are NOT normalized or
// validated; see package domains for that.
func Parse(format Format, text string) ([]string, Stats) {
	var out []string
	var st Stats
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || isComment(line) {
			continue
		}
		st.Lines++
		e, ok := parseLine(format, line)
		if !ok || e == "" {
			st.Skipped++
			continue
		}
		st.Parsed++
		out = append(out, e)
	}
	return out, st
}

func isComment(line string) bool {
	return strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "!") || strings.HasPrefix(line, ";")
}

// parseLine returns the entry for one line. The second result is false when
// the line is not understood in the given format.
func parseLine(format Format, line string) (string, bool) {
	// Inline comments.
	if i := strings.Index(line, " #"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	switch format {
	case FormatPlain:
		return parsePlain(line)
	case FormatHosts:
		return parseHosts(line)
	case FormatDnsmasq:
		return parseDnsmasq(line)
	case FormatV2fly:
		return parseV2fly(line)
	case FormatClash:
		return parseClash(line)
	default: // auto
		switch {
		case strings.Contains(line, "=/"):
			return parseDnsmasq(line)
		case strings.Contains(line, ","):
			return parseClash(line)
		case strings.Contains(line, ":") && !strings.Contains(line, "://") && !isIPv6ish(line):
			return parseV2fly(line)
		case strings.ContainsAny(line, " \t"):
			return parseHosts(line)
		default:
			return parsePlain(line)
		}
	}
}

func parsePlain(line string) (string, bool) {
	if strings.ContainsAny(line, " \t") {
		return "", false
	}
	// URLs: https://example.com/path -> example.com
	if i := strings.Index(line, "://"); i >= 0 {
		line = line[i+3:]
		if j := strings.IndexAny(line, "/?#"); j >= 0 {
			line = line[:j]
		}
		if at := strings.LastIndex(line, "@"); at >= 0 {
			line = line[at+1:]
		}
		if !strings.HasPrefix(line, "[") {
			if j := strings.LastIndex(line, ":"); j >= 0 && !strings.Contains(line[:j], ":") {
				line = line[:j]
			}
		}
	}
	line = strings.TrimPrefix(line, "*.")
	line = strings.TrimPrefix(line, ".")
	if line == "" || strings.Contains(line, "/") && !looksLikeCIDR(line) {
		return "", false
	}
	return line, plausible(line)
}

// plausible is a cheap pre-filter: a domain or IP/CIDR consists of letters,
// digits, '-', '_', '.', ':' and '/' and has at least one dot or colon. Full
// validation happens in package domains after normalization.
func plausible(s string) bool {
	if !strings.ContainsAny(s, ".:") {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':', c == '/':
		case c > 127 && !unicode.IsSpace(c) && !unicode.IsPunct(c):
		default:
			return false
		}
	}
	return true
}

func parseHosts(line string) (string, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", false
	}
	if !isIPLiteral(f[0]) {
		return "", false
	}
	return parsePlain(f[1])
}

func parseDnsmasq(line string) (string, bool) {
	// server=/example.com/1.1.1.1  ipset=/example.com/vpn  nftset=/example.com/4#inet#t#s  address=/example.com/0.0.0.0
	i := strings.Index(line, "=/")
	if i < 0 {
		return "", false
	}
	key := line[:i]
	switch key {
	case "server", "ipset", "nftset", "address", "local":
	default:
		return "", false
	}
	rest := line[i+2:]
	j := strings.Index(rest, "/")
	if j < 0 {
		return "", false
	}
	return parsePlain(rest[:j])
}

func parseV2fly(line string) (string, bool) {
	// domain:example.com  full:example.com  (attributes after space: "domain:a.com @ads")
	if f := strings.Fields(line); len(f) > 0 {
		line = f[0]
	}
	i := strings.Index(line, ":")
	if i < 0 {
		return parsePlain(line)
	}
	switch line[:i] {
	case "domain", "full":
		return parsePlain(line[i+1:])
	default: // regexp:, keyword:, include: are not representable as fqdn entries
		return "", false
	}
}

func parseClash(line string) (string, bool) {
	// DOMAIN-SUFFIX,example.com  DOMAIN,example.com[,POLICY]
	f := strings.Split(line, ",")
	if len(f) < 2 {
		return "", false
	}
	switch strings.ToUpper(strings.TrimSpace(f[0])) {
	case "DOMAIN-SUFFIX", "DOMAIN":
		return parsePlain(strings.TrimSpace(f[1]))
	default: // DOMAIN-KEYWORD, IP-CIDR, GEOIP, PROCESS-NAME ...
		return "", false
	}
}

func isIPLiteral(s string) bool {
	for _, c := range s {
		ok := c >= '0' && c <= '9' || c == '.' || c == ':' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !ok {
			return false
		}
	}
	return strings.Contains(s, ".") || strings.Contains(s, ":")
}

func isIPv6ish(s string) bool {
	return strings.Count(s, ":") >= 2
}

func looksLikeCIDR(s string) bool {
	i := strings.LastIndex(s, "/")
	if i <= 0 {
		return false
	}
	for _, c := range s[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return isIPLiteral(s[:i])
}
