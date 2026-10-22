package fake

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
)

// renderInterfaces prints Wireguard interfaces the way KeeneticOS does:
// keys are not shown unless ShowKeys is set.
func (f *Transport) renderInterfaces(b *strings.Builder) {
	for _, w := range f.State.Interfaces {
		fmt.Fprintf(b, "interface %s\n", w.Name)
		if w.Description != "" {
			fmt.Fprintf(b, "    description %s\n", cli.Quote(w.Description))
		}
		if w.SecurityLevel != "" {
			fmt.Fprintf(b, "    security-level %s\n", w.SecurityLevel)
		}
		if w.Address != "" {
			fmt.Fprintf(b, "    ip address %s\n", router.PrefixMask(w.Address))
		}
		if w.MTU != 0 {
			fmt.Fprintf(b, "    ip mtu %d\n", w.MTU)
		}
		if w.Global != "" {
			fmt.Fprintf(b, "    ip global %s\n", w.Global)
		}
		if w.AdjustMSS {
			b.WriteString("    ip tcp adjust-mss pmtu\n")
		}
		if w.ListenPort != 0 {
			fmt.Fprintf(b, "    wireguard listen-port %d\n", w.ListenPort)
		}
		if f.ShowKeys && w.PrivateKey != "" {
			fmt.Fprintf(b, "    wireguard private-key %s\n", w.PrivateKey)
		}
		if len(w.ASC) > 0 {
			fmt.Fprintf(b, "    wireguard asc %s\n", cli.ASCArgs(w.ASC))
		}
		for _, p := range w.Peers {
			fmt.Fprintf(b, "    wireguard peer %s !peer\n", p.PublicKey)
			if p.Endpoint != "" {
				fmt.Fprintf(b, "        endpoint %s\n", p.Endpoint)
			}
			if p.Keepalive != 0 {
				fmt.Fprintf(b, "        keepalive-interval %d\n", p.Keepalive)
			}
			if f.ShowKeys && p.PresharedKey != "" {
				fmt.Fprintf(b, "        preshared-key %s\n", p.PresharedKey)
			}
			for _, a := range p.AllowedIPs {
				fmt.Fprintf(b, "        allow-ips %s\n", router.PrefixMask(a))
			}
			b.WriteString("        connect\n    !\n")
		}
		if w.Up {
			b.WriteString("    up\n")
		}
		b.WriteString("!\n")
	}
}

func (f *Transport) iface(name string) (*router.WGInterface, bool) {
	for i := range f.State.Interfaces {
		if f.State.Interfaces[i].Name == name {
			return &f.State.Interfaces[i], true
		}
	}
	return nil, false
}

// applyInterface interprets `[no] interface N [no] <setting>`.
func (f *Transport) applyInterface(neg bool, name string, rest []string, raw string) error {
	if router.ValidateWGName(name) != nil {
		return &router.CommandError{Command: raw, Message: "fake: only Wireguard interfaces are supported"}
	}
	w, ok := f.iface(name)
	if len(rest) == 0 {
		switch {
		case neg && !ok:
			return &router.CommandError{Command: raw, Message: "no such interface"}
		case neg:
			kept := f.State.Interfaces[:0]
			for _, x := range f.State.Interfaces {
				if x.Name != name {
					kept = append(kept, x)
				}
			}
			f.State.Interfaces = kept
		case !ok:
			f.State.Interfaces = append(f.State.Interfaces, router.WGInterface{Name: name})
		}
		return nil
	}
	if neg || !ok {
		return &router.CommandError{Command: raw, Message: "no such interface"}
	}
	unset := false
	if rest[0] == "no" {
		unset, rest = true, rest[1:]
	}
	bad := &router.CommandError{Command: raw, Message: "fake: unknown interface setting"}
	if len(rest) == 0 {
		return bad
	}
	arg := strings.Join(rest[1:], " ")
	switch rest[0] {
	case "description":
		w.Description = arg
		if unset {
			w.Description = ""
		}
	case "security-level":
		w.SecurityLevel = arg
	case "up":
		w.Up = true
	case "down":
		w.Up = false
	case "ip":
		if len(rest) < 2 {
			return bad
		}
		val := strings.Join(rest[2:], " ")
		switch rest[1] {
		case "address":
			p, err := router.CanonicalPrefix(val)
			if err != nil {
				return &router.CommandError{Command: raw, Message: err.Error()}
			}
			w.Address = p
		case "mtu":
			w.MTU, _ = strconv.Atoi(val)
		case "global":
			w.Global = val
			if unset {
				w.Global = ""
			}
		case "tcp":
			w.AdjustMSS = !unset
		default:
			return bad
		}
	case "wireguard":
		if len(rest) < 2 {
			return bad
		}
		switch rest[1] {
		case "listen-port":
			w.ListenPort, _ = strconv.Atoi(strings.Join(rest[2:], ""))
		case "private-key":
			if len(rest) < 3 || router.ValidateKey(rest[2]) != nil {
				return &router.CommandError{Command: raw, Message: "invalid key"}
			}
			w.PrivateKey = rest[2]
		case "asc":
			w.ASC = nil
			if !unset {
				w.ASC = cli.TrimASC(append([]string(nil), rest[2:]...))
			}
		case "peer":
			if len(rest) < 3 {
				return bad
			}
			return f.applyPeer(w, unset, rest[2], rest[3:], raw)
		default:
			return bad
		}
	default:
		return bad
	}
	return nil
}

func (f *Transport) applyPeer(w *router.WGInterface, del bool, key string, rest []string, raw string) error {
	idx := -1
	for i, p := range w.Peers {
		if p.PublicKey == key {
			idx = i
		}
	}
	if del {
		if idx < 0 {
			return &router.CommandError{Command: raw, Message: "no such peer"}
		}
		w.Peers = append(w.Peers[:idx], w.Peers[idx+1:]...)
		return nil
	}
	if idx < 0 {
		if router.ValidateKey(key) != nil {
			return &router.CommandError{Command: raw, Message: "invalid key"}
		}
		w.Peers = append(w.Peers, router.WGPeer{PublicKey: key})
		idx = len(w.Peers) - 1
	}
	if len(rest) == 0 {
		return nil
	}
	p := &w.Peers[idx]
	unset := false
	if rest[0] == "no" {
		unset, rest = true, rest[1:]
	}
	if len(rest) == 0 {
		return &router.CommandError{Command: raw, Message: "fake: missing peer setting"}
	}
	val := strings.Join(rest[1:], " ")
	switch rest[0] {
	case "endpoint":
		p.Endpoint = val
		if unset {
			p.Endpoint = ""
		}
	case "keepalive-interval":
		p.Keepalive, _ = strconv.Atoi(val)
		if unset {
			p.Keepalive = 0
		}
	case "preshared-key":
		p.PresharedKey = val
		if unset {
			p.PresharedKey = ""
		}
	case "allow-ips":
		c, err := router.CanonicalPrefix(val)
		if err != nil {
			return &router.CommandError{Command: raw, Message: err.Error()}
		}
		p.AllowedIPs = without(p.AllowedIPs, c)
		if !unset {
			p.AllowedIPs = append(p.AllowedIPs, c)
		}
	default:
		return &router.CommandError{Command: raw, Message: "fake: unknown peer setting"}
	}
	return nil
}

// InterfaceStatus returns Status[name]; an interface present in the state
// but without a configured status is reported up with no handshake.
func (f *Transport) InterfaceStatus(_ context.Context, name string) (*router.WGStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.Status[name]; ok {
		c := *s
		return &c, nil
	}
	w, ok := f.iface(name)
	if !ok {
		return nil, &router.CommandError{Command: cli.ShowInterface(name), Message: "unable to find " + name}
	}
	s := &router.WGStatus{Name: name, Exists: true, Up: w.Up}
	for _, p := range w.Peers {
		s.Peers = append(s.Peers, router.WGPeerStatus{PublicKey: p.PublicKey, LastHandshake: -1})
	}
	return s, nil
}
