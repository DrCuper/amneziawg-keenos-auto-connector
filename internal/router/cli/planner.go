package cli

import "github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"

// Planner adapts this package to router.Planner.
type Planner struct{}

func (Planner) Parse(text string) *router.State                           { return Parse(text) }
func (Planner) Diff(cur *router.State, want *router.Desired) *router.Plan { return Diff(cur, want) }
func (Planner) SaveCommand() string                                       { return Save }
