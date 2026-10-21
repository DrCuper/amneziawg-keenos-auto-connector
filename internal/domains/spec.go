// Package domains describes domain groups (what the operator wants routed
// where), normalizes entries and builds a group's content from its sources.
// It is pure: fetching lives in package source, persistence in package store.
package domains

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/source"
)

// Spec is the domains.yaml file.
type Spec struct {
	// Connections map a name (awg, reality, ...) to the dns-proxy routes
	// every group bound to it gets.
	Connections map[string]Connection `yaml:"connections"`
	Groups      []Group               `yaml:"groups"`
	// Prune deletes groups on the router that are not listed here (see
	// router.Desired.Prune).
	Prune bool `yaml:"prune,omitempty"`

	// BaseDir is the directory of the file; relative `file:` sources resolve
	// against it. Set by Load.
	BaseDir string `yaml:"-"`
}

// Connection is a named set of routes.
type Connection struct {
	Description string             `yaml:"description,omitempty"`
	Routes      []router.RouteSpec `yaml:"routes"`
}

// Group is one object-group fqdn.
type Group struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description,omitempty"`
	Connection  string   `yaml:"connection"`
	Sources     []Source `yaml:"sources,omitempty"`
	// Include is always added; Remove is dropped from the result (never sent
	// to the router); Exclude is sent as `object-group fqdn … exclude`.
	Include []string `yaml:"include,omitempty"`
	Remove  []string `yaml:"remove,omitempty"`
	Exclude []string `yaml:"exclude,omitempty"`
	// MaxEntries overrides the global limit for this group (0 = global).
	MaxEntries int `yaml:"max_entries,omitempty"`
	// CollapseSubdomains drops entries whose parent domain is already listed
	// (default true: Keenetic matches subdomains anyway).
	CollapseSubdomains *bool `yaml:"collapse_subdomains,omitempty"`
}

// Collapse reports the effective CollapseSubdomains setting.
func (g Group) Collapse() bool { return g.CollapseSubdomains == nil || *g.CollapseSubdomains }

// Source is one list.
type Source struct {
	Name   string `yaml:"name,omitempty"`
	URL    string `yaml:"url,omitempty"`
	File   string `yaml:"file,omitempty"`
	Format string `yaml:"format,omitempty"` // auto|plain|hosts|dnsmasq|v2fly|clash
}

// Ref converts the source to a fetchable reference.
func (s Source) Ref(baseDir string) source.Ref {
	return source.Ref{URL: s.URL, File: s.File, BaseDir: baseDir}
}

// Label is the source name for stats and logs.
func (s Source) Label() string {
	if s.Name != "" {
		return s.Name
	}
	if s.URL != "" {
		return s.URL
	}
	return s.File
}

// Load reads and validates a domains.yaml file.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("read domains file: %w", err)
	}
	sp, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	sp.BaseDir = filepath.Dir(abs)
	return sp, nil
}

// Parse unmarshals and validates the YAML text.
func Parse(data []byte) (*Spec, error) {
	var sp Spec
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&sp); err != nil && err.Error() != "EOF" {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := sp.Validate(); err != nil {
		return nil, err
	}
	return &sp, nil
}

// Validate checks names, references and routes.
func (sp *Spec) Validate() error {
	var errs []error
	for name, c := range sp.Connections {
		if strings.TrimSpace(name) == "" {
			errs = append(errs, errors.New("connection with empty name"))
			continue
		}
		if len(c.Routes) == 0 {
			errs = append(errs, fmt.Errorf("connection %q: no routes", name))
		}
		// Reuse the stage-1 validation of route specs through a throw-away
		// desired group.
		d := router.Desired{Groups: []router.DesiredGroup{{Name: "x", RoutesSpec: c.Routes}}}
		if err := d.Normalize(router.Limits{}); err != nil {
			errs = append(errs, fmt.Errorf("connection %q: %w", name, err))
		}
	}
	seen := map[string]bool{}
	for i := range sp.Groups {
		g := &sp.Groups[i]
		g.Name = strings.TrimSpace(g.Name)
		if err := router.ValidateGroupName(g.Name); err != nil {
			errs = append(errs, fmt.Errorf("group #%d: %w", i+1, err))
			continue
		}
		if seen[g.Name] {
			errs = append(errs, fmt.Errorf("group %q: listed twice", g.Name))
			continue
		}
		seen[g.Name] = true
		g.Connection = strings.TrimSpace(g.Connection)
		if g.Connection == "" {
			errs = append(errs, fmt.Errorf("group %q: connection is required", g.Name))
		} else if _, ok := sp.Connections[g.Connection]; !ok {
			errs = append(errs, fmt.Errorf("group %q: unknown connection %q", g.Name, g.Connection))
		}
		for j, s := range g.Sources {
			if (s.URL == "") == (s.File == "") {
				errs = append(errs, fmt.Errorf("group %q: source #%d must set exactly one of url or file", g.Name, j+1))
			}
			if s.URL != "" && !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
				errs = append(errs, fmt.Errorf("group %q: source #%d: url must be http(s)", g.Name, j+1))
			}
			if _, err := source.ParseFormat(s.Format); err != nil {
				errs = append(errs, fmt.Errorf("group %q: source #%d: %w", g.Name, j+1, err))
			}
		}
		if g.MaxEntries < 0 {
			errs = append(errs, fmt.Errorf("group %q: max_entries must be >= 0", g.Name))
		}
	}
	return errors.Join(errs...)
}

// Group returns the group by name.
func (sp *Spec) Group(name string) (*Group, bool) {
	for i := range sp.Groups {
		if sp.Groups[i].Name == name {
			return &sp.Groups[i], true
		}
	}
	return nil, false
}

// Routes returns the route specs of the group's connection.
func (sp *Spec) Routes(g *Group) []router.RouteSpec {
	return sp.Connections[g.Connection].Routes
}
