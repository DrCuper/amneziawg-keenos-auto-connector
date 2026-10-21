package router

import "context"

// Transport executes KeeneticOS CLI commands. RCI (via `parse`), SSH and the
// test fake all implement it, so the client, the parser and the planner never
// care which one is in use.
type Transport interface {
	// RunningConfig returns the router's running configuration as CLI text.
	RunningConfig(ctx context.Context) (string, error)
	// Exec runs the commands in order and returns the router's text output for
	// each. It MUST return a *CommandError when the router rejects a command
	// and a *TransportError for anything network-related. Execution stops at
	// the first error; outputs for the commands that did run are returned.
	Exec(ctx context.Context, commands []string) ([]string, error)
	// Close releases connections/sessions.
	Close() error
}
