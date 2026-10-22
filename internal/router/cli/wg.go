package cli

import (
	"slices"
	"strconv"
	"strings"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// Wireguard interface commands. Like the object-group commands they use the
// one-line form from the root context; negation is placed before the leaf
// keyword (`interface Wireguard0 wireguard peer K no allow-ips …`), the form
// the KeeneticOS console documents for nested settings.

// IfCreate creates (or enters) the interface.
func IfCreate(name string) string { return "interface " + name }

// IfDelete removes the interface with all its settings.
func IfDelete(name string) string { return "no interface " + name }

// IfSet sets a setting inside the interface.
func IfSet(name, rest string) string { return "interface " + name + " " + rest }

// IfNo removes a setting inside the interface.
func IfNo(name, rest string) string { return "interface " + name + " no " + rest }

// PeerSet sets a setting of a peer (creating the peer when rest is "").
func PeerSet(name, key, rest string) string {
	s := "interface " + name + " wireguard peer " + key
	if rest != "" {
		s += " " + rest
	}
	return s
}

// PeerNo removes a setting of a peer.
func PeerNo(name, key, rest string) string {
	return "interface " + name + " wireguard peer " + key + " no " + rest
}

// PeerDelete removes a peer.
func PeerDelete(name, key string) string { return IfNo(name, "wireguard peer "+key) }

// ShowInterface is the status command for one interface.
func ShowInterface(name string) string { return "show interface " + name }

// ASCArgs renders `wireguard asc` values, quoting empty and spaced ones.
func ASCArgs(v []string) string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = Quote(s)
	}
	return strings.Join(out, " ")
}

// TrimASC drops the AWG 2.0 tail (S3 S4 I1..I5) when it is all defaults, so
// that 9- and 16-value forms of the same settings compare equal.
func TrimASC(v []string) []string {
	if len(v) != 16 {
		return v
	}
	for i, s := range v[9:] {
		if (i < 2 && s != "0" && s != "") || (i >= 2 && s != "") {
			return v
		}
	}
	return v[:9]
}

// applyIfaceLine applies one indented line of an interface section. It
// returns true when the line opened a `wireguard peer` subsection.
func applyIfaceLine(w *router.WGInterface, f []string) bool {
	switch {
	case f[0] == "description" && len(f) > 1:
		w.Description = strings.Join(f[1:], " ")
	case f[0] == "security-level" && len(f) > 1:
		w.SecurityLevel = f[1]
	case f[0] == "up":
		w.Up = true
	case f[0] == "down":
		w.Up = false
	case f[0] == "ip" && len(f) > 2 && f[1] == "address":
		if p, err := router.CanonicalPrefix(strings.Join(f[2:min(len(f), 4)], " ")); err == nil {
			w.Address = p
		}
	case f[0] == "ip" && len(f) > 2 && f[1] == "mtu":
		w.MTU, _ = strconv.Atoi(f[2])
	case f[0] == "ip" && len(f) > 2 && f[1] == "global":
		w.Global = f[2]
	case f[0] == "ip" && len(f) > 3 && f[1] == "tcp" && f[2] == "adjust-mss":
		w.AdjustMSS = true
	case f[0] == "wireguard" && len(f) > 2:
		switch f[1] {
		case "listen-port":
			w.ListenPort, _ = strconv.Atoi(f[2])
		case "private-key":
			w.PrivateKey = f[2]
		case "asc":
			w.ASC = TrimASC(append([]string(nil), f[2:]...))
		case "peer":
			w.Peers = append(w.Peers, router.WGPeer{PublicKey: f[2]})
			rest := f[3:]
			if len(rest) > 0 && strings.HasPrefix(rest[0], "!") {
				rest = nil // `wireguard peer KEY !name`
			}
			if len(rest) > 0 {
				applyPeerLine(&w.Peers[len(w.Peers)-1], rest)
				return false
			}
			return true
		}
	}
	return false
}

// applyPeerLine applies one line of a `wireguard peer` subsection.
func applyPeerLine(p *router.WGPeer, f []string) {
	if len(f) < 2 {
		return
	}
	switch f[0] {
	case "endpoint":
		p.Endpoint = f[1]
	case "keepalive-interval":
		p.Keepalive, _ = strconv.Atoi(f[1])
	case "preshared-key":
		p.PresharedKey = f[1]
	case "allow-ips":
		if c, err := router.CanonicalPrefix(strings.Join(f[1:min(len(f), 3)], " ")); err == nil {
			p.AllowedIPs = append(p.AllowedIPs, c)
		}
	}
}

// diffInterfaces adds the commands that bring each desired interface to its
// desired settings. Interfaces not listed are not touched.
func diffInterfaces(p *router.Plan, cur *router.State, want *router.Desired) {
	for _, w := range want.Interfaces {
		n := w.Name
		c, exists := cur.Interface(n)
		if !exists {
			p.AddInterface(n, "+ interface "+n, IfCreate(n))
		}
		if w.Description != c.Description {
			if w.Description == "" {
				p.AddInterface(n, "- description", IfNo(n, "description"))
			} else {
				p.AddInterface(n, "~ description "+Quote(w.Description), IfSet(n, "description "+Quote(w.Description)))
			}
		}
		if w.SecurityLevel != "" && w.SecurityLevel != c.SecurityLevel {
			p.AddInterface(n, "~ security-level "+w.SecurityLevel, IfSet(n, "security-level "+w.SecurityLevel))
		}
		if w.Address != "" && w.Address != c.Address {
			p.AddInterface(n, "~ ip address "+w.Address, IfSet(n, "ip address "+router.PrefixMask(w.Address)))
		}
		if w.MTU != 0 && w.MTU != c.MTU {
			p.AddInterface(n, "~ ip mtu "+strconv.Itoa(w.MTU), IfSet(n, "ip mtu "+strconv.Itoa(w.MTU)))
		}
		switch {
		case w.Global == "":
		case w.Global == "off":
			if c.Global != "" {
				p.AddInterface(n, "- ip global", IfNo(n, "ip global"))
			}
		case w.Global != c.Global:
			p.AddInterface(n, "~ ip global "+w.Global, IfSet(n, "ip global "+w.Global))
		}
		if w.AdjustMSS != c.AdjustMSS {
			if w.AdjustMSS {
				p.AddInterface(n, "+ ip tcp adjust-mss pmtu", IfSet(n, "ip tcp adjust-mss pmtu"))
			} else {
				p.AddInterface(n, "- ip tcp adjust-mss", IfNo(n, "ip tcp adjust-mss"))
			}
		}
		if w.ListenPort != 0 && w.ListenPort != c.ListenPort {
			p.AddInterface(n, "~ listen-port "+strconv.Itoa(w.ListenPort), IfSet(n, "wireguard listen-port "+strconv.Itoa(w.ListenPort)))
		}
		if w.PrivateKey != "" && (!exists || w.SetKeys || (c.PrivateKey != "" && c.PrivateKey != w.PrivateKey)) {
			p.AddSecret(n, "~ private-key <secret>", w.PrivateKey, IfSet(n, "wireguard private-key "+w.PrivateKey))
		}
		want, have := TrimASC(w.ASC), TrimASC(c.ASC)
		if !slices.Equal(want, have) {
			if len(want) == 0 {
				p.AddInterface(n, "- asc", IfNo(n, "wireguard asc"))
			} else {
				p.AddInterface(n, "~ asc "+ASCArgs(want), IfSet(n, "wireguard asc "+ASCArgs(want)))
			}
		}
		diffPeers(p, n, exists, c, w)
		if w.Up != c.Up || !exists {
			if w.Up {
				p.AddInterface(n, "+ up", IfSet(n, "up"))
			} else if exists {
				p.AddInterface(n, "- down", IfSet(n, "down"))
			}
		}
	}
}

func diffPeers(p *router.Plan, n string, exists bool, c, w router.WGInterface) {
	for _, wp := range w.Peers {
		k := wp.PublicKey
		cp, ok := c.Peer(k)
		short := k[:min(len(k), 8)] + "…"
		if !ok {
			p.AddInterface(n, "+ peer "+short, PeerSet(n, k, ""))
		}
		if wp.Endpoint != cp.Endpoint {
			if wp.Endpoint == "" {
				p.AddInterface(n, "- peer "+short+" endpoint", PeerNo(n, k, "endpoint"))
			} else {
				p.AddInterface(n, "~ peer "+short+" endpoint "+wp.Endpoint, PeerSet(n, k, "endpoint "+wp.Endpoint))
			}
		}
		if wp.Keepalive != cp.Keepalive {
			if wp.Keepalive == 0 {
				p.AddInterface(n, "- peer "+short+" keepalive", PeerNo(n, k, "keepalive-interval"))
			} else {
				ka := strconv.Itoa(wp.Keepalive)
				p.AddInterface(n, "~ peer "+short+" keepalive "+ka, PeerSet(n, k, "keepalive-interval "+ka))
			}
		}
		switch {
		case wp.PresharedKey != "" && (!ok || !exists || w.SetKeys || (cp.PresharedKey != "" && cp.PresharedKey != wp.PresharedKey)):
			p.AddSecret(n, "~ peer "+short+" preshared-key <secret>", wp.PresharedKey, PeerSet(n, k, "preshared-key "+wp.PresharedKey))
		case wp.PresharedKey == "" && cp.PresharedKey != "":
			p.AddInterface(n, "- peer "+short+" preshared-key", PeerNo(n, k, "preshared-key"))
		}
		have := map[string]bool{}
		for _, a := range cp.AllowedIPs {
			have[a] = true
		}
		keep := map[string]bool{}
		for _, a := range wp.AllowedIPs {
			keep[a] = true
			if !have[a] {
				p.AddInterface(n, "+ peer "+short+" allow-ips "+a, PeerSet(n, k, "allow-ips "+router.PrefixMask(a)))
			}
		}
		for _, a := range cp.AllowedIPs {
			if !keep[a] {
				p.AddInterface(n, "- peer "+short+" allow-ips "+a, PeerNo(n, k, "allow-ips "+router.PrefixMask(a)))
			}
		}
	}
	for _, cp := range c.Peers {
		if _, ok := w.Peer(cp.PublicKey); !ok {
			k := cp.PublicKey
			p.AddInterface(n, "- peer "+k[:min(len(k), 8)]+"…", PeerDelete(n, k))
		}
	}
}

// removeInterfaces deletes the listed interfaces that exist.
func removeInterfaces(p *router.Plan, cur *router.State, names []string) {
	for _, n := range names {
		if c, ok := cur.Interface(n); ok {
			p.AddInterface(c.Name, "- interface "+c.Name, IfDelete(c.Name))
		}
	}
}
