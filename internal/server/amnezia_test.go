package server

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/remote"
)

const amneziaConf = `[Interface]
PrivateKey = YUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=
Address = 10.8.1.0/24
ListenPort = 34567
Jc = 4
Jmin = 10
Jmax = 50
S1 = 101
S2 = 69
H1 = 1111
H2 = 2222
H3 = 3333
H4 = 4444

[Peer]
PublicKey = QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=
PresharedKey = Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M=
AllowedIPs = 10.8.1.1/32
`

// amneziaHost emulates an Amnezia server: Docker with an amnezia-awg
// container holding /opt/amnezia/awg/wg0.conf.
type amneziaHost struct {
	mu      sync.Mutex
	conf    string
	synced  int
	scripts []string
}

func unquote(s string) string {
	s = strings.TrimPrefix(strings.TrimSuffix(s, "'"), "'")
	return strings.ReplaceAll(s, `'\''`, `'`)
}

func (h *amneziaHost) Run(_ context.Context, script string, stdin []byte) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scripts = append(h.scripts, script)
	if strings.HasPrefix(script, "docker ps") {
		return "amnezia-xray\namnezia-awg\n", nil
	}
	inner, ok := strings.CutPrefix(script, "docker exec -i 'amnezia-awg' sh -c ")
	if !ok {
		return "", &remote.ExitError{Status: 127, Stderr: "unexpected: " + script}
	}
	inner = unquote(inner)
	switch {
	case strings.HasPrefix(inner, "ls /opt/amnezia/awg/"):
		return "/opt/amnezia/awg/wg0.conf\n", nil
	case inner == "cat '/opt/amnezia/awg/wg0.conf'":
		return h.conf, nil
	case strings.Contains(inner, "syncconf 'wg0'"):
		h.conf = string(stdin)
		h.synced++
		return "", nil
	case strings.Contains(inner, "show 'wg0' dump"):
		return "PRIV\tPUB\t34567\toff\nQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=\tPSK\t(none)\t10.8.1.1/32\t0\t0\t0\toff\n", nil
	}
	return "", &remote.ExitError{Status: 127, Stderr: "unexpected in container: " + inner}
}

func (h *amneziaHost) Close() error { return nil }

func TestAmneziaServer(t *testing.T) {
	ctx := context.Background()
	h := &amneziaHost{conf: amneziaConf}
	cfg := Config{Name: "am", Host: "198.51.100.5:22", User: "root", Password: "pw", KnownHostsFile: "kh", TrustOnFirstUse: true,
		Endpoint: "198.51.100.5", Amnezia: true}.WithDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Config: cfg, Store: NewStore(t.TempDir(), "am"), Runner: h}

	st, err := m.Init(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	a := st.Amnezia
	if a == nil || a.Container != "amnezia-awg" || a.Interface != "wg0" || a.ListenPort != 34567 || a.Subnet != "10.8.1.0/24" || st.Params.H1 != "1111" || st.Params.S1 != 101 {
		t.Fatalf("adopted %+v %+v", a, st.Params)
	}
	if _, err := m.Init(ctx, false); err == nil {
		t.Error("second adopt without force accepted")
	}
	if _, err := m.Install(ctx); err == nil {
		t.Error("install on an Amnezia server accepted")
	}

	p, err := m.AddPeer(ctx, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if p.Address != "10.8.1.2/32" {
		t.Errorf("address %s (10.8.1.1 is the app's client)", p.Address)
	}
	plan, err := m.Deploy(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Applied || plan.Restart || len(plan.Changes) != 1 || plan.Changes[0] != "+ peer phone (10.8.1.2/32)" {
		t.Errorf("plan %+v", plan)
	}
	if !strings.HasPrefix(h.conf, strings.TrimRight(amneziaConf, "\n")) || !strings.Contains(h.conf, "# awgkeen: phone\nPublicKey = "+p.Keys.Public) || h.synced != 1 {
		t.Errorf("container conf:\n%s", h.conf)
	}
	if again, _ := m.Deploy(ctx, false); !again.Empty() {
		t.Errorf("second deploy %v", again.Changes)
	}

	cc, _ := m.ClientConf("phone")
	for _, want := range []string{"Endpoint = 198.51.100.5:34567", "H1 = 1111", "S2 = 69", "PublicKey = " + st.Keys.Public} {
		if !strings.Contains(cc, want) {
			t.Errorf("client conf lacks %q:\n%s", want, cc)
		}
	}
	s, err := m.Status(ctx)
	if err != nil || !s.Up || len(s.Peers) != 2 || !s.Peers[0].Unknown || s.Peers[1].Name != "phone" {
		t.Errorf("status %+v %v", s, err)
	}

	if err := m.RemovePeer(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	if plan, err = m.Deploy(ctx, false); err != nil || len(plan.Changes) != 1 || !strings.HasPrefix(plan.Changes[0], "- peer phone") {
		t.Errorf("remove plan %+v %v", plan, err)
	}
	if strings.Contains(h.conf, "awgkeen") || !strings.Contains(h.conf, "AllowedIPs = 10.8.1.1/32") {
		t.Errorf("after remove:\n%s", h.conf)
	}

	// The app re-installed the server: new keys must not be overwritten.
	h.conf = strings.Replace(amneziaConf, "YUFB", "ZUFB", 1)
	if _, err := m.Deploy(ctx, false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("changed server keys: %v", err)
	}
}

func adminKey(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(doc)
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(raw)))
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return "vpn://" + base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

func TestParseAdminKey(t *testing.T) {
	k, err := ParseAdminKey(adminKey(t, map[string]any{"hostName": "198.51.100.5", "userName": "ubuntu", "password": "pw", "port": 2222,
		"containers": []any{map[string]any{"container": "amnezia-awg"}}, "description": "my vps"}))
	if err != nil {
		t.Fatal(err)
	}
	if k.SSHHost() != "198.51.100.5:2222" || k.User != "ubuntu" || k.PrivateKey() || len(k.Containers) != 1 {
		t.Errorf("%+v", k)
	}
	if _, err := ParseAdminKey(adminKey(t, map[string]any{"containers": []any{}, "hostName": "h"})); err == nil || !strings.Contains(err.Error(), "client key") {
		t.Errorf("client key: %v", err)
	}
	if IsAdminKey([]byte("[Interface]\n")) {
		t.Error(".conf taken for an admin key")
	}
}
