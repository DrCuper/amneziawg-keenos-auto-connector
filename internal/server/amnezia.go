package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/curve25519"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/remote"
)

// Servers installed by the AmneziaVPN app run AmneziaWG in a Docker
// container (amnezia-awg) with the config in /opt/amnezia/awg/. The app's
// "full access" share is a vpn:// key with the server's SSH credentials.
// awgkeen adopts such a server: it reads the keys and obfuscation
// parameters from the container, keeps the [Interface] and the app's own
// peers untouched and adds its peers marked with "# awgkeen: <name>".

// AdminKey is the SSH access carried by an Amnezia admin (full access) key.
type AdminKey struct {
	Host     string // hostName
	Port     int    // SSH port
	User     string
	Password string // or an SSH private key in PEM
	// Containers lists the protocols installed (amnezia-awg, ...).
	Containers  []string
	Description string
}

// PrivateKey reports whether Password holds an SSH private key.
func (k *AdminKey) PrivateKey() bool { return strings.Contains(k.Password, "PRIVATE KEY-----") }

// ParseAdminKey decodes an admin key. A client key (no credentials) is an
// error mentioning that.
func ParseAdminKey(key string) (*AdminKey, error) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "vpn://") {
		return nil, errors.New("an admin key is a vpn:// key")
	}
	doc, err := awg.DecodeVPNDocument(key)
	if err != nil {
		return nil, err
	}
	s := func(k string) string {
		switch v := doc[k].(type) {
		case string:
			return v
		case float64:
			return strconv.Itoa(int(v))
		}
		return ""
	}
	k := &AdminKey{Host: s("hostName"), User: s("userName"), Password: s("password"), Description: s("description")}
	if k.Host == "" || k.User == "" || k.Password == "" {
		return nil, errors.New("not an admin key: no server credentials inside (a client key gives access to the tunnel only)")
	}
	k.Port, _ = strconv.Atoi(s("port"))
	if k.Port == 0 {
		k.Port = 22
	}
	if cs, ok := doc["containers"].([]any); ok {
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok {
				if n, ok := m["container"].(string); ok {
					k.Containers = append(k.Containers, n)
				}
			}
		}
	}
	if strings.ContainsAny(k.Host, " \t\"'/") || strings.ContainsAny(k.User, " \t\"'/:") {
		return nil, errors.New("admin key: bad host or user name")
	}
	return k, nil
}

// IsAdminKey reports whether data is an Amnezia admin key.
func IsAdminKey(data []byte) bool {
	_, err := ParseAdminKey(string(data))
	return err == nil
}

// SSHHost is host:port for the SSH runner.
func (k *AdminKey) SSHHost() string { return net.JoinHostPort(k.Host, strconv.Itoa(k.Port)) }

var containerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// awgTools picks awg/awg-quick or wg/wg-quick inside the container.
const awgTools = `W=wg; WQ=wg-quick; if command -v awg >/dev/null 2>&1; then W=awg; WQ=awg-quick; fi; `

// inContainer wraps a script to run inside the container.
func inContainer(container, script string) string {
	return "docker exec -i " + remote.ShellQuote(container) + " sh -c " + remote.ShellQuote(script)
}

// detectAmnezia finds the AWG container and its config file.
func (m *Manager) detectAmnezia(ctx context.Context) (*AmneziaHost, error) {
	h := &AmneziaHost{Container: m.Config.Container}
	if h.Container == "" {
		out, err := m.run(ctx, `docker ps --format '{{.Names}}'`, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot list Docker containers (is this an Amnezia server? does the user have Docker access?): %w", err)
		}
		var found []string
		for _, n := range strings.Fields(out) {
			if strings.HasPrefix(n, "amnezia-awg") {
				found = append(found, n)
			}
		}
		switch {
		case len(found) == 0:
			return nil, fmt.Errorf("server %s: no amnezia-awg container running (install AmneziaWG in the AmneziaVPN app first)", m.Config.Name)
		case len(found) > 1 && contains(found, "amnezia-awg"):
			h.Container = "amnezia-awg"
		default:
			h.Container = found[0]
		}
	}
	if !containerName.MatchString(h.Container) {
		return nil, fmt.Errorf("bad container name %q", h.Container)
	}
	out, err := m.run(ctx, inContainer(h.Container, `ls /opt/amnezia/awg/*.conf 2>/dev/null`), nil)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("server %s: no config in %s:/opt/amnezia/awg: %w", m.Config.Name, h.Container, err)
	}
	files := strings.Fields(out)
	for _, pref := range []string{"/opt/amnezia/awg/wg0.conf", "/opt/amnezia/awg/awg0.conf"} {
		if contains(files, pref) {
			h.ConfigPath = pref
			break
		}
	}
	if h.ConfigPath == "" && len(files) > 0 {
		h.ConfigPath = files[0]
	}
	if h.ConfigPath == "" || !safePath.MatchString(h.ConfigPath) {
		return nil, fmt.Errorf("server %s: no AWG config found in %s:/opt/amnezia/awg", m.Config.Name, h.Container)
	}
	base := h.ConfigPath[strings.LastIndex(h.ConfigPath, "/")+1:]
	h.Interface = strings.TrimSuffix(base, ".conf")
	if !ifaceName.MatchString(h.Interface) {
		return nil, fmt.Errorf("bad interface name %q", h.Interface)
	}
	return h, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// readAmneziaConf reads the container's config.
func (m *Manager) readAmneziaConf(ctx context.Context, h *AmneziaHost) (string, error) {
	out, err := m.run(ctx, inContainer(h.Container, "cat "+remote.ShellQuote(h.ConfigPath)), nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("server %s: %s is empty", m.Config.Name, h.ConfigPath)
	}
	return out, nil
}

// serverInterface is the [Interface] of a server config.
type serverInterface struct {
	PrivateKey string
	Address    string
	ListenPort int
	Params     Params
}

// parseServerInterface reads the keys, address, port and obfuscation
// parameters of a server config; unknown keys (PostUp, SaveConfig) are
// skipped.
func parseServerInterface(conf string) (*serverInterface, error) {
	si := &serverInterface{}
	in := false
	for _, l := range strings.Split(conf, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			in = strings.EqualFold(l, "[Interface]")
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !in || !ok || strings.HasPrefix(l, "#") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		n, _ := strconv.Atoi(v)
		p := &si.Params
		switch strings.ToLower(k) {
		case "privatekey":
			si.PrivateKey = v
		case "address":
			for _, a := range strings.Split(v, ",") {
				if pf, err := netip.ParsePrefix(strings.TrimSpace(a)); err == nil && pf.Addr().Is4() && si.Address == "" {
					si.Address = pf.String()
				}
			}
		case "listenport":
			si.ListenPort = n
		case "jc":
			p.Jc = n
		case "jmin":
			p.Jmin = n
		case "jmax":
			p.Jmax = n
		case "s1":
			p.S1 = n
		case "s2":
			p.S2 = n
		case "s3":
			p.S3 = n
		case "s4":
			p.S4 = n
		case "h1":
			p.H1 = v
		case "h2":
			p.H2 = v
		case "h3":
			p.H3 = v
		case "h4":
			p.H4 = v
		case "i1", "i2", "i3", "i4", "i5":
			p.I[k[1]-'1'] = v
		}
	}
	var errs []error
	if raw, err := base64.StdEncoding.DecodeString(si.PrivateKey); err != nil || len(raw) != 32 {
		errs = append(errs, errors.New("[Interface] PrivateKey is missing or invalid"))
	}
	if si.Address == "" {
		errs = append(errs, errors.New("[Interface] Address has no IPv4 network"))
	}
	if si.ListenPort == 0 {
		errs = append(errs, errors.New("[Interface] ListenPort is missing"))
	}
	if si.Params.H1 == "" {
		// Plain WireGuard values: the client gets the defaults explicitly.
		si.Params.H1, si.Params.H2, si.Params.H3, si.Params.H4 = "1", "2", "3", "4"
	}
	return si, errors.Join(errs...)
}

func publicKey(private string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(private)
	if err != nil || len(raw) != 32 {
		return "", errors.New("bad private key")
	}
	pub, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// adoptAmnezia builds the state of an Amnezia server from its container.
func (m *Manager) adoptAmnezia(ctx context.Context) (*State, error) {
	h, err := m.detectAmnezia(ctx)
	if err != nil {
		return nil, err
	}
	conf, err := m.readAmneziaConf(ctx, h)
	if err != nil {
		return nil, err
	}
	si, err := parseServerInterface(conf)
	if err != nil {
		return nil, fmt.Errorf("server %s: %s: %w", m.Config.Name, h.ConfigPath, err)
	}
	pub, err := publicKey(si.PrivateKey)
	if err != nil {
		return nil, err
	}
	pf := netip.MustParsePrefix(si.Address)
	h.ListenPort, h.Subnet = si.ListenPort, pf.Masked().String()
	return &State{Keys: KeyPair{Private: si.PrivateKey, Public: pub}, Params: si.Params, Address: si.Address,
		CreatedAt: m.now().UTC(), Amnezia: h}, nil
}

// managedMark tags the peers awgkeen adds to an Amnezia config.
const managedMark = "# awgkeen: "

// splitConf cuts a config into its head ([Interface] and anything before
// the first peer) and [Peer] blocks, each trimmed.
func splitConf(conf string) (string, []string) {
	lines := strings.Split(strings.ReplaceAll(conf, "\r\n", "\n"), "\n")
	var head []string
	var blocks [][]string
	for _, l := range lines {
		if strings.EqualFold(strings.TrimSpace(l), "[Peer]") {
			blocks = append(blocks, []string{l})
			continue
		}
		if len(blocks) == 0 {
			head = append(head, l)
		} else {
			blocks[len(blocks)-1] = append(blocks[len(blocks)-1], l)
		}
	}
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, strings.TrimRight(strings.Join(b, "\n"), "\n\t "))
	}
	return strings.TrimRight(strings.Join(head, "\n"), "\n\t "), out
}

func managedBlock(b string) bool { return strings.Contains(b, "\n"+managedMark) }

// foreignAddresses lists the addresses of peers awgkeen does not manage.
func foreignAddresses(conf string) []netip.Addr {
	_, blocks := splitConf(conf)
	var out []netip.Addr
	for _, b := range blocks {
		if managedBlock(b) {
			continue
		}
		for _, l := range strings.Split(b, "\n") {
			k, v, ok := strings.Cut(l, "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "AllowedIPs") {
				continue
			}
			for _, a := range strings.Split(v, ",") {
				if p, err := netip.ParsePrefix(strings.TrimSpace(a)); err == nil {
					out = append(out, p.Addr())
				}
			}
		}
	}
	return out
}

// AmneziaConf renders the container config: the host's head and foreign
// peers unchanged, then awgkeen's enabled peers.
func AmneziaConf(have string, s *State) string {
	head, blocks := splitConf(have)
	var b strings.Builder
	b.WriteString(head + "\n")
	for _, blk := range blocks {
		if !managedBlock(blk) {
			b.WriteString("\n" + blk + "\n")
		}
	}
	for _, p := range s.Peers {
		if !p.Disabled {
			fmt.Fprintf(&b, "\n[Peer]\n%s%s\nPublicKey = %s\nPresharedKey = %s\nAllowedIPs = %s\n", managedMark, p.Name, p.Keys.Public, p.PSK, p.Address)
		}
	}
	return b.String()
}

// planAmnezia is Plan for an adopted Amnezia server.
func (m *Manager) planAmnezia(ctx context.Context, st *State) (*DeployPlan, string, error) {
	have, err := m.readAmneziaConf(ctx, st.Amnezia)
	if err != nil {
		return nil, "", err
	}
	want := AmneziaConf(have, st)
	p := &DeployPlan{}
	if head, _ := splitConf(have); !strings.Contains(head, st.Keys.Private) {
		return nil, "", fmt.Errorf("server %s: the server keys in %s changed (re-installed in the app?); run `awgkeen server init --force -s %s` and re-issue the clients", m.Config.Name, st.Amnezia.ConfigPath, m.Config.Name)
	}
	if normalizeAmnezia(have) == want {
		return p, want, nil
	}
	diffPeers(p, confPeers(have), confPeers(want))
	if len(p.Changes) == 0 {
		p.Changes = append(p.Changes, "~ "+st.Amnezia.ConfigPath+" (formatting)")
	}
	return p, want, nil
}

// normalizeAmnezia is the host config re-rendered without changes.
func normalizeAmnezia(have string) string {
	head, blocks := splitConf(have)
	var b strings.Builder
	b.WriteString(head + "\n")
	for _, blk := range blocks {
		b.WriteString("\n" + blk + "\n")
	}
	return b.String()
}

// deployAmnezia writes the config into the container and syncs the peers
// live; the interface itself is never restarted.
func (m *Manager) deployAmnezia(ctx context.Context, st *State, conf string) error {
	h := st.Amnezia
	q := remote.ShellQuote
	script := "set -e; umask 077; cat > " + q(h.ConfigPath+".awgkeen") + "; mv " + q(h.ConfigPath+".awgkeen") + " " + q(h.ConfigPath) + "; " +
		awgTools + "t=$(mktemp); $WQ strip " + q(h.ConfigPath) + " > \"$t\"; $W syncconf " + q(h.Interface) + " \"$t\"; rm -f \"$t\""
	_, err := m.run(ctx, inContainer(h.Container, script), []byte(conf))
	return err
}

// dumpScript is `awg show <if> dump` for either kind of server.
func (m *Manager) dumpScript(st *State) string {
	if st != nil && st.Amnezia != nil {
		return inContainer(st.Amnezia.Container, awgTools+"$W show "+remote.ShellQuote(st.Amnezia.Interface)+" dump")
	}
	return "awg show " + remote.ShellQuote(m.Config.Interface) + " dump"
}
