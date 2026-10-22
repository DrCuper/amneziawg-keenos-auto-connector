package awg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// KeyStore keeps client keys imported at runtime (`awgkeen awg import`, the
// API and the web UI) as <dir>/<tunnel>.conf files with mode 0600. Each file
// is a tunnel with default settings.
type KeyStore struct {
	Dir string
}

func (k KeyStore) path(name string) string { return filepath.Join(k.Dir, name+".conf") }

// Save validates the key (.conf or vpn://) and stores it. An existing key is
// replaced only with replace set.
func (k KeyStore) Save(name string, data []byte, replace bool) (*Conf, error) {
	t := Tunnel{Name: name, ConfFile: k.path(name)}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	data = []byte(strings.TrimSpace(string(data)) + "\n")
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return nil, err
	}
	p := k.path(name)
	if _, err := os.Stat(p); err == nil && !replace {
		return nil, fmt.Errorf("awg tunnel %q already has an imported key (replace it explicitly)", name)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, p); err != nil {
		return nil, err
	}
	return c, nil
}

// Delete removes a stored key.
func (k KeyStore) Delete(name string) error {
	if !tunnelName.MatchString(name) {
		return fmt.Errorf("bad tunnel name %q", name)
	}
	err := os.Remove(k.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("awg tunnel %q has no imported key", name)
	}
	return err
}

// Has reports whether a key is stored for the tunnel.
func (k KeyStore) Has(name string) bool {
	if !tunnelName.MatchString(name) {
		return false
	}
	_, err := os.Stat(k.path(name))
	return err == nil
}

// Tunnels returns the stored keys as tunnels, sorted by name.
func (k KeyStore) Tunnels() ([]Tunnel, error) {
	ents, err := os.ReadDir(k.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Tunnel
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".conf")
		if !ok || e.IsDir() || !tunnelName.MatchString(name) {
			continue
		}
		out = append(out, Tunnel{Name: name, ConfFile: k.path(name), Imported: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
