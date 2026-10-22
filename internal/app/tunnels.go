package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
)

// ImportTunnel stores a client key (.conf or vpn://) of any AmneziaWG
// server as a tunnel and, with apply, imports it onto the router. replace
// swaps the key of an imported tunnel (keys are re-sent on apply). A failed
// apply keeps the key: `awg apply` retries it.
func (a *App) ImportTunnel(ctx context.Context, trigger, name string, key []byte, apply, replace bool) (*awg.Result, error) {
	m := a.Tunnels()
	if t, ok := m.Tunnel(name); ok && !t.Imported {
		return nil, fmt.Errorf("awg tunnel %q is defined in the config file; pick another name", name)
	}
	if _, ok := m.Tunnel(name); ok && !replace {
		return nil, fmt.Errorf("awg tunnel %q already exists (use replace to swap its key)", name)
	}
	if _, err := a.Keys().Save(name, key, replace); err != nil {
		return nil, err
	}
	t := awg.Tunnel{Name: name, ConfFile: filepath.Join(a.Keys().Dir, name+".conf"), Imported: true}
	if err := m.Put(t); err != nil {
		return nil, err
	}
	if !apply {
		return nil, nil
	}
	started := time.Now()
	res, err := m.Apply(ctx, []string{name}, awg.ApplyOptions{})
	if res != nil {
		e := journal.FromApply(started, time.Since(started), false, res.Apply, err, a.Config.Journal.MaxChanges)
		e.Kind, e.Trigger = journal.KindAWGApply, trigger
		a.Record(e)
	}
	if err != nil {
		return res, fmt.Errorf("key saved, but import onto the router failed (retry: awgkeen awg apply %s): %w", name, err)
	}
	return res, nil
}

// ForgetTunnel removes an imported tunnel: its interface on the router (when
// it was imported) and its stored key.
func (a *App) ForgetTunnel(ctx context.Context, trigger, name string) (*awg.Result, error) {
	m := a.Tunnels()
	t, ok := m.Tunnel(name)
	if !ok || !t.Imported {
		return nil, fmt.Errorf("awg tunnel %q is not an imported key (tunnels from the config file are removed there)", name)
	}
	recs, err := m.Records()
	if err != nil {
		return nil, err
	}
	var res *awg.Result
	if r, ok := recs[name]; ok && r.Interface != "" {
		started := time.Now()
		res, err = m.Remove(ctx, name, false)
		if res != nil {
			e := journal.FromApply(started, time.Since(started), false, res.Apply, err, a.Config.Journal.MaxChanges)
			e.Kind, e.Trigger = journal.KindAWGRemove, trigger
			a.Record(e)
		}
		if err != nil {
			return res, err
		}
	}
	if err := a.Keys().Delete(name); err != nil {
		return res, err
	}
	m.Drop(name)
	return res, nil
}
