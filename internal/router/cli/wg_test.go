package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

const (
	keyA = "YUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=" // 32 x 'a'... base64
	keyB = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="
	keyP = "UFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFA="
)

const wgConfig = `interface Wireguard0
    description "AWG main"
    security-level public
    ip address 10.8.1.2 255.255.255.255
    ip mtu 1376
    ip global 32766
    ip tcp adjust-mss pmtu
    wireguard listen-port 51820
    wireguard asc 4 40 70 0 0 1 2 3 4
    wireguard peer ` + keyB + ` !server
        endpoint 203.0.113.1:51820
        keepalive-interval 25
        allow-ips 0.0.0.0 0.0.0.0
        allow-ips 10.0.0.0 255.0.0.0
        connect
    !
    up
!
interface GigabitEthernet0
    up
`

func TestParseWireguard(t *testing.T) {
	st := Parse(wgConfig + sampleConfig)
	if len(st.Interfaces) != 2 || st.Interfaces[1].Name != "Wireguard2" {
		t.Fatalf("interfaces: %+v", st.Interfaces)
	}
	want := router.WGInterface{
		Name: "Wireguard0", Description: "AWG main", SecurityLevel: "public", Address: "10.8.1.2/32",
		MTU: 1376, Global: "32766", AdjustMSS: true, ListenPort: 51820,
		ASC: []string{"4", "40", "70", "0", "0", "1", "2", "3", "4"},
		Peers: []router.WGPeer{{PublicKey: keyB, Endpoint: "203.0.113.1:51820", Keepalive: 25,
			AllowedIPs: []string{"0.0.0.0/0", "10.0.0.0/8"}}},
		Up: true,
	}
	if !reflect.DeepEqual(st.Interfaces[0], want) {
		t.Errorf("got  %+v\nwant %+v", st.Interfaces[0], want)
	}
	if len(st.Groups) != 3 {
		t.Errorf("groups after interface section: %d", len(st.Groups))
	}
}

func TestParseASC20(t *testing.T) {
	st := Parse("interface Wireguard3\n    wireguard asc 3 20 60 15 40 100-200 300-400 500-600 700-800 0 0 \"<b 0x1234>\" \"\" \"\" \"\" \"\"\n")
	got := st.Interfaces[0].ASC
	if len(got) != 16 || got[5] != "100-200" || got[11] != "<b 0x1234>" || got[12] != "" {
		t.Errorf("asc %q", got)
	}
	// all-default tail trims to 9 values
	st = Parse("interface Wireguard3\n    wireguard asc 3 20 60 15 40 1 2 3 4 0 0 \"\" \"\" \"\" \"\" \"\"\n")
	if len(st.Interfaces[0].ASC) != 9 {
		t.Errorf("asc %q", st.Interfaces[0].ASC)
	}
}

func desiredWG(w router.WGInterface) *router.Desired {
	d := &router.Desired{Interfaces: []router.WGInterface{w}}
	if err := d.Normalize(router.Limits{}); err != nil {
		panic(err)
	}
	return d
}

func newIface() router.WGInterface {
	return router.WGInterface{
		Name: "Wireguard1", Description: "awgkeen: main", SecurityLevel: "public", Address: "10.8.1.2/32",
		MTU: 1376, AdjustMSS: true, ASC: []string{"4", "40", "70", "0", "0", "1", "2", "3", "4"},
		PrivateKey: keyA, Up: true,
		Peers: []router.WGPeer{{PublicKey: keyB, PresharedKey: keyP, Endpoint: "vpn.example.com:443", Keepalive: 25, AllowedIPs: []string{"0.0.0.0/0"}}},
	}
}

func TestDiffNewInterface(t *testing.T) {
	p := Diff(Parse(""), desiredWG(newIface()))
	want := []string{
		"interface Wireguard1",
		`interface Wireguard1 description "awgkeen: main"`,
		"interface Wireguard1 security-level public",
		"interface Wireguard1 ip address 10.8.1.2 255.255.255.255",
		"interface Wireguard1 ip mtu 1376",
		"interface Wireguard1 ip tcp adjust-mss pmtu",
		"interface Wireguard1 wireguard private-key " + keyA,
		"interface Wireguard1 wireguard asc 4 40 70 0 0 1 2 3 4",
		"interface Wireguard1 wireguard peer " + keyB,
		"interface Wireguard1 wireguard peer " + keyB + " endpoint vpn.example.com:443",
		"interface Wireguard1 wireguard peer " + keyB + " keepalive-interval 25",
		"interface Wireguard1 wireguard peer " + keyB + " preshared-key " + keyP,
		"interface Wireguard1 wireguard peer " + keyB + " allow-ips 0.0.0.0 0.0.0.0",
		"interface Wireguard1 up",
	}
	if !reflect.DeepEqual(p.Commands, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(p.Commands, "\n"), strings.Join(want, "\n"))
	}
	out := p.String()
	if strings.Contains(out, keyA) || strings.Contains(out, keyP) {
		t.Errorf("plan output leaks a key:\n%s", out)
	}
	if !strings.Contains(out, "interface Wireguard1:") || !strings.Contains(out, "<secret>") {
		t.Errorf("plan output:\n%s", out)
	}
}

func TestDiffExistingInterface(t *testing.T) {
	cur := Parse(wgConfig)
	w := router.WGInterface{
		Name: "Wireguard0", Description: "AWG main", Address: "10.8.1.2/32", MTU: 1376, AdjustMSS: true,
		ASC: []string{"5", "40", "70", "0", "0", "1", "2", "3", "4"}, PrivateKey: keyA, Up: true,
		Peers: []router.WGPeer{{PublicKey: keyB, Endpoint: "203.0.113.1:51820", Keepalive: 25, AllowedIPs: []string{"0.0.0.0/0", "192.168.0.0/16"}}},
	}
	p := Diff(cur, desiredWG(w))
	want := []string{
		"interface Wireguard0 wireguard asc 5 40 70 0 0 1 2 3 4",
		"interface Wireguard0 wireguard peer " + keyB + " allow-ips 192.168.0.0 255.255.0.0",
		"interface Wireguard0 wireguard peer " + keyB + " no allow-ips 10.0.0.0 255.0.0.0",
	}
	if !reflect.DeepEqual(p.Commands, want) {
		t.Errorf("got\n%s", strings.Join(p.Commands, "\n"))
	}
	// keys are sent only when asked
	w.SetKeys = true
	p = Diff(cur, desiredWG(w))
	if p.Commands[0] != "interface Wireguard0 wireguard private-key "+keyA || !p.Changes[0].WriteOnly {
		t.Errorf("got %q", p.Commands)
	}
}

func TestDiffPeerReplaceAndRemove(t *testing.T) {
	cur := Parse(wgConfig)
	w := router.WGInterface{Name: "Wireguard0", Description: "AWG main", Address: "10.8.1.2/32", AdjustMSS: true,
		Global: "off", Up: false,
		Peers: []router.WGPeer{{PublicKey: keyA, Endpoint: "198.51.100.1:1", AllowedIPs: []string{"0.0.0.0/0"}}}}
	p := Diff(cur, desiredWG(w))
	want := []string{
		"interface Wireguard0 no ip global",
		"interface Wireguard0 no wireguard asc",
		"interface Wireguard0 wireguard peer " + keyA,
		"interface Wireguard0 wireguard peer " + keyA + " endpoint 198.51.100.1:1",
		"interface Wireguard0 wireguard peer " + keyA + " allow-ips 0.0.0.0 0.0.0.0",
		"interface Wireguard0 no wireguard peer " + keyB,
		"interface Wireguard0 down",
	}
	if !reflect.DeepEqual(p.Commands, want) {
		t.Errorf("got\n%s", strings.Join(p.Commands, "\n"))
	}
}

func TestDiffRemoveInterface(t *testing.T) {
	d := &router.Desired{RemoveInterfaces: []string{"Wireguard0", "Wireguard9"}}
	p := Diff(Parse(wgConfig), d)
	if !reflect.DeepEqual(p.Commands, []string{"no interface Wireguard0"}) {
		t.Errorf("got %q", p.Commands)
	}
}

func TestDiffGroupsIgnoreInterfaces(t *testing.T) {
	p := Diff(Parse(wgConfig), &router.Desired{})
	if !p.Empty() {
		t.Errorf("unlisted interfaces must not be touched: %q", p.Commands)
	}
}

func TestParseShowText(t *testing.T) {
	text := `
               id: Wireguard0
            index: 0
             type: Wireguard
      description: AWG
             link: up
        connected: yes
            state: up
        wireguard: 
              public-key: ` + keyA + `
             listen-port: 51820
                    peer: 
                     public-key: ` + keyB + `
                         remote: 203.0.113.1
                    remote-port: 51820
                        rxbytes: 1024
                        txbytes: 2048
                 last-handshake: 42
                         online: yes
`
	tree := ParseShow(text)
	s := router.ParseWGStatus("Wireguard0", tree)
	if !s.Exists || !s.Up || !s.Connected || len(s.Peers) != 1 {
		t.Fatalf("status %+v (tree %v)", s, tree)
	}
	p := s.Peers[0]
	if p.PublicKey != keyB || p.Endpoint != "203.0.113.1:51820" || p.RxBytes != 1024 || p.LastHandshake.Seconds() != 42 || !p.Online {
		t.Errorf("peer %+v", p)
	}
}

func TestParseShowTwoPeers(t *testing.T) {
	text := `
         id: Wireguard1
      state: up
  wireguard:
             peer:
                    public-key: a
                last-handshake: 2147483647
             peer:
                    public-key: b
                last-handshake: 5
`
	s := router.ParseWGStatus("Wireguard1", ParseShow(text))
	if len(s.Peers) != 2 || s.Peers[0].LastHandshake >= 0 || s.Peers[1].LastHandshake.Seconds() != 5 {
		t.Errorf("peers %+v", s.Peers)
	}
}
