package router

import (
	"fmt"
	"strings"
)

// Change is a single human-readable line of the diff, e.g. "+ include a.com".
type Change struct {
	Group string
	Text  string
}

// Plan is an ordered list of CLI commands plus the diff they implement.
type Plan struct {
	Commands []string
	Changes  []Change
}

// Empty reports whether applying the plan would do nothing.
func (p *Plan) Empty() bool { return p == nil || len(p.Commands) == 0 }

// Add records one diff line and the commands implementing it.
func (p *Plan) Add(group, text string, cmds ...string) {
	p.Changes = append(p.Changes, Change{Group: group, Text: text})
	p.Commands = append(p.Commands, cmds...)
}

// String renders the plan for dry-run output.
func (p *Plan) String() string {
	if p.Empty() {
		return "no changes\n"
	}
	var b strings.Builder
	cur := ""
	for _, c := range p.Changes {
		if c.Group != cur {
			cur = c.Group
			fmt.Fprintf(&b, "group %s:\n", cur)
		}
		fmt.Fprintf(&b, "  %s\n", c.Text)
	}
	fmt.Fprintf(&b, "\n%d command(s):\n", len(p.Commands))
	for _, c := range p.Commands {
		fmt.Fprintf(&b, "  %s\n", c)
	}
	return b.String()
}
