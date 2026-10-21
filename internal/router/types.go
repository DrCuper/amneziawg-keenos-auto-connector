// Package router contains the domain model of a Keenetic router as seen by
// this service: FQDN object groups, DNS-proxy routes, the desired state the
// service wants, and the plan (diff) that turns one into the other.
//
// Everything in this file is transport-agnostic and pure.
package router

import (
	"errors"
	"fmt"
	"strings"
)

// FQDNGroup is an `object-group fqdn <name>` section of running-config.
type FQDNGroup struct {
	Name        string
	Description string
	Include     []string // domains or CIDRs, in router order
	Exclude     []string
}

// DNSRoute is one `dns-proxy route object-group ...` rule.
//
// Exactly one of Interface or Gateway is set as the destination; Gateway may
// additionally carry a Via interface (`route object-group G 10.0.0.1 ISP auto`).
type DNSRoute struct {
	Group     string
	Interface string
	Gateway   string
	Via       string
	Auto      bool
	Reject    bool
}

// Key identifies a route for diffing: group + destination. Flags are compared
// separately so a flag change is "replace", not "add another".
func (r DNSRoute) Key() string {
	return r.Group + "|" + r.Destination()
}

// Destination renders the destination tokens exactly as the CLI expects.
func (r DNSRoute) Destination() string {
	if r.Interface != "" {
		return r.Interface
	}
	if r.Via != "" {
		return r.Gateway + " " + r.Via
	}
	return r.Gateway
}

// State is the parsed running-config, restricted to what this service manages.
type State struct {
	Groups []FQDNGroup
	Routes []DNSRoute
	// Raw is the full running-config text the state was parsed from. It is
	// kept for backups and `router show --raw`.
	Raw string
}

// Group returns the group with the given name, if present.
func (s *State) Group(name string) (FQDNGroup, bool) {
	for _, g := range s.Groups {
		if g.Name == name {
			return g, true
		}
	}
	return FQDNGroup{}, false
}

// DesiredGroup is what the service wants one group to look like on the router.
type DesiredGroup struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description,omitempty"`
	Include     []string   `yaml:"include,omitempty"`
	Exclude     []string   `yaml:"exclude,omitempty"`
	Routes      []DNSRoute `yaml:"-"` // filled from RoutesSpec by Normalize
	// RoutesSpec is the YAML-facing form of Routes.
	RoutesSpec []RouteSpec `yaml:"routes,omitempty"`
}

// RouteSpec is the YAML form of a DNSRoute for a desired group.
type RouteSpec struct {
	Interface string `yaml:"interface,omitempty"`
	Gateway   string `yaml:"gateway,omitempty"`
	Via       string `yaml:"via,omitempty"`
	Auto      *bool  `yaml:"auto,omitempty"` // default true
	Reject    bool   `yaml:"reject,omitempty"`
}

// Desired is the full desired state for the groups this service owns.
type Desired struct {
	Groups []DesiredGroup `yaml:"groups"`
	// Prune removes groups (and their routes) that exist on the router but are
	// not listed here. Off by default so a partial desired file never deletes
	// groups the operator created by hand.
	Prune bool `yaml:"prune,omitempty"`
}

// Normalize fills Routes from RoutesSpec, trims and lowercases entries,
// deduplicates them (keeping first occurrence) and validates everything.
func (d *Desired) Normalize(limits Limits) error {
	var errs []error
	seen := map[string]bool{}
	for i := range d.Groups {
		g := &d.Groups[i]
		g.Name = strings.TrimSpace(g.Name)
		if err := ValidateGroupName(g.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		if seen[g.Name] {
			errs = append(errs, fmt.Errorf("group %q: listed twice", g.Name))
			continue
		}
		seen[g.Name] = true
		g.Include = normalizeEntries(g.Include)
		g.Exclude = normalizeEntries(g.Exclude)
		for _, e := range append(append([]string{}, g.Include...), g.Exclude...) {
			if err := ValidateEntry(e); err != nil {
				errs = append(errs, fmt.Errorf("group %q: %w", g.Name, err))
			}
		}
		if n := len(g.Include) + len(g.Exclude); limits.MaxEntries > 0 && n > limits.MaxEntries {
			errs = append(errs, fmt.Errorf("group %q: %d entries exceed limit %d", g.Name, n, limits.MaxEntries))
		}
		g.Routes = g.Routes[:0]
		for _, rs := range g.RoutesSpec {
			r := DNSRoute{Group: g.Name, Interface: strings.TrimSpace(rs.Interface), Gateway: strings.TrimSpace(rs.Gateway), Via: strings.TrimSpace(rs.Via), Reject: rs.Reject, Auto: true}
			if rs.Auto != nil {
				r.Auto = *rs.Auto
			}
			if (r.Interface == "") == (r.Gateway == "") {
				errs = append(errs, fmt.Errorf("group %q: route must set exactly one of interface or gateway", g.Name))
				continue
			}
			if r.Via != "" && r.Gateway == "" {
				errs = append(errs, fmt.Errorf("group %q: route 'via' is only valid with gateway", g.Name))
				continue
			}
			g.Routes = append(g.Routes, r)
		}
	}
	return errors.Join(errs...)
}

// Limits holds operator-tunable sanity bounds.
type Limits struct {
	// MaxEntries caps include+exclude per group; 0 disables the check.
	MaxEntries int
}

func normalizeEntries(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		e = strings.TrimSuffix(e, ".")
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

// ValidateGroupName enforces what Keenetic accepts as an object-group name:
// non-empty, no whitespace, printable ASCII.
func ValidateGroupName(name string) error {
	if name == "" {
		return errors.New("group name is empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("group name %q is longer than 64 characters", name)
	}
	for _, c := range name {
		if c <= ' ' || c > '~' || c == '"' || c == '\'' {
			return fmt.Errorf("group name %q contains invalid character %q", name, c)
		}
	}
	return nil
}

// ValidateEntry accepts a domain name (optionally wildcard-free, dot-separated
// labels) or an IPv4/IPv6 CIDR/address — both are allowed by
// `object-group fqdn include` since KeeneticOS 5.0 beta 3.
func ValidateEntry(e string) error {
	if e == "" {
		return errors.New("empty entry")
	}
	if strings.ContainsAny(e, " \t\"'") {
		return fmt.Errorf("entry %q contains whitespace or quotes", e)
	}
	if isIPOrCIDR(e) {
		return nil
	}
	if len(e) > 253 {
		return fmt.Errorf("entry %q is longer than 253 characters", e)
	}
	for _, label := range strings.Split(e, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("entry %q has an empty or too long label", e)
		}
		for i, c := range label {
			ok := c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c > 127
			if !ok || (c == '-' && (i == 0 || i == len(label)-1)) {
				return fmt.Errorf("entry %q is not a valid domain name", e)
			}
		}
	}
	return nil
}
