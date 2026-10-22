package router

import (
	"errors"
	"fmt"
	"strings"
)

// Change kinds: what Change.Group names.
const (
	KindGroup     = ""          // an object-group fqdn
	KindInterface = "interface" // a Wireguard interface
)

// Change is a single human-readable line of the diff, e.g. "+ include a.com".
type Change struct {
	Group string
	Text  string
	// Kind is KindGroup or KindInterface.
	Kind string
	// WriteOnly marks changes the router cannot show back (keys): they are
	// re-sent while requested and never fail verification.
	WriteOnly bool
}

// Plan is an ordered list of CLI commands plus the diff they implement.
type Plan struct {
	Commands []string
	Changes  []Change
	// secrets are values that must never appear in output, logs or errors.
	secrets []string
}

// Empty reports whether applying the plan would do nothing.
func (p *Plan) Empty() bool { return p == nil || len(p.Commands) == 0 }

// Add records one diff line and the commands implementing it.
func (p *Plan) Add(group, text string, cmds ...string) {
	p.Changes = append(p.Changes, Change{Group: group, Text: text})
	p.Commands = append(p.Commands, cmds...)
}

// AddInterface records a change of a Wireguard interface.
func (p *Plan) AddInterface(name, text string, cmds ...string) {
	p.Changes = append(p.Changes, Change{Group: name, Text: text, Kind: KindInterface})
	p.Commands = append(p.Commands, cmds...)
}

// AddSecret records a write-only interface change whose command carries a
// secret value; the value is masked everywhere the plan is rendered.
func (p *Plan) AddSecret(name, text, secret, cmd string) {
	p.Changes = append(p.Changes, Change{Group: name, Text: text, Kind: KindInterface, WriteOnly: true})
	p.Commands = append(p.Commands, cmd)
	p.secrets = append(p.secrets, secret)
}

// Verifiable reports whether the plan contains changes the router can show
// back; a re-plan after apply with only write-only changes is not a failure.
func (p *Plan) Verifiable() bool {
	if p.Empty() {
		return false
	}
	for _, c := range p.Changes {
		if !c.WriteOnly {
			return true
		}
	}
	return false
}

// Redact masks the plan's secrets in s.
func (p *Plan) Redact(s string) string {
	if p == nil {
		return s
	}
	for _, v := range p.secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "<secret>")
		}
	}
	return s
}

// RedactedCommands returns the commands with secrets masked.
func (p *Plan) RedactedCommands() []string {
	out := make([]string, len(p.Commands))
	for i, c := range p.Commands {
		out[i] = p.Redact(c)
	}
	return out
}

// RedactErr wraps err so that its message has the plan's secrets masked;
// errors.Is/As still see the original error.
func (p *Plan) RedactErr(err error) error {
	if err == nil || p == nil || len(p.secrets) == 0 {
		return err
	}
	msg := p.Redact(err.Error())
	if msg == err.Error() {
		return err
	}
	var ce *CommandError
	if errors.As(err, &ce) {
		// Keep the type callers inspect, with a masked copy.
		return &redacted{msg: msg, err: &CommandError{Command: p.Redact(ce.Command), Message: p.Redact(ce.Message)}}
	}
	return &redacted{msg: msg, err: err}
}

type redacted struct {
	msg string
	err error
}

func (r *redacted) Error() string { return r.msg }
func (r *redacted) Unwrap() error { return r.err }

// String renders the plan for dry-run output.
func (p *Plan) String() string {
	if p.Empty() {
		return "no changes\n"
	}
	var b strings.Builder
	cur := "\x00"
	for _, c := range p.Changes {
		if key := c.Kind + "|" + c.Group; key != cur {
			cur = key
			if c.Kind == KindInterface {
				fmt.Fprintf(&b, "interface %s:\n", c.Group)
			} else {
				fmt.Fprintf(&b, "group %s:\n", c.Group)
			}
		}
		fmt.Fprintf(&b, "  %s\n", c.Text)
	}
	fmt.Fprintf(&b, "\n%d command(s):\n", len(p.Commands))
	for _, c := range p.RedactedCommands() {
		fmt.Fprintf(&b, "  %s\n", c)
	}
	return b.String()
}
