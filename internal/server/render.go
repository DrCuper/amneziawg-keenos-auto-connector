package server

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// header marks files written by awgkeen.
const header = "# Managed by awgkeen: changes made by hand are overwritten on deploy.\n"

// ServerConf renders the awg-quick configuration of the server. natIface is
// the uplink for MASQUERADE.
func ServerConf(c Config, s *State, natIface string) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", s.Keys.Private)
	fmt.Fprintf(&b, "Address = %s\n", s.Address)
	fmt.Fprintf(&b, "ListenPort = %d\n", c.ListenPort)
	for _, l := range s.Params.Lines() {
		b.WriteString(l + "\n")
	}
	if natIface != "" {
		fmt.Fprintf(&b, "PostUp = iptables -t nat -A POSTROUTING -s %s -o %s -j MASQUERADE; iptables -A FORWARD -i %%i -j ACCEPT; iptables -A FORWARD -o %%i -j ACCEPT\n", c.Subnet, natIface)
		fmt.Fprintf(&b, "PostDown = iptables -t nat -D POSTROUTING -s %s -o %s -j MASQUERADE; iptables -D FORWARD -i %%i -j ACCEPT; iptables -D FORWARD -o %%i -j ACCEPT\n", c.Subnet, natIface)
	}
	for _, p := range s.Peers {
		if p.Disabled {
			fmt.Fprintf(&b, "\n# peer %s is disabled\n", p.Name)
			continue
		}
		fmt.Fprintf(&b, "\n[Peer]\n# %s\nPublicKey = %s\nPresharedKey = %s\nAllowedIPs = %s\n", p.Name, p.Keys.Public, p.PSK, p.Address)
	}
	return b.String()
}

// interfaceSection returns the [Interface] part (for restart decisions).
func interfaceSection(conf string) string {
	i := strings.Index(conf, "\n[Peer]")
	j := strings.Index(conf, "\n# peer ")
	if j >= 0 && (i < 0 || j < i) {
		i = j
	}
	if i < 0 {
		return conf
	}
	return conf[:i]
}

// Hash fingerprints a configuration text.
func Hash(conf string) string {
	s := sha256.Sum256([]byte(conf))
	return hex.EncodeToString(s[:])
}

// ClientConf renders the client configuration of a peer (AmneziaWG .conf).
func ClientConf(c Config, s *State, p *Peer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s @ %s (awgkeen)\n[Interface]\n", p.Name, c.Name)
	fmt.Fprintf(&b, "PrivateKey = %s\n", p.Keys.Private)
	fmt.Fprintf(&b, "Address = %s\n", p.Address)
	if len(c.DNS) > 0 {
		fmt.Fprintf(&b, "DNS = %s\n", strings.Join(c.DNS, ", "))
	}
	fmt.Fprintf(&b, "MTU = %d\n", c.MTU)
	for _, l := range s.Params.Lines() {
		b.WriteString(l + "\n")
	}
	fmt.Fprintf(&b, "\n[Peer]\nPublicKey = %s\nPresharedKey = %s\n", s.Keys.Public, p.PSK)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(c.AllowedIPs, ", "))
	fmt.Fprintf(&b, "Endpoint = %s\n", c.ClientEndpoint())
	fmt.Fprintf(&b, "PersistentKeepalive = %d\n", c.Keepalive)
	return b.String()
}

// QRPNG renders the client configuration as a QR code PNG (scannable by the
// AmneziaWG and WireGuard mobile apps).
func QRPNG(conf string, size int) ([]byte, error) {
	return qrcode.Encode(conf, qrcode.Low, size)
}

// QRText renders the QR code for a terminal.
func QRText(conf string) (string, error) {
	q, err := qrcode.New(conf, qrcode.Low)
	if err != nil {
		return "", err
	}
	return q.ToSmallString(false), nil
}

// VPNKey renders the Amnezia `vpn://` share key (AmneziaVPN app import):
// base64url of a Qt-compressed JSON document with one AWG container.
func VPNKey(c Config, s *State, p *Peer) (string, error) {
	host, port := c.ClientEndpoint(), strconv.Itoa(c.ListenPort)
	if h, pt, ok := strings.Cut(host, ":"); ok && !strings.Contains(pt, ":") {
		host, port = h, pt
	}
	conf := ClientConf(c, s, p)
	ip := p.Address
	if a, err := netip.ParsePrefix(p.Address); err == nil {
		ip = a.Addr().String()
	}
	last := map[string]any{
		"config": conf, "hostName": host, "port": atoi(port), "client_ip": ip,
		"client_priv_key": p.Keys.Private, "client_pub_key": p.Keys.Public, "psk_key": p.PSK,
		"server_pub_key": s.Keys.Public, "mtu": strconv.Itoa(c.MTU),
		"Jc": strconv.Itoa(s.Params.Jc), "Jmin": strconv.Itoa(s.Params.Jmin), "Jmax": strconv.Itoa(s.Params.Jmax),
		"S1": strconv.Itoa(s.Params.S1), "S2": strconv.Itoa(s.Params.S2),
		"H1": s.Params.H1, "H2": s.Params.H2, "H3": s.Params.H3, "H4": s.Params.H4,
	}
	lastJSON, err := json.Marshal(last)
	if err != nil {
		return "", err
	}
	awg := map[string]any{
		"last_config": string(lastJSON), "port": port, "transport_proto": "udp",
		"Jc": strconv.Itoa(s.Params.Jc), "Jmin": strconv.Itoa(s.Params.Jmin), "Jmax": strconv.Itoa(s.Params.Jmax),
		"S1": strconv.Itoa(s.Params.S1), "S2": strconv.Itoa(s.Params.S2),
		"H1": s.Params.H1, "H2": s.Params.H2, "H3": s.Params.H3, "H4": s.Params.H4,
	}
	doc := map[string]any{
		"containers":       []any{map[string]any{"container": "amnezia-awg", "awg": awg}},
		"defaultContainer": "amnezia-awg",
		"description":      c.Name + " / " + p.Name,
		"hostName":         host,
	}
	if len(c.DNS) > 0 {
		doc["dns1"] = c.DNS[0]
	}
	if len(c.DNS) > 1 {
		doc["dns2"] = c.DNS[1]
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(raw))) //nolint:gosec // a JSON document of a few KiB
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return "vpn://" + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
