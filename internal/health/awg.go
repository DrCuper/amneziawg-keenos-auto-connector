package health

import (
	"context"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
)

// TunnelStatuser is the part of awg.Manager used by AWGCheck.
type TunnelStatuser interface {
	Status(ctx context.Context, t awg.Tunnel) awg.Status
}

// AWGCheck is the built-in tunnel check: the interface exists, is up and
// has a recent handshake. It reports under awg_down:<tunnel>.
type AWGCheck struct {
	Tunnel  awg.Tunnel
	Tunnels TunnelStatuser
}

// Signal implements Check.
func (c AWGCheck) Signal() notify.Signal {
	return notify.Signal{Alert: notify.AlertAWGDown, Check: c.Tunnel.Name}
}

// Run implements Check.
func (c AWGCheck) Run(ctx context.Context) error {
	return c.Tunnels.Status(ctx, c.Tunnel).Err
}
