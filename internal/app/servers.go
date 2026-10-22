package app

import (
	"context"
	"fmt"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/server"
)

var peerKinds = map[string]string{
	server.PeerAdd: journal.KindPeerAdd, server.PeerRemove: journal.KindPeerRemove, server.PeerRotate: journal.KindPeerRotate,
	server.PeerDisable: journal.KindPeerDisable, server.PeerEnable: journal.KindPeerEnable,
}

// PeerOp changes a client of a managed server, records it in the journal
// and, with deploy, pushes the new configuration to the host. The peer is
// returned for add and rotate. A failed deploy leaves the change in the
// state: the next deploy applies it.
func (a *App) PeerOp(ctx context.Context, trigger, srv, op, peer string, deploy bool) (*server.Peer, *server.DeployPlan, error) {
	kind, ok := peerKinds[op]
	if !ok {
		return nil, nil, fmt.Errorf("unknown peer operation %q", op)
	}
	m, err := a.Server(srv)
	if err != nil {
		return nil, nil, err
	}
	started := time.Now()
	var p *server.Peer
	switch op {
	case server.PeerAdd:
		p, err = m.AddPeer(ctx, peer)
	case server.PeerRemove:
		err = m.RemovePeer(ctx, peer)
	case server.PeerRotate:
		p, err = m.RotatePeer(ctx, peer)
	case server.PeerDisable, server.PeerEnable:
		err = m.SetDisabled(ctx, peer, op == server.PeerDisable)
	}
	a.Record(journal.FromServer(kind, trigger, srv, peer, started, false, nil, err))
	if err != nil || !deploy {
		return p, nil, err
	}
	plan, err := a.DeployServer(ctx, trigger, srv, false)
	if err != nil {
		return p, plan, fmt.Errorf("peer %s: %s done, but deploy failed (run `awgkeen server deploy`): %w", peer, op, err)
	}
	return p, plan, nil
}

// DeployServer writes the server configuration to the host (or plans it)
// and records the result.
func (a *App) DeployServer(ctx context.Context, trigger, srv string, dryRun bool) (*server.DeployPlan, error) {
	m, err := a.Server(srv)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	plan, err := m.Deploy(ctx, dryRun)
	var changes []string
	if plan != nil {
		changes = plan.Changes
	}
	if err != nil || dryRun || len(changes) > 0 {
		a.Record(journal.FromServer(journal.KindServerDeploy, trigger, srv, "", started, dryRun, changes, err))
	}
	return plan, err
}

// InitServer generates the server identity and records it.
func (a *App) InitServer(ctx context.Context, trigger, srv string, force bool) (*server.State, error) {
	m, err := a.Server(srv)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	st, err := m.Init(ctx, force)
	a.Record(journal.FromServer(journal.KindServerInit, trigger, srv, "", started, false, nil, err))
	return st, err
}
