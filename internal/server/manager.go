package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/remote"
)

// Peer operations (journal kinds and API paths use the same names).
const (
	PeerAdd     = "add"
	PeerRemove  = "remove"
	PeerRotate  = "rotate"
	PeerDisable = "disable"
	PeerEnable  = "enable"
)

// Manager operates one server.
type Manager struct {
	Config Config // defaults applied
	Store  *Store
	Runner remote.Runner
	Logger *slog.Logger
	Now    func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Manager) run(ctx context.Context, script string, stdin []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.Config.Timeout)
	defer cancel()
	out, err := m.Runner.Run(ctx, script, stdin)
	if err != nil {
		return out, fmt.Errorf("server %s: %w", m.Config.Name, err)
	}
	return out, nil
}

// Init generates the server keys, obfuscation parameters and address.
// An initialized server is kept unless force is set (which invalidates
// every issued client configuration).
func (m *Manager) Init(ctx context.Context, force bool) (*State, error) {
	if m.Config.Amnezia {
		return m.adopt(ctx, force)
	}
	prefix, err := m.Config.Prefix()
	if err != nil {
		return nil, err
	}
	return m.Store.Update(ctx, func(cur *State) (*State, error) {
		if cur != nil && !force {
			return nil, fmt.Errorf("server %s is already initialized (use --force to regenerate keys and parameters; all clients must then be re-issued)", m.Config.Name)
		}
		keys, err := NewKeyPair()
		if err != nil {
			return nil, err
		}
		params, err := NewParams(m.Config.AWG2)
		if err != nil {
			return nil, err
		}
		st := &State{Keys: keys, Params: params, CreatedAt: m.now().UTC(),
			Address: netip.PrefixFrom(prefix.Addr().Next(), prefix.Bits()).String()}
		if cur != nil {
			// Keep the peers' identities and addresses; only the server changes.
			st.Peers = cur.Peers
		}
		return st, nil
	})
}

// adopt reads an Amnezia server's identity from its container. The peers
// awgkeen issued are kept.
func (m *Manager) adopt(ctx context.Context, force bool) (*State, error) {
	if _, err := m.Store.Load(); err == nil && !force {
		return nil, fmt.Errorf("server %s is already adopted (use --force to re-read it after re-installing in the AmneziaVPN app)", m.Config.Name)
	}
	st, err := m.adoptAmnezia(ctx)
	if err != nil {
		return nil, err
	}
	return m.Store.Update(ctx, func(cur *State) (*State, error) {
		if cur != nil {
			st.Peers = cur.Peers
		}
		return st, nil
	})
}

// effective is the config with what an adopted server dictates.
func (m *Manager) effective(st *State) Config {
	c := m.Config
	if st != nil && st.Amnezia != nil {
		c.ListenPort, c.Subnet, c.Interface = st.Amnezia.ListenPort, st.Amnezia.Subnet, st.Amnezia.Interface
	}
	return c
}

// Load returns the state.
func (m *Manager) Load() (*State, error) { return m.Store.Load() }

// AddPeer allocates an address and keys for a new client.
func (m *Manager) AddPeer(ctx context.Context, name string) (*Peer, error) {
	if err := ValidatePeerName(name); err != nil {
		return nil, err
	}
	cur, err := m.Store.Load()
	if err != nil {
		return nil, err
	}
	prefix, err := m.effective(cur).Prefix()
	if err != nil {
		return nil, err
	}
	var extra []netip.Addr
	if cur.Amnezia != nil {
		conf, err := m.readAmneziaConf(ctx, cur.Amnezia)
		if err != nil {
			return nil, err
		}
		extra = foreignAddresses(conf)
	}
	var added Peer
	_, err = m.Store.Update(ctx, func(st *State) (*State, error) {
		if st == nil {
			return nil, ErrNotInitialized
		}
		if _, ok := st.Peer(name); ok {
			return nil, fmt.Errorf("peer %q already exists", name)
		}
		addr, err := st.NextAddress(prefix, extra...)
		if err != nil {
			return nil, err
		}
		keys, err := NewKeyPair()
		if err != nil {
			return nil, err
		}
		psk, err := NewPSK()
		if err != nil {
			return nil, err
		}
		added = Peer{Name: name, Address: netip.PrefixFrom(addr, 32).String(), Keys: keys, PSK: psk, CreatedAt: m.now().UTC()}
		st.Peers = append(st.Peers, added)
		return st, nil
	})
	if err != nil {
		return nil, err
	}
	return &added, nil
}

// RemovePeer forgets a client; deploy removes it from the server.
func (m *Manager) RemovePeer(ctx context.Context, name string) error {
	_, err := m.Store.Update(ctx, func(st *State) (*State, error) {
		if st == nil {
			return nil, ErrNotInitialized
		}
		for i, p := range st.Peers {
			if p.Name == name {
				st.Peers = append(st.Peers[:i], st.Peers[i+1:]...)
				return st, nil
			}
		}
		return nil, fmt.Errorf("unknown peer %q", name)
	})
	return err
}

// SetDisabled blocks or unblocks a client without forgetting it.
func (m *Manager) SetDisabled(ctx context.Context, name string, disabled bool) error {
	_, err := m.Store.Update(ctx, func(st *State) (*State, error) {
		if st == nil {
			return nil, ErrNotInitialized
		}
		p, ok := st.Peer(name)
		if !ok {
			return nil, fmt.Errorf("unknown peer %q", name)
		}
		p.Disabled = disabled
		return st, nil
	})
	return err
}

// RotatePeer issues new keys for a client; its old configuration stops
// working after deploy.
func (m *Manager) RotatePeer(ctx context.Context, name string) (*Peer, error) {
	var out Peer
	_, err := m.Store.Update(ctx, func(st *State) (*State, error) {
		if st == nil {
			return nil, ErrNotInitialized
		}
		p, ok := st.Peer(name)
		if !ok {
			return nil, fmt.Errorf("unknown peer %q", name)
		}
		keys, err := NewKeyPair()
		if err != nil {
			return nil, err
		}
		psk, err := NewPSK()
		if err != nil {
			return nil, err
		}
		p.Keys, p.PSK, p.RotatedAt = keys, psk, m.now().UTC()
		out = *p
		return st, nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ClientConf returns a peer's client configuration.
func (m *Manager) ClientConf(name string) (string, error) {
	st, p, err := m.peer(name)
	if err != nil {
		return "", err
	}
	return ClientConf(m.effective(st), st, p), nil
}

// VPNKey returns a peer's Amnezia vpn:// key.
func (m *Manager) VPNKey(name string) (string, error) {
	st, p, err := m.peer(name)
	if err != nil {
		return "", err
	}
	return VPNKey(m.effective(st), st, p)
}

func (m *Manager) peer(name string) (*State, *Peer, error) {
	st, err := m.Store.Load()
	if err != nil {
		return nil, nil, err
	}
	p, ok := st.Peer(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown peer %q", name)
	}
	return st, p, nil
}

// DeployPlan describes what a deploy changes on the host.
type DeployPlan struct {
	Changes []string `json:"changes"`
	// Restart is needed when the [Interface] part changed (keys, address,
	// port, obfuscation); peer changes are applied live with syncconf.
	Restart bool   `json:"restart"`
	Create  bool   `json:"create"` // no configuration on the host yet
	Applied bool   `json:"applied"`
	NAT     string `json:"nat_interface,omitempty"`
}

// Empty reports whether the host already has the configuration.
func (p *DeployPlan) Empty() bool { return len(p.Changes) == 0 }

// String renders the plan.
func (p *DeployPlan) String() string {
	if p.Empty() {
		return "no changes\n"
	}
	var b strings.Builder
	for _, c := range p.Changes {
		b.WriteString("  " + c + "\n")
	}
	return b.String()
}

func (m *Manager) natInterface(ctx context.Context) (string, error) {
	if m.Config.NATInterface != "" {
		return m.Config.NATInterface, nil
	}
	out, err := m.run(ctx, `ip -4 route show default | awk '{for(i=1;i<NF;i++) if($i=="dev"){print $(i+1); exit}}'`, nil)
	if err != nil {
		return "", err
	}
	dev := strings.TrimSpace(out)
	if !ifaceName.MatchString(dev) {
		return "", fmt.Errorf("server %s: cannot detect the uplink interface (set nat_interface)", m.Config.Name)
	}
	return dev, nil
}

// Plan compares the rendered configuration with the one on the host.
func (m *Manager) Plan(ctx context.Context) (*DeployPlan, string, *State, error) {
	st, err := m.Store.Load()
	if err != nil {
		return nil, "", nil, err
	}
	if st.Amnezia != nil {
		p, want, err := m.planAmnezia(ctx, st)
		return p, want, st, err
	}
	nat, err := m.natInterface(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	want := ServerConf(m.Config, st, nat)
	have, err := m.run(ctx, "cat "+remote.ShellQuote(m.Config.ConfigPath)+" 2>/dev/null || true", nil)
	if err != nil {
		return nil, "", nil, err
	}
	p := &DeployPlan{NAT: nat}
	if have == want {
		return p, want, st, nil
	}
	if strings.TrimSpace(have) == "" {
		p.Create = true
		p.Changes = append(p.Changes, "+ "+m.Config.ConfigPath+" ("+m.Config.Interface+", port "+strconv.Itoa(m.Config.ListenPort)+")")
	} else if interfaceSection(have) != interfaceSection(want) {
		p.Restart = true
		p.Changes = append(p.Changes, "~ [Interface] settings (keys, address, port or obfuscation): interface restart")
	}
	diffPeers(p, confPeers(have), confPeers(want))
	if len(p.Changes) == 0 {
		p.Changes = append(p.Changes, "~ "+m.Config.ConfigPath+" (formatting)")
	}
	return p, want, st, nil
}

// diffPeers adds +/~/- lines for peers, sorted by name.
func diffPeers(p *DeployPlan, havePeers, wantPeers map[string]string) {
	var names []string
	for k := range wantPeers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch old, ok := havePeers[k]; {
		case !ok:
			p.Changes = append(p.Changes, "+ peer "+label(wantPeers[k]))
		case old != wantPeers[k]:
			p.Changes = append(p.Changes, "~ peer "+label(wantPeers[k]))
		}
	}
	names = names[:0]
	for k := range havePeers {
		if _, ok := wantPeers[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		p.Changes = append(p.Changes, "- peer "+label(havePeers[k]))
	}
}

// confPeers maps peer name → "name (address)|fingerprint" from a server
// config; the fingerprint of key and PSK makes a key change a "~".
func confPeers(conf string) map[string]string {
	out := map[string]string{}
	for _, sec := range strings.Split(conf, "[Peer]")[1:] {
		var name, pub, psk, addr string
		for _, l := range strings.Split(sec, "\n") {
			l = strings.TrimSpace(l)
			switch {
			case strings.HasPrefix(l, "# ") && name == "":
				name = strings.TrimPrefix(strings.TrimPrefix(l, "# "), "awgkeen: ")
			case strings.HasPrefix(l, "PublicKey"):
				pub = strings.TrimSpace(after(l))
			case strings.HasPrefix(l, "PresharedKey"):
				psk = strings.TrimSpace(after(l))
			case strings.HasPrefix(l, "AllowedIPs"):
				addr = strings.TrimSpace(after(l))
			}
		}
		if name == "" {
			name = pub[:min(len(pub), 8)] + "…"
		}
		out[name] = name + " (" + addr + ")" + "|" + Hash(pub + psk)[:8]
	}
	return out
}

// label drops the fingerprint from a confPeers value.
func label(v string) string {
	l, _, _ := strings.Cut(v, "|")
	return l
}

func after(l string) string {
	_, v, _ := strings.Cut(l, "=")
	return v
}

// systemdPath reports whether the config lives where awg-quick@ expects it.
func (m *Manager) systemdPath() bool {
	return m.Config.ConfigPath == "/etc/amnezia/amneziawg/"+m.Config.Interface+".conf"
}

// Deploy writes the configuration and brings the interface up or syncs it.
func (m *Manager) Deploy(ctx context.Context, dryRun bool) (*DeployPlan, error) {
	p, conf, st, err := m.Plan(ctx)
	if err != nil || dryRun || p.Empty() {
		return p, err
	}
	if st.Amnezia != nil {
		if err := m.deployAmnezia(ctx, st, conf); err != nil {
			return p, err
		}
		return p, m.deployed(ctx, p, conf)
	}
	cfg := m.Config
	q := remote.ShellQuote
	dir := path.Dir(cfg.ConfigPath)
	write := "set -e; umask 077; mkdir -p " + q(dir) + "; cat > " + q(cfg.ConfigPath+".tmp") + "; mv " + q(cfg.ConfigPath+".tmp") + " " + q(cfg.ConfigPath)
	if _, err := m.run(ctx, write, []byte(conf)); err != nil {
		return p, err
	}
	if _, err := m.run(ctx, "sysctl -qw net.ipv4.ip_forward=1 && echo net.ipv4.ip_forward=1 > /etc/sysctl.d/99-awgkeen.conf", nil); err != nil {
		return p, err
	}
	iface := q(cfg.Interface)
	var up, restart string
	if m.systemdPath() {
		up = "systemctl enable --now awg-quick@" + cfg.Interface
		restart = "systemctl restart awg-quick@" + cfg.Interface
	} else {
		up = "awg-quick up " + q(cfg.ConfigPath)
		restart = "awg-quick down " + q(cfg.ConfigPath) + "; awg-quick up " + q(cfg.ConfigPath)
	}
	sync := "t=$(mktemp) && awg-quick strip " + q(cfg.ConfigPath) + " > \"$t\" && awg syncconf " + iface + " \"$t\"; rc=$?; rm -f \"$t\"; exit $rc"
	script := "if awg show " + iface + " >/dev/null 2>&1; then "
	if p.Restart {
		script += restart
	} else {
		script += sync
	}
	script += "; else " + up + "; fi"
	if _, err := m.run(ctx, script, nil); err != nil {
		return p, err
	}
	return p, m.deployed(ctx, p, conf)
}

// deployed records a successful deploy.
func (m *Manager) deployed(ctx context.Context, p *DeployPlan, conf string) error {
	p.Applied = true
	_, err := m.Store.Update(ctx, func(st *State) (*State, error) {
		if st == nil {
			return nil, ErrNotInitialized
		}
		st.DeployedHash, st.DeployedAt = Hash(conf), m.now().UTC()
		return st, nil
	})
	m.logger().Info("server: deployed", "server", m.Config.Name, "changes", len(p.Changes), "restart", p.Restart)
	return err
}

// PeerStatus is the runtime state of one peer (`awg show dump`).
type PeerStatus struct {
	Name          string    `json:"name"`
	PublicKey     string    `json:"public_key"`
	Address       string    `json:"address,omitempty"`
	Endpoint      string    `json:"endpoint,omitempty"`
	LastHandshake time.Time `json:"last_handshake,omitzero"`
	RxBytes       uint64    `json:"rx_bytes"`
	TxBytes       uint64    `json:"tx_bytes"`
	Disabled      bool      `json:"disabled,omitempty"`
	Unknown       bool      `json:"unknown,omitempty"` // on the host but not in the state
}

// Status is the runtime state of the server interface.
type Status struct {
	Up         bool         `json:"up"`
	ListenPort int          `json:"listen_port,omitempty"`
	PublicKey  string       `json:"public_key,omitempty"`
	Peers      []PeerStatus `json:"peers"`
}

// Status reads `awg show <iface> dump`. A missing interface is Up=false.
func (m *Manager) Status(ctx context.Context) (*Status, error) {
	st, err := m.Store.Load()
	if err != nil {
		return nil, err
	}
	out, err := m.run(ctx, m.dumpScript(st), nil)
	var ee *remote.ExitError
	if errors.As(err, &ee) {
		s := &Status{}
		for _, p := range st.Peers {
			s.Peers = append(s.Peers, PeerStatus{Name: p.Name, PublicKey: p.Keys.Public, Address: p.Address, Disabled: p.Disabled})
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseDump(out, st), nil
}

// ParseDump parses `awg show <iface> dump` and names peers from the state.
func ParseDump(out string, st *State) *Status {
	s := &Status{}
	byKey := map[string]*Peer{}
	if st != nil {
		for i := range st.Peers {
			byKey[st.Peers[i].Keys.Public] = &st.Peers[i]
		}
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if i == 0 {
			if len(f) >= 3 {
				s.Up = true
				s.PublicKey = f[1]
				s.ListenPort, _ = strconv.Atoi(f[2])
			}
			continue
		}
		if len(f) < 8 {
			continue
		}
		ps := PeerStatus{PublicKey: f[0], Address: f[3]}
		if f[2] != "(none)" {
			ps.Endpoint = f[2]
		}
		if ts, err := strconv.ParseInt(f[4], 10, 64); err == nil && ts > 0 {
			ps.LastHandshake = time.Unix(ts, 0).UTC()
		}
		ps.RxBytes, _ = strconv.ParseUint(f[5], 10, 64)
		ps.TxBytes, _ = strconv.ParseUint(f[6], 10, 64)
		if p, ok := byKey[f[0]]; ok {
			ps.Name = p.Name
			seen[f[0]] = true
		} else {
			ps.Unknown = true
		}
		s.Peers = append(s.Peers, ps)
	}
	if st != nil {
		for _, p := range st.Peers {
			if !seen[p.Keys.Public] {
				s.Peers = append(s.Peers, PeerStatus{Name: p.Name, PublicKey: p.Keys.Public, Address: p.Address, Disabled: p.Disabled})
			}
		}
	}
	return s
}

// InstallScript installs amneziawg (kernel module via DKMS + tools) from the
// Amnezia PPA on Ubuntu. Other systems get instructions.
const InstallScript = `set -e
if command -v awg >/dev/null 2>&1 && command -v awg-quick >/dev/null 2>&1; then
  echo "amneziawg is already installed: $(awg --version 2>/dev/null || echo awg)"
  exit 0
fi
. /etc/os-release
if [ "$ID" != ubuntu ]; then
  echo "automatic install supports Ubuntu only (found $ID); install amneziawg by hand:" >&2
  echo "https://github.com/amnezia-vpn/amneziawg-linux-kernel-module" >&2
  exit 3
fi
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q software-properties-common iptables "linux-headers-$(uname -r)" || apt-get install -y -q software-properties-common iptables
add-apt-repository -y ppa:amnezia/ppa
apt-get update -q
apt-get install -y -q amneziawg
command -v awg-quick >/dev/null
echo "amneziawg installed"
`

// Install runs InstallScript on the host.
func (m *Manager) Install(ctx context.Context) (string, error) {
	if m.Config.Amnezia {
		return "", fmt.Errorf("server %s is managed by the AmneziaVPN app: install and update protocols there", m.Config.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	out, err := m.Runner.Run(ctx, InstallScript, nil)
	if err != nil {
		return out, fmt.Errorf("server %s: install: %w", m.Config.Name, err)
	}
	return out, nil
}

// Check is the health probe: the interface is up on the host.
func (m *Manager) Check(ctx context.Context) error {
	s, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if !s.Up {
		iface := m.Config.Interface
		if st, err := m.Store.Load(); err == nil {
			iface = m.effective(st).Interface
		}
		return fmt.Errorf("interface %s is not up on %s", iface, m.Config.Name)
	}
	return nil
}
