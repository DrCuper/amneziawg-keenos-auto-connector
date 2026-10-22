// Package server manages an AmneziaWG server on a Linux host (a VPS or the
// machine awgkeen runs on): it generates the server identity and
// obfuscation parameters, keeps the list of peers with their addresses and
// keys, renders the awg-quick configuration and deploys it over SSH, and
// issues client configurations as .conf, QR code or Amnezia vpn:// key —
// without the Amnezia application.
package server

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// Config is one `servers[]` entry of the config file.
type Config struct {
	Name string `yaml:"name"`
	// Local manages the machine awgkeen runs on instead of SSH.
	Local bool `yaml:"local"`
	// SSH access (root, or a user with passwordless sudo and Sudo set).
	Host            string `yaml:"host"`
	User            string `yaml:"user"`
	Password        string `yaml:"password"`
	KeyFile         string `yaml:"key_file"`
	KnownHostsFile  string `yaml:"known_hosts_file"`
	InsecureHostKey bool   `yaml:"insecure_host_key"`
	Sudo            bool   `yaml:"sudo"`

	// Interface is the awg-quick interface name (default awg0).
	Interface string `yaml:"interface"`
	// ConfigPath defaults to /etc/amnezia/amneziawg/<interface>.conf.
	ConfigPath string `yaml:"config_path"`
	// Endpoint is the public host[:port] written into client configs.
	Endpoint   string `yaml:"endpoint"`
	ListenPort int    `yaml:"listen_port"` // default 51820
	// Subnet is the tunnel network; the server takes the first address.
	Subnet string `yaml:"subnet"` // default 10.8.1.0/24
	// NATInterface is the uplink for MASQUERADE; empty = detect the default
	// route's device on the server.
	NATInterface string `yaml:"nat_interface"`
	// Client config defaults.
	DNS        []string `yaml:"dns"`         // default 1.1.1.1, 1.0.0.1
	AllowedIPs []string `yaml:"allowed_ips"` // default 0.0.0.0/0
	MTU        int      `yaml:"mtu"`         // default 1376
	Keepalive  int      `yaml:"keepalive"`   // default 25
	// AWG2 generates AmneziaWG 2.0 parameters (S3, S4, H ranges) for new
	// servers; needs amneziawg >= 2.0 on the server and on every client.
	AWG2    bool          `yaml:"awg2"`
	Timeout time.Duration `yaml:"timeout"` // per remote command, default 2m
	// HealthCheck adds the `server_down:<name>` check to serve (default true).
	HealthCheck *bool `yaml:"health_check"`
}

// CheckEnabled reports whether serve checks the server.
func (c Config) CheckEnabled() bool { return c.HealthCheck == nil || *c.HealthCheck }

var (
	serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)
	ifaceName  = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`)
	safePath   = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	peerName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,63}$`)
)

// WithDefaults fills the defaults.
func (c Config) WithDefaults() Config {
	if c.Interface == "" {
		c.Interface = "awg0"
	}
	if c.ConfigPath == "" {
		c.ConfigPath = "/etc/amnezia/amneziawg/" + c.Interface + ".conf"
	}
	if c.ListenPort == 0 {
		c.ListenPort = 51820
	}
	if c.Subnet == "" {
		c.Subnet = "10.8.1.0/24"
	}
	if len(c.DNS) == 0 {
		c.DNS = []string{"1.1.1.1", "1.0.0.1"}
	}
	if len(c.AllowedIPs) == 0 {
		c.AllowedIPs = []string{"0.0.0.0/0"}
	}
	if c.MTU == 0 {
		c.MTU = 1376
	}
	if c.Keepalive == 0 {
		c.Keepalive = 25
	}
	if c.User == "" {
		c.User = "root"
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Minute
	}
	return c
}

// ClientEndpoint is Endpoint with the listen port added when missing.
func (c Config) ClientEndpoint() string {
	if strings.Contains(c.Endpoint, ":") && !strings.HasSuffix(c.Endpoint, "]") {
		return c.Endpoint
	}
	return fmt.Sprintf("%s:%d", c.Endpoint, c.ListenPort)
}

// Prefix returns the parsed subnet.
func (c Config) Prefix() (netip.Prefix, error) {
	p, err := netip.ParsePrefix(c.Subnet)
	if err != nil || !p.Addr().Is4() || p.Bits() > 30 || p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("server %q: subnet %q must be an IPv4 network /30 or larger", c.Name, c.Subnet)
	}
	return p, nil
}

// Validate checks one server (defaults applied).
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("server %q: "+format, append([]any{c.Name}, a...)...))
	}
	if !serverName.MatchString(c.Name) {
		errs = append(errs, fmt.Errorf("server name %q must match %s", c.Name, serverName))
	}
	if !c.Local {
		if c.Host == "" {
			bad("host is required (or local: true)")
		}
		if c.Password == "" && c.KeyFile == "" {
			bad("key_file or password is required")
		}
		if c.KnownHostsFile == "" && !c.InsecureHostKey {
			bad("known_hosts_file is required unless insecure_host_key is true")
		}
	}
	if !ifaceName.MatchString(c.Interface) {
		bad("interface %q is not a valid name", c.Interface)
	}
	if !safePath.MatchString(c.ConfigPath) || strings.Contains(c.ConfigPath, "..") {
		bad("config_path %q must be an absolute path without special characters", c.ConfigPath)
	}
	if c.NATInterface != "" && !ifaceName.MatchString(c.NATInterface) {
		bad("nat_interface %q is not a valid name", c.NATInterface)
	}
	if c.Endpoint == "" || strings.ContainsAny(c.Endpoint, " \t\"'/") {
		bad("endpoint (public host[:port] for clients) is required")
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		bad("listen_port out of range")
	}
	if _, err := c.Prefix(); err != nil {
		errs = append(errs, err)
	}
	for _, a := range c.AllowedIPs {
		if _, err := router.CanonicalPrefix(a); err != nil {
			bad("allowed_ips: %v", err)
		}
	}
	for _, d := range c.DNS {
		if _, err := netip.ParseAddr(d); err != nil {
			bad("dns %q is not an IP address", d)
		}
	}
	if c.MTU < 576 || c.MTU > 1500 {
		bad("mtu out of range 576..1500")
	}
	return errors.Join(errs...)
}

// ValidateAll applies defaults, validates and checks name uniqueness.
func ValidateAll(cs []Config) error {
	var errs []error
	seen := map[string]bool{}
	for _, c := range cs {
		c = c.WithDefaults()
		if err := c.Validate(); err != nil {
			errs = append(errs, err)
		}
		if seen[c.Name] {
			errs = append(errs, fmt.Errorf("server %q is listed twice", c.Name))
		}
		seen[c.Name] = true
	}
	return errors.Join(errs...)
}

// ValidatePeerName checks a peer name (used in file names and comments).
func ValidatePeerName(n string) error {
	if !peerName.MatchString(n) {
		return fmt.Errorf("peer name %q must match %s", n, peerName)
	}
	return nil
}
