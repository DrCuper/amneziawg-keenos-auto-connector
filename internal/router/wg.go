package router

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// WGInterface is an `interface Wireguard<N>` section of running-config, or
// the desired state of one. Only the fields this service manages are kept.
//
// On a parsed State, PrivateKey and peer PresharedKey are filled only when
// the firmware prints them in running-config (most do not); they are
// write-only from the service's point of view.
type WGInterface struct {
	Name          string // Wireguard0, Wireguard1, ...
	Description   string
	SecurityLevel string // public | private | protected; "" = leave as is (desired)
	Address       string // IPv4 prefix, e.g. 10.8.1.2/32
	MTU           int    // 0 = leave as is (desired)
	// Global is `ip global`: "auto" or a priority number; "" = leave as is
	// (desired) / not set (state); "off" (desired) removes it.
	Global     string
	AdjustMSS  bool
	ListenPort int // 0 = leave as is (desired)
	// ASC are the AmneziaWG obfuscation values in `wireguard asc` order:
	// Jc Jmin Jmax S1 S2 H1 H2 H3 H4 [S3 S4 I1 I2 I3 I4 I5]. Empty = plain
	// WireGuard (desired: `no wireguard asc`).
	ASC   []string
	Peers []WGPeer
	Up    bool

	PrivateKey string
	// SetKeys (desired only) forces the private and preshared keys to be
	// sent even though the router does not show them: the caller sets it
	// when the keys changed since the last successful apply.
	SetKeys bool
}

// WGPeer is one `wireguard peer <public-key>` subsection.
type WGPeer struct {
	PublicKey    string
	Endpoint     string   // host:port
	AllowedIPs   []string // canonical prefixes (0.0.0.0/0)
	Keepalive    int      // seconds, 0 = off
	PresharedKey string
}

// Peer returns the peer with the given public key.
func (w *WGInterface) Peer(key string) (WGPeer, bool) {
	for _, p := range w.Peers {
		if p.PublicKey == key {
			return p, true
		}
	}
	return WGPeer{}, false
}

// Interface returns the parsed Wireguard interface with the given name.
func (s *State) Interface(name string) (WGInterface, bool) {
	for _, w := range s.Interfaces {
		if strings.EqualFold(w.Name, name) {
			return w, true
		}
	}
	return WGInterface{}, false
}

// FreeWireguardName returns the lowest Wireguard<N> not present in the state
// and not listed in taken.
func (s *State) FreeWireguardName(taken ...string) string {
	used := map[string]bool{}
	for _, w := range s.Interfaces {
		used[strings.ToLower(w.Name)] = true
	}
	for _, t := range taken {
		used[strings.ToLower(t)] = true
	}
	for i := 0; ; i++ {
		n := "Wireguard" + strconv.Itoa(i)
		if !used[strings.ToLower(n)] {
			return n
		}
	}
}

var wgName = regexp.MustCompile(`^Wireguard[0-9]{1,3}$`)

// ValidateWGName accepts Wireguard0..Wireguard999.
func ValidateWGName(name string) error {
	if !wgName.MatchString(name) {
		return fmt.Errorf("interface %q: name must be Wireguard<N>", name)
	}
	return nil
}

// ValidateKey checks a base64 WireGuard key (32 bytes). The key itself is
// never part of the error.
func ValidateKey(k string) error {
	b, err := base64.StdEncoding.DecodeString(k)
	if err != nil || len(b) != 32 {
		return errors.New("not a base64 32-byte WireGuard key")
	}
	return nil
}

// CanonicalPrefix turns "10.0.0.1", "10.0.0.0/8" or "0.0.0.0 0.0.0.0"
// (address + mask, as Keenetic prints it) into a canonical prefix.
func CanonicalPrefix(s string) (string, error) {
	s = strings.TrimSpace(s)
	if a, m, ok := strings.Cut(s, " "); ok {
		addr, err := netip.ParseAddr(a)
		if err != nil {
			return "", fmt.Errorf("bad address %q", s)
		}
		mask, err := netip.ParseAddr(strings.TrimSpace(m))
		if err != nil || !mask.Is4() || !addr.Is4() {
			return "", fmt.Errorf("bad address/mask %q", s)
		}
		bits, ok := maskBits(mask.As4())
		if !ok {
			return "", fmt.Errorf("non-contiguous mask %q", s)
		}
		return netip.PrefixFrom(addr, bits).String(), nil
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.String(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return "", fmt.Errorf("bad address %q", s)
	}
	return netip.PrefixFrom(a, a.BitLen()).String(), nil
}

// PrefixMask renders an IPv4 prefix as "addr mask"; IPv6 stays "addr/len".
func PrefixMask(prefix string) string {
	p, err := netip.ParsePrefix(prefix)
	if err != nil || !p.Addr().Is4() {
		return prefix
	}
	var m [4]byte
	for i := 0; i < p.Bits(); i++ {
		m[i/8] |= 0x80 >> (i % 8)
	}
	return p.Addr().String() + " " + netip.AddrFrom4(m).String()
}

func maskBits(m [4]byte) (int, bool) {
	v := uint32(m[0])<<24 | uint32(m[1])<<16 | uint32(m[2])<<8 | uint32(m[3])
	bits := 0
	for v&0x80000000 != 0 {
		bits++
		v <<= 1
	}
	return bits, v == 0
}

var ascRange = regexp.MustCompile(`^[0-9]+(-[0-9]+)?$`)

// normalize validates a desired interface and canonicalizes its values.
func (w *WGInterface) normalize() error {
	var errs []error
	w.Name = strings.TrimSpace(w.Name)
	if err := ValidateWGName(w.Name); err != nil {
		return err
	}
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("interface %s: "+format, append([]any{w.Name}, a...)...))
	}
	if strings.ContainsAny(w.Description, "\"\n") {
		bad("description must not contain quotes or newlines")
	}
	switch w.SecurityLevel {
	case "", "public", "private", "protected":
	default:
		bad("security level %q is not public, private or protected", w.SecurityLevel)
	}
	if w.Address != "" {
		p, err := CanonicalPrefix(w.Address)
		if err != nil || !strings.Contains(p, ".") {
			bad("address must be an IPv4 address or prefix")
		} else {
			w.Address = p
		}
	}
	if w.MTU != 0 && (w.MTU < 576 || w.MTU > 1500) {
		bad("mtu %d out of range 576..1500", w.MTU)
	}
	if g := w.Global; g != "" && g != "auto" && g != "off" {
		if n, err := strconv.Atoi(g); err != nil || n < 0 || n > 65535 {
			bad("ip global must be auto, off or a number")
		}
	}
	if w.ListenPort < 0 || w.ListenPort > 65535 {
		bad("listen port %d out of range", w.ListenPort)
	}
	if w.PrivateKey != "" {
		if err := ValidateKey(w.PrivateKey); err != nil {
			bad("private key: %v", err)
		}
	}
	if n := len(w.ASC); n != 0 && n != 9 && n != 16 {
		bad("asc needs 9 or 16 values, got %d", n)
	} else {
		for i, v := range w.ASC {
			switch {
			case i < 5 || i == 9 || i == 10: // Jc Jmin Jmax S1 S2 S3 S4
				if _, err := strconv.ParseUint(v, 10, 32); err != nil {
					bad("asc value #%d %q is not a number", i+1, v)
				}
			case i < 9: // H1..H4: number or range
				if !ascRange.MatchString(v) {
					bad("asc header #%d %q is not a number or range", i-4, v)
				}
			default: // I1..I5: free-form, may be empty
				if strings.ContainsAny(v, "\"\n") {
					bad("asc I%d must not contain quotes or newlines", i-10)
				}
			}
		}
	}
	seen := map[string]bool{}
	for i := range w.Peers {
		p := &w.Peers[i]
		if err := ValidateKey(p.PublicKey); err != nil {
			bad("peer #%d public key: %v", i+1, err)
			continue
		}
		if seen[p.PublicKey] {
			bad("peer #%d listed twice", i+1)
		}
		seen[p.PublicKey] = true
		if p.PresharedKey != "" {
			if err := ValidateKey(p.PresharedKey); err != nil {
				bad("peer #%d preshared key: %v", i+1, err)
			}
		}
		if p.Endpoint != "" && (strings.ContainsAny(p.Endpoint, " \t\"") || !strings.Contains(p.Endpoint, ":")) {
			bad("peer #%d endpoint %q must be host:port", i+1, p.Endpoint)
		}
		if p.Keepalive < 0 || p.Keepalive > 65535 {
			bad("peer #%d keepalive out of range", i+1)
		}
		ips := make([]string, 0, len(p.AllowedIPs))
		have := map[string]bool{}
		for _, a := range p.AllowedIPs {
			c, err := CanonicalPrefix(a)
			if err != nil {
				bad("peer #%d allowed ip: %v", i+1, err)
				continue
			}
			if !have[c] {
				have[c] = true
				ips = append(ips, c)
			}
		}
		p.AllowedIPs = ips
	}
	return errors.Join(errs...)
}
