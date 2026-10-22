// Package awg imports AmneziaWG client configurations onto the router: it
// parses Amnezia `.conf` files and `vpn://` keys, turns them into desired
// Wireguard interfaces for router.Client, remembers which interface belongs
// to which tunnel and judges tunnel health from the interface status.
package awg

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/curve25519"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// Conf is a parsed AmneziaWG client configuration.
type Conf struct {
	PrivateKey string
	Addresses  []string // canonical prefixes, IPv4 first as in the file
	DNS        []string
	MTU        int
	ListenPort int
	// ASC holds the obfuscation values by name (Jc, Jmin, ..., I5).
	ASC   map[string]string
	Peers []Peer
	// Warnings list settings that are ignored on the router.
	Warnings []string
}

// Peer is a [Peer] section.
type Peer struct {
	PublicKey    string
	PresharedKey string
	Endpoint     string
	AllowedIPs   []string
	Keepalive    int
}

// ascOrder is the `wireguard asc` argument order.
var ascOrder = []string{"Jc", "Jmin", "Jmax", "S1", "S2", "H1", "H2", "H3", "H4", "S3", "S4", "I1", "I2", "I3", "I4", "I5"}

// ascDefaults are AmneziaWG's values for omitted parameters (plain
// WireGuard message types for H1..H4).
var ascDefaults = map[string]string{"H1": "1", "H2": "2", "H3": "3", "H4": "4"}

// ignored settings are accepted with a warning: they have no router
// counterpart (wg-quick hooks, AWG 1.5 J1..J3/Itime not supported by
// KeeneticOS).
var ignored = map[string]bool{
	"table": true, "preup": true, "postup": true, "predown": true, "postdown": true, "saveconfig": true,
	"fwmark": true, "j1": true, "j2": true, "j3": true, "itime": true,
}

// ASCValues returns the `wireguard asc` arguments: nil for plain WireGuard,
// 9 values (AWG 1.0) or 16 values when any AWG 2.0 field (S3, S4, I1..I5)
// is set.
func (c *Conf) ASCValues() []string {
	if len(c.ASC) == 0 {
		return nil
	}
	n := 9
	for _, k := range ascOrder[9:] {
		if v, ok := c.ASC[k]; ok && v != "" && v != "0" {
			n = 16
		}
	}
	out := make([]string, n)
	for i, k := range ascOrder[:n] {
		v, ok := c.ASC[k]
		switch {
		case ok:
			out[i] = v
		case ascDefaults[k] != "":
			out[i] = ascDefaults[k]
		case i < 11:
			out[i] = "0"
		}
	}
	return out
}

// IPv4Address returns the first IPv4 address prefix ("" when none).
func (c *Conf) IPv4Address() string {
	for _, a := range c.Addresses {
		if strings.Contains(a, ".") {
			return a
		}
	}
	return ""
}

// PublicKey derives the interface's public key from the private key.
func (c *Conf) PublicKey() (string, error) { return PublicKey(c.PrivateKey) }

// PublicKey derives a WireGuard public key from a base64 private key.
func PublicKey(private string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(private)
	if err != nil || len(b) != 32 {
		return "", errors.New("invalid private key")
	}
	pub, err := curve25519.X25519(b, curve25519.Basepoint)
	if err != nil {
		return "", errors.New("invalid private key")
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// Parse reads a `.conf` text or a `vpn://` key (Amnezia share format).
// Error messages never contain key material.
func Parse(data []byte) (*Conf, error) {
	text := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if strings.HasPrefix(text, "vpn://") {
		conf, err := DecodeVPNKey(text)
		if err != nil {
			return nil, err
		}
		text = conf
	}
	return parseINI(text)
}

func parseINI(text string) (*Conf, error) {
	c := &Conf{ASC: map[string]string{}}
	var (
		section string
		peer    *Peer
		errs    []error
	)
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || s[0] == '#' || s[0] == ';' {
			continue
		}
		if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
			section = strings.ToLower(strings.TrimSpace(s[1 : len(s)-1]))
			switch section {
			case "interface":
			case "peer":
				c.Peers = append(c.Peers, Peer{})
				peer = &c.Peers[len(c.Peers)-1]
			default:
				errs = append(errs, fmt.Errorf("line %d: unknown section [%s]", line, section))
			}
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			errs = append(errs, fmt.Errorf("line %d: expected key = value", line))
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		lk := strings.ToLower(k)
		bad := func(what string) { errs = append(errs, fmt.Errorf("line %d: %s: %s", line, k, what)) }
		switch section {
		case "interface":
			switch {
			case lk == "privatekey":
				if router.ValidateKey(v) != nil {
					bad("not a base64 32-byte key")
				}
				c.PrivateKey = v
			case lk == "address":
				for _, a := range splitList(v) {
					p, err := router.CanonicalPrefix(a)
					if err != nil {
						bad(err.Error())
						continue
					}
					c.Addresses = append(c.Addresses, p)
				}
			case lk == "dns":
				c.DNS = append(c.DNS, splitList(v)...)
			case lk == "mtu":
				n, err := strconv.Atoi(v)
				if err != nil {
					bad("not a number")
				}
				c.MTU = n
			case lk == "listenport":
				n, err := strconv.Atoi(v)
				if err != nil {
					bad("not a number")
				}
				c.ListenPort = n
			case ascKey(k) != "":
				c.ASC[ascKey(k)] = v
			case ignored[lk]:
				c.Warnings = append(c.Warnings, k+" is not supported by the router and is ignored")
			default:
				errs = append(errs, fmt.Errorf("line %d: unknown [Interface] key %s", line, k))
			}
		case "peer":
			switch lk {
			case "publickey":
				if router.ValidateKey(v) != nil {
					bad("not a base64 32-byte key")
				}
				peer.PublicKey = v
			case "presharedkey":
				if router.ValidateKey(v) != nil {
					bad("not a base64 32-byte key")
				}
				peer.PresharedKey = v
			case "endpoint":
				peer.Endpoint = v
			case "allowedips":
				for _, a := range splitList(v) {
					p, err := router.CanonicalPrefix(a)
					if err != nil {
						bad(err.Error())
						continue
					}
					peer.AllowedIPs = append(peer.AllowedIPs, p)
				}
			case "persistentkeepalive":
				if v == "off" {
					continue
				}
				n, err := strconv.Atoi(v)
				if err != nil {
					bad("not a number")
				}
				peer.Keepalive = n
			default:
				errs = append(errs, fmt.Errorf("line %d: unknown [Peer] key %s", line, k))
			}
		default:
			errs = append(errs, fmt.Errorf("line %d: %s outside of a section", line, k))
		}
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, err)
	}
	if c.PrivateKey == "" {
		errs = append(errs, errors.New("[Interface] PrivateKey is missing"))
	}
	if c.IPv4Address() == "" {
		errs = append(errs, errors.New("[Interface] Address has no IPv4 address"))
	}
	if len(c.Peers) == 0 {
		errs = append(errs, errors.New("no [Peer] section"))
	}
	for i, p := range c.Peers {
		if p.PublicKey == "" {
			errs = append(errs, fmt.Errorf("peer #%d: PublicKey is missing", i+1))
		}
		if p.Endpoint == "" {
			errs = append(errs, fmt.Errorf("peer #%d: Endpoint is missing", i+1))
		}
	}
	for _, a := range c.Addresses {
		if !strings.Contains(a, ".") {
			c.Warnings = append(c.Warnings, "IPv6 address "+a+" is ignored (the router interface gets the IPv4 address only)")
		}
	}
	for _, p := range c.Peers {
		if len(ipv4Only(p.AllowedIPs)) < len(p.AllowedIPs) {
			c.Warnings = append(c.Warnings, "IPv6 AllowedIPs are ignored (IPv4 only on the router)")
			break
		}
	}
	if len(c.DNS) > 0 {
		c.Warnings = append(c.Warnings, "DNS from the config is ignored: the router keeps its own DNS (domain routing relies on it)")
	}
	for _, k := range []string{"S3", "S4", "I1", "I2", "I3", "I4", "I5"} {
		if v := c.ASC[k]; v != "" && v != "0" {
			c.Warnings = append(c.Warnings, "AWG 2.0 parameters (S3/S4/I1-I5) need KeeneticOS 5.1 or newer")
			break
		}
	}
	return c, errors.Join(errs...)
}

// ascKey canonicalizes an obfuscation key name ("jmin" → "Jmin").
func ascKey(k string) string {
	for _, a := range ascOrder {
		if strings.EqualFold(a, k) {
			return a
		}
	}
	return ""
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// DecodeVPNKey extracts the WireGuard config text from an Amnezia `vpn://`
// key: base64url of a Qt-compressed (4-byte length + zlib) or plain JSON
// document whose AWG container carries `last_config` with a `config` field.
func DecodeVPNKey(key string) (string, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(key, "vpn://"))
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
	if err != nil {
		return "", errors.New("vpn:// key: not base64url")
	}
	if len(data) > 4 && data[0] != '{' {
		zr, err := zlib.NewReader(bytes.NewReader(data[4:]))
		if err != nil {
			return "", errors.New("vpn:// key: not a compressed Amnezia document")
		}
		data, err = io.ReadAll(io.LimitReader(zr, 4<<20))
		if err != nil {
			return "", errors.New("vpn:// key: corrupt compressed data")
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", errors.New("vpn:// key: not a JSON document")
	}
	conf := findConfig(doc)
	if conf == "" {
		return "", errors.New("vpn:// key: no AmneziaWG/WireGuard config inside (only AWG containers can be imported)")
	}
	for k, v := range map[string]string{"$PRIMARY_DNS": str(doc["dns1"]), "$SECONDARY_DNS": str(doc["dns2"])} {
		conf = strings.ReplaceAll(conf, k, v)
	}
	return conf, nil
}

// findConfig looks for containers[].<proto>.last_config.config.
func findConfig(doc map[string]any) string {
	cs, _ := doc["containers"].([]any)
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		for _, proto := range cm {
			pm, ok := proto.(map[string]any)
			if !ok {
				continue
			}
			lc := str(pm["last_config"])
			if lc == "" {
				continue
			}
			var inner map[string]any
			if err := json.Unmarshal([]byte(lc), &inner); err != nil {
				continue
			}
			if conf := str(inner["config"]); strings.Contains(conf, "[Interface]") {
				return conf
			}
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
