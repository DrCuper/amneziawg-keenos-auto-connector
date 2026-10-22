package awg

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
)

const (
	priv  = "YUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="
	priv2 = "Y0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M="
	pub   = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="
	psk   = "UFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFA="
)

var sampleConf = `[Interface]
PrivateKey = ` + priv + `
Address = 10.8.1.2/32, fd00::2/128
DNS = 1.1.1.1, 1.0.0.1
MTU = 1376
Jc = 4
Jmin = 40
Jmax = 70
S1 = 0
S2 = 0
H1 = 1
H2 = 2
H3 = 3
H4 = 4

[Peer]
PublicKey = ` + pub + `
PresharedKey = ` + psk + `
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = vpn.example.com:51820
PersistentKeepalive = 25
`

func TestParseConf(t *testing.T) {
	c, err := Parse([]byte(sampleConf))
	if err != nil {
		t.Fatal(err)
	}
	if c.IPv4Address() != "10.8.1.2/32" || c.MTU != 1376 || len(c.Peers) != 1 {
		t.Fatalf("conf %+v", c)
	}
	if got := strings.Join(c.ASCValues(), " "); got != "4 40 70 0 0 1 2 3 4" {
		t.Errorf("asc %q", got)
	}
	p := c.Peers[0]
	if p.PublicKey != pub || p.PresharedKey != psk || p.Keepalive != 25 || len(p.AllowedIPs) != 2 {
		t.Errorf("peer %+v", p)
	}
	w := strings.Join(c.Warnings, "\n")
	for _, s := range []string{"IPv6 address", "DNS", "IPv6 AllowedIPs"} {
		if !strings.Contains(w, s) {
			t.Errorf("missing warning %q in %q", s, w)
		}
	}
	if k, err := c.PublicKey(); err != nil || len(k) != 44 {
		t.Errorf("public key %q %v", k, err)
	}
}

func TestParseASC20AndPlain(t *testing.T) {
	conf := strings.Replace(sampleConf, "H4 = 4\n", "H4 = 700-800\nS3 = 10\nI1 = <b 0x1234>\n", 1)
	c, err := Parse([]byte(conf))
	if err != nil {
		t.Fatal(err)
	}
	v := c.ASCValues()
	if len(v) != 16 || v[8] != "700-800" || v[9] != "10" || v[10] != "0" || v[11] != "<b 0x1234>" || v[15] != "" {
		t.Errorf("asc %q", v)
	}
	plain := "[Interface]\nPrivateKey = " + priv + "\nAddress = 10.0.0.2/32\n[Peer]\nPublicKey = " + pub + "\nEndpoint = h:1\nAllowedIPs = 0.0.0.0/0\n"
	c, err = Parse([]byte(plain))
	if err != nil || c.ASCValues() != nil {
		t.Errorf("plain: %v %v", err, c.ASCValues())
	}
}

func TestParseErrorsDoNotLeakKeys(t *testing.T) {
	bad := "[Interface]\nPrivateKey = c2VjcmV0LWJ1dC10b28tc2hvcnQ=\nAddress = nope\nFoo = 1\n[Peer]\nPresharedKey = c2VjcmV0LXBzaw==\n"
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, s := range []string{"c2VjcmV0", "PrivateKey: not a base64", "unknown [Interface] key Foo", "PublicKey is missing", "Endpoint is missing"} {
		has := strings.Contains(msg, s)
		if (s == "c2VjcmV0") == has {
			t.Errorf("message %q: contains %q = %v", msg, s, has)
		}
	}
}

func vpnKey(t *testing.T, conf string, compress bool) string {
	t.Helper()
	inner, _ := json.Marshal(map[string]string{"config": conf})
	doc, _ := json.Marshal(map[string]any{
		"containers": []any{map[string]any{"container": "amnezia-awg", "awg": map[string]any{"last_config": string(inner)}}},
		"dns1":       "1.1.1.1",
	})
	data := doc
	if compress {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(len(doc)))
		zw := zlib.NewWriter(&b)
		_, _ = zw.Write(doc)
		_ = zw.Close()
		data = b.Bytes()
	}
	return "vpn://" + base64.RawURLEncoding.EncodeToString(data)
}

func TestParseVPNKey(t *testing.T) {
	for _, compress := range []bool{true, false} {
		c, err := Parse([]byte(vpnKey(t, strings.Replace(sampleConf, "DNS = 1.1.1.1, 1.0.0.1", "DNS = $PRIMARY_DNS", 1), compress)))
		if err != nil {
			t.Fatalf("compress=%v: %v", compress, err)
		}
		if c.PrivateKey != priv || len(c.DNS) != 1 || c.DNS[0] != "1.1.1.1" {
			t.Errorf("conf %+v", c)
		}
	}
	if _, err := Parse([]byte("vpn://!!!")); err == nil {
		t.Error("garbage accepted")
	}
}

type env struct {
	f   *fake.Transport
	m   *Manager
	dir string
}

func newEnv(t *testing.T, running string, tunnels ...Tunnel) *env {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.conf"), []byte(sampleConf), 0o600); err != nil {
		t.Fatal(err)
	}
	f := fake.New(running)
	c := router.NewClient(f, cli.Planner{}, router.Options{Timeout: time.Second, SaveConfig: true})
	if len(tunnels) == 0 {
		tunnels = []Tunnel{{Name: "main", ConfFile: "main.conf"}}
	}
	return &env{f: f, dir: dir, m: &Manager{Router: c, Tunnels: tunnels, BaseDir: dir, StatePath: filepath.Join(dir, "state", "awg.json")}}
}

func TestManagerApplyAuto(t *testing.T) {
	e := newEnv(t, "interface Wireguard0\n    up\n")
	ctx := context.Background()

	res, err := e.m.Apply(ctx, nil, ApplyOptions{DryRun: true})
	if err != nil || !res.DryRun || len(e.f.Commands) != 0 {
		t.Fatalf("dry-run: %+v %v %q", res, err, e.f.Commands)
	}
	if _, err := os.Stat(e.m.StatePath); err == nil {
		t.Error("dry-run wrote state")
	}

	res, err = e.m.Apply(ctx, nil, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if it := res.Items[0]; it.Interface != "Wireguard1" || !it.Created {
		t.Errorf("item %+v", it)
	}
	w, ok := e.f.State.Interface("Wireguard1")
	if !ok || w.PrivateKey != priv || w.Peers[0].PresharedKey != psk || !w.Up || w.Description != "awgkeen: main" {
		t.Fatalf("router interface %+v", w)
	}
	if got := w.Peers[0].AllowedIPs; len(got) != 1 || got[0] != "0.0.0.0/0" {
		t.Errorf("allowed ips %q", got)
	}
	if iface, err := e.m.Resolve("main"); err != nil || iface != "Wireguard1" {
		t.Errorf("resolve %q %v", iface, err)
	}

	// Unchanged: nothing sent.
	n := len(e.f.Commands)
	res, err = e.m.Apply(ctx, nil, ApplyOptions{})
	if err != nil || res.Apply.Applied != 0 || len(e.f.Commands) != n {
		t.Errorf("second apply %+v %v", res.Apply, err)
	}

	// New private key: only the key is sent.
	conf := strings.Replace(sampleConf, priv, priv2, 1)
	if err := os.WriteFile(filepath.Join(e.dir, "main.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Apply(ctx, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	sent := e.f.Commands[n:]
	if len(sent) != 3 || !strings.Contains(sent[0], "private-key") || !strings.Contains(sent[1], "preshared-key") || sent[2] != cli.Save {
		t.Errorf("sent %q", sent)
	}

	// Remove deletes only our interface.
	if _, err := e.m.Remove(ctx, "main", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.f.State.Interface("Wireguard1"); ok {
		t.Error("interface not removed")
	}
	if _, ok := e.f.State.Interface("Wireguard0"); !ok {
		t.Error("foreign interface removed")
	}
	if _, err := e.m.Remove(ctx, "main", false); err == nil {
		t.Error("second remove should fail: nothing recorded")
	}
}

func TestManagerRefusesForeignInterface(t *testing.T) {
	e := newEnv(t, "interface Wireguard0\n    up\n", Tunnel{Name: "main", ConfFile: "main.conf", Interface: "Wireguard0"})
	_, err := e.m.Apply(context.Background(), nil, ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "adopt") {
		t.Fatalf("err %v", err)
	}
	e.m.Tunnels[0].Adopt = true
	if _, err := e.m.Apply(context.Background(), nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if w, _ := e.f.State.Interface("Wireguard0"); len(w.Peers) != 1 {
		t.Errorf("adopted interface %+v", w)
	}
}

func TestManagerStatus(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if _, err := e.m.Apply(ctx, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	st := e.m.Status(ctx, e.m.Tunnels[0])
	if st.Err == nil || !strings.Contains(st.Err.Error(), "no handshake") {
		t.Errorf("status %+v", st)
	}
	e.f.Status = map[string]*router.WGStatus{"Wireguard0": {Name: "Wireguard0", Exists: true, Up: true,
		Peers: []router.WGPeerStatus{{PublicKey: pub, LastHandshake: 30 * time.Second, Endpoint: "203.0.113.1:51820"}}}}
	if st := e.m.Status(ctx, e.m.Tunnels[0]); st.Err != nil {
		t.Errorf("healthy tunnel: %v", st.Err)
	}
	e.f.Status["Wireguard0"].Peers[0].LastHandshake = 10 * time.Minute
	if st := e.m.Status(ctx, e.m.Tunnels[0]); st.Err == nil || !strings.Contains(st.Err.Error(), "203.0.113.1") {
		t.Errorf("stale handshake: %v", st.Err)
	}
	e.f.Status["Wireguard0"].Up = false
	if st := e.m.Status(ctx, e.m.Tunnels[0]); st.Err == nil || !strings.Contains(st.Err.Error(), "down") {
		t.Errorf("down: %v", st.Err)
	}
}

func TestValidateTunnels(t *testing.T) {
	err := ValidateTunnels([]Tunnel{
		{Name: "Main", ConfFile: "a"},
		{Name: "a", ConfFile: "a", Interface: "Wireguard1"},
		{Name: "b", ConfFile: "b", Conf: "x", Interface: "Wireguard1"},
		{Name: "c", ConfFile: "c", Adopt: true},
		{Name: "d", ConfFile: "d", Interface: "eth0"},
	})
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, s := range []string{`"Main"`, "exactly one of conf_file", "adopt needs", "Wireguard<N>"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("missing %q in %v", s, err)
		}
	}
}
