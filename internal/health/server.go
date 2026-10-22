package health

import (
	"context"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
)

// ServerChecker is the part of server.Manager used by ServerCheck.
type ServerChecker interface {
	Check(ctx context.Context) error
}

// ServerCheck is the built-in check of a managed AmneziaWG server: the
// host is reachable and its interface is up. It reports under
// server_down:<server>.
type ServerCheck struct {
	Name   string
	Server ServerChecker
}

// Signal implements Check.
func (c ServerCheck) Signal() notify.Signal {
	return notify.Signal{Alert: notify.AlertServerDown, Check: c.Name}
}

// Run implements Check.
func (c ServerCheck) Run(ctx context.Context) error { return c.Server.Check(ctx) }
