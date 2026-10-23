package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/api"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
)

// Servers imported from Amnezia admin keys are registered as
// <state_dir>/servers/<name>.server.yaml (0600, holds the SSH password),
// with <name>.key for a private key and <name>.known_hosts recorded on the
// first connection.

func (a *App) serversDir() string { return filepath.Join(a.Config.StateDir, "servers") }

func (a *App) registration(name string) string {
	return filepath.Join(a.serversDir(), name+".server.yaml")
}

// importedServers reads the registered servers, sorted by name.
func (a *App) importedServers() []server.Config {
	files, _ := filepath.Glob(filepath.Join(a.serversDir(), "*.server.yaml"))
	sort.Strings(files)
	var out []server.Config
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // our own state directory
		if err != nil {
			a.Logger.Warn("server: cannot read registration", "file", f, "err", err)
			continue
		}
		var c server.Config
		if err := yaml.Unmarshal(data, &c); err != nil || c.Name+".server.yaml" != filepath.Base(f) {
			a.Logger.Warn("server: bad registration", "file", f, "err", err)
			continue
		}
		out = append(out, c)
	}
	return out
}

// Imported reports whether a server was imported from an admin key.
func (a *App) Imported(name string) bool {
	_, err := os.Stat(a.registration(name))
	return err == nil
}

// RegisterAdminKey records the server behind an Amnezia admin key. A server
// of the config file cannot be shadowed; an imported one is replaced only
// with replace (its host key is then learned again).
func (a *App) RegisterAdminKey(name string, k *server.AdminKey, replace bool) error {
	if _, ok := a.Config.Server(name); ok {
		return fmt.Errorf("server %q is defined in the config file; pick another name", name)
	}
	if a.Imported(name) && !replace {
		return fmt.Errorf("server %q is already imported (replace it explicitly)", name)
	}
	dir := a.serversDir()
	c := server.Config{
		Name: name, Host: k.SSHHost(), User: k.User, Endpoint: k.Host, Amnezia: true,
		KnownHostsFile: filepath.Join(dir, name+".known_hosts"), TrustOnFirstUse: true,
		Sudo: k.User != "root",
	}
	if k.PrivateKey() {
		c.KeyFile = filepath.Join(dir, name+".key")
	} else {
		c.Password = k.Password
	}
	if err := c.WithDefaults().Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if c.KeyFile != "" {
		if err := writeSecret(c.KeyFile, []byte(strings.TrimSpace(k.Password)+"\n")); err != nil {
			return err
		}
	}
	if replace {
		_ = os.Remove(c.KnownHostsFile)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := writeSecret(a.registration(name), data); err != nil {
		return err
	}
	a.dropServer(name)
	return nil
}

// ForgetServer removes an imported server's registration and credentials.
// The clients it issued keep working; importing the key again restores
// their management (the state file is kept).
func (a *App) ForgetServer(name string) error {
	if !a.Imported(name) {
		return fmt.Errorf("server %q was not imported from an admin key (servers of the config file are removed there)", name)
	}
	a.dropServer(name)
	dir := a.serversDir()
	for _, f := range []string{a.registration(name), filepath.Join(dir, name+".key"), filepath.Join(dir, name+".known_hosts")} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func writeSecret(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ImportKey adds what an Amnezia key gives access to: an admin key
// registers (and adopts) the server for issuing clients, a client key
// becomes a tunnel. The router is changed only with apply.
func (a *App) ImportKey(ctx context.Context, trigger, name string, key []byte, apply, replace bool) (*api.KeyImport, error) {
	if k, err := server.ParseAdminKey(string(key)); err == nil {
		out := &api.KeyImport{Kind: api.KeyServer, Name: name}
		if err := a.RegisterAdminKey(name, k, replace); err != nil {
			return nil, err
		}
		st, err := a.InitServer(ctx, trigger, name, true)
		if err != nil {
			return out, fmt.Errorf("server %s registered, but reading it failed (retry: awgkeen server init --force -s %s): %w", name, name, err)
		}
		out.Adopted, out.Endpoint = true, k.Host
		if st.Amnezia != nil {
			out.Endpoint = fmt.Sprintf("%s:%d", k.Host, st.Amnezia.ListenPort)
		}
		return out, nil
	}
	res, err := a.ImportTunnel(ctx, trigger, name, key, apply, replace)
	out := &api.KeyImport{Kind: api.KeyTunnel, Name: name, Applied: apply && err == nil}
	if res != nil {
		out.Items = res.Items
		if res.Apply != nil && res.Apply.Plan != nil {
			out.Plan = res.Apply.Plan.String()
		}
	}
	if err != nil && res == nil {
		return nil, err
	}
	return out, err
}
