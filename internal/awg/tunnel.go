package awg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// InterfaceAuto picks the lowest free Wireguard<N> on first import.
const InterfaceAuto = "auto"

// Tunnel is one `awg.tunnels[]` entry of the config: an Amnezia client
// configuration to keep imported on the router.
type Tunnel struct {
	Name string `yaml:"name"`
	// ConfFile is a `.conf` file or a file with a `vpn://` key. Relative
	// paths resolve against the config file's directory.
	ConfFile string `yaml:"conf_file"`
	// Conf is the same content inline (discouraged: it holds the private key).
	Conf string `yaml:"conf"`
	// Interface is Wireguard<N> or "auto" (default).
	Interface string `yaml:"interface"`
	// Adopt allows taking over an existing interface the service did not
	// create (only with an explicit Interface).
	Adopt         bool   `yaml:"adopt"`
	Description   string `yaml:"description"`    // default "awgkeen: <name>"
	SecurityLevel string `yaml:"security_level"` // default public
	// IPGlobal is `ip global`: "" leaves it, "auto"/<priority> sets, "off" removes.
	IPGlobal  string `yaml:"ip_global"`
	AdjustMSS *bool  `yaml:"adjust_mss"` // default true
	MTU       int    `yaml:"mtu"`        // overrides the .conf MTU
	// Keepalive applies when the .conf has no PersistentKeepalive (default 25).
	Keepalive *int `yaml:"keepalive"`
	// AllowedIPs overrides the peers' AllowedIPs.
	AllowedIPs []string `yaml:"allowed_ips"`
	Enabled    *bool    `yaml:"enabled"` // interface up, default true
	Check      Check    `yaml:"check"`
	// Imported marks a key added with `awg import` (kept in state_dir).
	Imported bool `yaml:"-"`
}

// Check configures the built-in health check of a tunnel.
type Check struct {
	Enabled *bool `yaml:"enabled"` // default true
	// MaxHandshakeAge: older handshake = tunnel down (default 5m).
	MaxHandshakeAge time.Duration `yaml:"max_handshake_age"`
}

// IsEnabled reports whether the check runs.
func (c Check) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// MaxAge returns the effective handshake age limit.
func (c Check) MaxAge() time.Duration {
	if c.MaxHandshakeAge > 0 {
		return c.MaxHandshakeAge
	}
	return 5 * time.Minute
}

// IsUp reports whether the interface should be up.
func (t Tunnel) IsUp() bool { return t.Enabled == nil || *t.Enabled }

// AutoInterface reports whether the interface is picked automatically.
func (t Tunnel) AutoInterface() bool { return t.Interface == "" || t.Interface == InterfaceAuto }

var tunnelName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// Validate checks one tunnel definition (not the .conf content).
func (t Tunnel) Validate() error {
	var errs []error
	if !tunnelName.MatchString(t.Name) {
		errs = append(errs, fmt.Errorf("awg tunnel name %q must match %s", t.Name, tunnelName))
	}
	if (t.ConfFile == "") == (t.Conf == "") {
		errs = append(errs, fmt.Errorf("awg tunnel %q: set exactly one of conf_file or conf", t.Name))
	}
	if !t.AutoInterface() {
		if err := router.ValidateWGName(t.Interface); err != nil {
			errs = append(errs, fmt.Errorf("awg tunnel %q: %w (or auto)", t.Name, err))
		}
	} else if t.Adopt {
		errs = append(errs, fmt.Errorf("awg tunnel %q: adopt needs an explicit interface", t.Name))
	}
	switch t.SecurityLevel {
	case "", "public", "private", "protected":
	default:
		errs = append(errs, fmt.Errorf("awg tunnel %q: security_level must be public, private or protected", t.Name))
	}
	if t.Keepalive != nil && (*t.Keepalive < 0 || *t.Keepalive > 65535) {
		errs = append(errs, fmt.Errorf("awg tunnel %q: keepalive out of range", t.Name))
	}
	for _, a := range t.AllowedIPs {
		if _, err := router.CanonicalPrefix(a); err != nil {
			errs = append(errs, fmt.Errorf("awg tunnel %q: allowed_ips: %w", t.Name, err))
		}
	}
	if t.Check.MaxHandshakeAge < 0 {
		errs = append(errs, fmt.Errorf("awg tunnel %q: check.max_handshake_age must be >= 0", t.Name))
	}
	return errors.Join(errs...)
}

// ValidateTunnels validates all tunnels and their uniqueness.
func ValidateTunnels(ts []Tunnel) error {
	var errs []error
	names, ifaces := map[string]bool{}, map[string]string{}
	for _, t := range ts {
		if err := t.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		if names[t.Name] {
			errs = append(errs, fmt.Errorf("awg tunnel %q is listed twice", t.Name))
		}
		names[t.Name] = true
		if !t.AutoInterface() {
			k := strings.ToLower(t.Interface)
			if other, ok := ifaces[k]; ok {
				errs = append(errs, fmt.Errorf("awg tunnels %q and %q use the same interface %s", other, t.Name, t.Interface))
			}
			ifaces[k] = t.Name
		}
	}
	return errors.Join(errs...)
}

// Load reads and parses the tunnel's configuration. baseDir resolves a
// relative conf_file.
func (t Tunnel) Load(baseDir string) (*Conf, error) {
	data := []byte(t.Conf)
	var warn []string
	if t.ConfFile != "" {
		p := t.ConfFile
		if !filepath.IsAbs(p) && baseDir != "" {
			p = filepath.Join(baseDir, p)
		}
		var err error
		data, err = os.ReadFile(p) //nolint:gosec // operator-supplied path
		if err != nil {
			return nil, fmt.Errorf("awg tunnel %q: %w", t.Name, err)
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
			warn = append(warn, fmt.Sprintf("%s is readable by other users (mode %o); it holds the private key, chmod 600 it", p, fi.Mode().Perm()))
		}
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("awg tunnel %q: %w", t.Name, err)
	}
	c.Warnings = append(warn, c.Warnings...)
	return c, nil
}

// Desired builds the desired router interface from the tunnel settings and
// its configuration.
func (t Tunnel) Desired(iface string, c *Conf) router.WGInterface {
	w := router.WGInterface{
		Name:          iface,
		Description:   t.Description,
		SecurityLevel: t.SecurityLevel,
		Address:       c.IPv4Address(),
		MTU:           c.MTU,
		Global:        t.IPGlobal,
		AdjustMSS:     t.AdjustMSS == nil || *t.AdjustMSS,
		ListenPort:    c.ListenPort,
		ASC:           c.ASCValues(),
		Up:            t.IsUp(),
		PrivateKey:    c.PrivateKey,
	}
	if w.Description == "" {
		w.Description = "awgkeen: " + t.Name
	}
	if w.SecurityLevel == "" {
		w.SecurityLevel = "public"
	}
	if t.MTU != 0 {
		w.MTU = t.MTU
	}
	keepalive := 25
	if t.Keepalive != nil {
		keepalive = *t.Keepalive
	}
	for _, p := range c.Peers {
		wp := router.WGPeer{
			PublicKey: p.PublicKey, PresharedKey: p.PresharedKey, Endpoint: p.Endpoint,
			AllowedIPs: p.AllowedIPs, Keepalive: p.Keepalive,
		}
		if wp.Keepalive == 0 {
			wp.Keepalive = keepalive
		}
		if len(t.AllowedIPs) > 0 {
			wp.AllowedIPs = t.AllowedIPs
		}
		wp.AllowedIPs = ipv4Only(wp.AllowedIPs)
		if len(wp.AllowedIPs) == 0 {
			wp.AllowedIPs = []string{"0.0.0.0/0"}
		}
		w.Peers = append(w.Peers, wp)
	}
	return w
}

// ipv4Only drops IPv6 prefixes: the router interface carries IPv4 only.
func ipv4Only(in []string) []string {
	var out []string
	for _, a := range in {
		if !strings.Contains(a, ":") {
			out = append(out, a)
		}
	}
	return out
}
