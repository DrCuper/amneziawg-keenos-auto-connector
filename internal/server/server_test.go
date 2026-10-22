package server

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/remote"
)

// fakeHost emulates the scripts Manager runs on a Linux host.
type fakeHost struct {
	mu      sync.Mutex
	files   map[string]string
	up      bool
	dump    string
	scripts []string
}

func (h *fakeHost) Run(_ context.Context, script string, stdin []byte) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scripts = append(h.scripts, script)
	switch {
	case strings.HasPrefix(script, "ip -4 route"):
		return "eth0\n", nil
	case strings.HasPrefix(script, "cat '"):
		p := strings.TrimPrefix(script, "cat '")
		p = p[:strings.Index(p, "'")]
		return h.files[p], nil
	case strings.Contains(script, "cat > "):
		i := strings.Index(script, "mv '")
		p := script[i+4:]
		p = p[strings.Index(p, " '")+2:]
		p = strings.TrimSuffix(p, "'")
		h.files[p] = string(stdin)
		return "", nil
	case strings.HasPrefix(script, "sysctl"):
		return "", nil
	case strings.HasPrefix(script, "if awg show"):
		h.up = true
		return "", nil
	case strings.HasPrefix(script, "awg show"):
		if !h.up {
			return "", &remote.ExitError{Status: 1, Stderr: "Unable to access interface: No such device"}
		}
		return h.dump, nil
	}
	return "", &remote.ExitError{Status: 127, Stderr: "unexpected script: " + script}
}

func (h *fakeHost) Close() error { return nil }

func newTestManager(t *testing.T) (*Manager, *fakeHost) {
	t.Helper()
	h := &fakeHost{files: map[string]string{}}
	cfg := Config{Name: "vps", Host: "203.0.113.10", KeyFile: "k", InsecureHostKey: true, Endpoint: "vpn.example.com"}.WithDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return &Manager{Config: cfg, Store: NewStore(t.TempDir(), "vps"), Runner: h}, h
}

func TestKeyPair(t *testing.T) {
	k, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := base64.StdEncoding.DecodeString(k.Private)
	pub, _ := curve25519.X25519(priv, curve25519.Basepoint)
	if base64.StdEncoding.EncodeToString(pub) != k.Public || priv[0]&7 != 0 || priv[31]&0xC0 != 0x40 {
		t.Errorf("bad key pair %+v", k)
	}
}

func TestParams(t *testing.T) {
	for _, awg2 := range []bool{false, true} {
		for range 50 {
			p, err := NewParams(awg2)
			if err != nil {
				t.Fatal(err)
			}
			if p.Jc < 3 || p.Jc > 10 || p.Jmin >= p.Jmax || p.Jmax > 1280 || p.S1+56 == p.S2 || p.S1 > 1132 || p.S2 > 1188 {
				t.Fatalf("params out of range: %+v", p)
			}
			hs := map[string]bool{p.H1: true, p.H2: true, p.H3: true, p.H4: true}
			if len(hs) != 4 {
				t.Fatalf("H values not distinct: %+v", p)
			}
			if awg2 != strings.Contains(p.H1, "-") || awg2 != (p.S3 != 0) {
				t.Fatalf("awg2=%v: %+v", awg2, p)
			}
		}
	}
}

func TestNextAddress(t *testing.T) {
	sub := netip.MustParsePrefix("10.0.0.0/30")
	st := &State{Address: "10.0.0.1/30"}
	a, err := st.NextAddress(sub)
	if err != nil || a.String() != "10.0.0.2" {
		t.Fatalf("got %v %v", a, err)
	}
	st.Peers = append(st.Peers, Peer{Address: "10.0.0.2/32"})
	if _, err := st.NextAddress(sub); err == nil {
		t.Fatal("expected full subnet")
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	m, h := newTestManager(t)

	if _, err := m.AddPeer(ctx, "phone"); err == nil {
		t.Fatal("add before init must fail")
	}
	st, err := m.Init(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Address != "10.8.1.1/24" {
		t.Errorf("server address %s", st.Address)
	}
	if _, err := m.Init(ctx, false); err == nil {
		t.Fatal("second init without force must fail")
	}
	info, _ := os.Stat(m.Store.Path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state mode %v", info.Mode())
	}

	p1, err := m.AddPeer(ctx, "phone")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := m.AddPeer(ctx, "laptop")
	if p1.Address != "10.8.1.2/32" || p2.Address != "10.8.1.3/32" {
		t.Errorf("addresses %s %s", p1.Address, p2.Address)
	}
	if _, err := m.AddPeer(ctx, "phone"); err == nil {
		t.Error("duplicate peer accepted")
	}
	if _, err := m.AddPeer(ctx, "bad name"); err == nil {
		t.Error("bad peer name accepted")
	}

	// First deploy creates the file and brings the interface up.
	plan, err := m.Deploy(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Create || !plan.Applied || len(plan.Changes) != 3 {
		t.Errorf("first plan %+v", plan)
	}
	conf := h.files[m.Config.ConfigPath]
	for _, want := range []string{"PrivateKey = " + st.Keys.Private, "ListenPort = 51820", "-o eth0 -j MASQUERADE", "# phone", "AllowedIPs = 10.8.1.3/32", "Jc = "} {
		if !strings.Contains(conf, want) {
			t.Errorf("server conf lacks %q:\n%s", want, conf)
		}
	}
	if !strings.Contains(h.scripts[len(h.scripts)-1], "systemctl enable --now awg-quick@awg0") {
		t.Errorf("bring-up script %q", h.scripts[len(h.scripts)-1])
	}
	if p, _ := m.Deploy(ctx, false); !p.Empty() {
		t.Errorf("second deploy not empty: %v", p.Changes)
	}

	// Peer changes sync live; disabling removes the [Peer] section.
	if err := m.SetDisabled(ctx, "laptop", true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RotatePeer(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	plan, err = m.Deploy(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Restart || len(plan.Changes) != 2 || plan.Changes[0] != "~ peer phone (10.8.1.2/32)" || plan.Changes[1] != "- peer laptop (10.8.1.3/32)" {
		t.Errorf("peer plan %+v", plan)
	}
	if last := h.scripts[len(h.scripts)-1]; !strings.Contains(last, "awg syncconf 'awg0'") {
		t.Errorf("sync script %q", last)
	}
	if strings.Contains(h.files[m.Config.ConfigPath], p2.Keys.Public) {
		t.Error("disabled peer still in server conf")
	}

	// Client config matches the server.
	cc, err := m.ClientConf("phone")
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := m.Load()
	for _, want := range []string{"Address = 10.8.1.2/32", "PublicKey = " + cur.Keys.Public, "Endpoint = vpn.example.com:51820", "AllowedIPs = 0.0.0.0/0", "H1 = " + cur.Params.H1, "PrivateKey = " + cur.Peers[0].Keys.Private} {
		if !strings.Contains(cc, want) {
			t.Errorf("client conf lacks %q:\n%s", want, cc)
		}
	}

	// Status maps runtime peers to names.
	h.dump = "PRIV\t" + cur.Keys.Public + "\t51820\toff\n" +
		cur.Peers[0].Keys.Public + "\tPSK\t198.51.100.7:4242\t10.8.1.2/32\t" + "1700000000" + "\t100\t200\t25\n" +
		"Zm9yZWlnbg==\t(none)\t(none)\t10.8.1.9/32\t0\t0\t0\toff\n"
	s, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Up || s.ListenPort != 51820 || len(s.Peers) != 3 || s.Peers[0].Name != "phone" || s.Peers[0].RxBytes != 100 ||
		s.Peers[0].LastHandshake != time.Unix(1700000000, 0).UTC() || !s.Peers[1].Unknown || s.Peers[2].Name != "laptop" || !s.Peers[2].Disabled {
		t.Errorf("status %+v", s)
	}
	if err := m.Check(ctx); err != nil {
		t.Errorf("check: %v", err)
	}

	// Interface key change needs a restart.
	if _, err := m.Init(ctx, true); err != nil {
		t.Fatal(err)
	}
	if plan, _, _, _ := m.Plan(ctx); !plan.Restart {
		t.Errorf("re-init plan %+v", plan)
	}
	if err := m.RemovePeer(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemovePeer(ctx, "phone"); err == nil {
		t.Error("double remove accepted")
	}
}

func TestStatusDown(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t)
	if _, err := m.Init(ctx, false); err != nil {
		t.Fatal(err)
	}
	s, err := m.Status(ctx)
	if err != nil || s.Up {
		t.Fatalf("status %+v %v", s, err)
	}
	if err := m.Check(ctx); err == nil || !strings.Contains(err.Error(), "not up") {
		t.Errorf("check: %v", err)
	}
}

func TestVPNKey(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t)
	_, _ = m.Init(ctx, false)
	_, _ = m.AddPeer(ctx, "phone")
	key, err := m.VPNKey("phone")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, "vpn://"))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw[4:]))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(zr)
	var doc struct {
		HostName   string `json:"hostName"`
		Containers []struct {
			AWG struct {
				LastConfig string `json:"last_config"`
				Port       string `json:"port"`
			} `json:"awg"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.HostName != "vpn.example.com" || len(doc.Containers) != 1 || doc.Containers[0].AWG.Port != "51820" {
		t.Fatalf("doc %+v", doc)
	}
	var last struct {
		Config string `json:"config"`
	}
	_ = json.Unmarshal([]byte(doc.Containers[0].AWG.LastConfig), &last)
	if !strings.Contains(last.Config, "Address = 10.8.1.2/32") {
		t.Errorf("last_config %q", last.Config)
	}
}

func TestQR(t *testing.T) {
	png, err := QRPNG("[Interface]\nPrivateKey = x\n", 256)
	if err != nil || !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Fatalf("png %v", err)
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Name: "vps", Host: "h", Password: "p", InsecureHostKey: true, Endpoint: "1.2.3.4"}
	cases := map[string]func(*Config){
		"name":     func(c *Config) { c.Name = "Bad Name" },
		"host":     func(c *Config) { c.Host = "" },
		"auth":     func(c *Config) { c.Password = "" },
		"hostkey":  func(c *Config) { c.InsecureHostKey = false },
		"endpoint": func(c *Config) { c.Endpoint = "" },
		"subnet":   func(c *Config) { c.Subnet = "10.8.1.1/24" },
		"path":     func(c *Config) { c.ConfigPath = "/etc/x;rm" },
		"iface":    func(c *Config) { c.Interface = "a b" },
	}
	if err := ValidateAll([]Config{ok}); err != nil {
		t.Fatal(err)
	}
	for name, mut := range cases {
		c := ok
		mut(&c)
		if err := ValidateAll([]Config{c}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateAll([]Config{ok, ok}); err == nil {
		t.Error("duplicate accepted")
	}
	local := Config{Name: "home", Local: true, Endpoint: "my.ddns.net:51821"}
	if err := ValidateAll([]Config{local}); err != nil {
		t.Errorf("local: %v", err)
	}
	if e := local.WithDefaults().ClientEndpoint(); e != "my.ddns.net:51821" {
		t.Errorf("endpoint %s", e)
	}
}

func TestCustomPathUsesAWGQuick(t *testing.T) {
	ctx := context.Background()
	m, h := newTestManager(t)
	m.Config.ConfigPath = filepath.Join("/opt/awg", "wg9.conf")
	_, _ = m.Init(ctx, false)
	if _, err := m.Deploy(ctx, false); err != nil {
		t.Fatal(err)
	}
	if last := h.scripts[len(h.scripts)-1]; !strings.Contains(last, "awg-quick up '/opt/awg/wg9.conf'") {
		t.Errorf("script %q", last)
	}
}
