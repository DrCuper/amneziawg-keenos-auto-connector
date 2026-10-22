// Package fake is an in-memory Transport that behaves like a Keenetic router
// for the subset of CLI this service uses. It records every command, can
// inject transport or command failures, and re-renders its running-config
// after each mutation so that read-after-write verification is exercised.
package fake

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
)

// Transport is the fake. Zero value is a router with an empty configuration.
type Transport struct {
	mu sync.Mutex

	// Extra is arbitrary running-config text prepended to the rendered state
	// (e.g. "system\n    hostname x\n").
	Extra string
	// State is the router's current configuration.
	State router.State
	// Saved counts `system configuration save` calls.
	Saved int
	// Commands is every mutating command received, in order.
	Commands []string
	// Reads counts RunningConfig calls.
	Reads int
	// ShowKeys makes running-config print private and preshared keys (most
	// firmwares do not).
	ShowKeys bool
	// Status answers InterfaceStatus by interface name.
	Status map[string]*router.WGStatus
	// Closed is set by Close.
	Closed bool

	// FailReads makes the next N RunningConfig calls return a TransportError.
	FailReads int
	// FailExec makes the next N Exec calls return a TransportError before
	// executing anything.
	FailExec int
	// RejectCommand, when matched exactly, makes Exec return a CommandError.
	RejectCommand string
	// Ignore lists commands that are accepted but have no effect (simulates
	// Keenetic silent no-ops).
	Ignore map[string]bool
}

var _ router.Transport = (*Transport)(nil)

// New returns a fake pre-loaded with the given running-config.
func New(runningConfig string) *Transport {
	st := cli.Parse(runningConfig)
	return &Transport{State: *st}
}

func (f *Transport) RunningConfig(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reads++
	if f.FailReads > 0 {
		f.FailReads--
		return "", &router.TransportError{Op: "running-config", Err: fmt.Errorf("fake network failure")}
	}
	return f.render(), nil
}

func (f *Transport) Exec(ctx context.Context, commands []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailExec > 0 {
		f.FailExec--
		return nil, &router.TransportError{Op: "exec", Err: fmt.Errorf("fake network failure")}
	}
	var out []string
	for _, c := range commands {
		if c == f.RejectCommand {
			return out, &router.CommandError{Command: c, Message: "fake: command rejected"}
		}
		f.Commands = append(f.Commands, c)
		if f.Ignore[c] {
			out = append(out, "")
			continue
		}
		if err := f.apply(c); err != nil {
			return out, err
		}
		out = append(out, "")
	}
	return out, nil
}

func (f *Transport) Close() error { f.Closed = true; return nil }

// Render returns the current running-config text.
func (f *Transport) Render() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.render()
}

func (f *Transport) render() string {
	var b strings.Builder
	b.WriteString(f.Extra)
	for _, g := range f.State.Groups {
		fmt.Fprintf(&b, "object-group fqdn %s\n", g.Name)
		if g.Description != "" {
			fmt.Fprintf(&b, "    description %s\n", cli.Quote(g.Description))
		}
		for _, e := range g.Include {
			fmt.Fprintf(&b, "    include %s\n", e)
		}
		for _, e := range g.Exclude {
			fmt.Fprintf(&b, "    exclude %s\n", e)
		}
	}
	f.renderInterfaces(&b)
	if len(f.State.Routes) > 0 {
		b.WriteString("dns-proxy\n")
		for _, r := range f.State.Routes {
			line := strings.TrimPrefix(cli.AddRoute(r), "dns-proxy ")
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return b.String()
}

// apply interprets the one-line CLI forms produced by package cli.
func (f *Transport) apply(c string) error {
	if c == cli.Save {
		f.Saved++
		return nil
	}
	fields := cli.Fields(c)
	neg := false
	if len(fields) > 0 && fields[0] == "no" {
		neg = true
		fields = fields[1:]
	}
	switch {
	case len(fields) >= 2 && fields[0] == "interface":
		return f.applyInterface(neg, fields[1], fields[2:], c)
	case len(fields) >= 3 && fields[0] == "object-group" && fields[1] == "fqdn":
		return f.applyGroup(neg, fields[2], fields[3:], c)
	case len(fields) >= 4 && fields[0] == "dns-proxy" && fields[1] == "route" && fields[2] == "object-group":
		tmp := cli.Parse("dns-proxy " + strings.Join(fields[1:], " "))
		if len(tmp.Routes) != 1 {
			return &router.CommandError{Command: c, Message: "fake: cannot parse route"}
		}
		r := tmp.Routes[0]
		if _, ok := f.group(r.Group); !ok {
			return &router.CommandError{Command: c, Message: fmt.Sprintf("object-group %q not found", r.Group)}
		}
		kept := f.State.Routes[:0]
		for _, x := range f.State.Routes {
			if x.Key() != r.Key() {
				kept = append(kept, x)
			}
		}
		f.State.Routes = kept
		if !neg {
			f.State.Routes = append(f.State.Routes, r)
		}
		return nil
	}
	return &router.CommandError{Command: c, Message: "fake: unknown command"}
}

func (f *Transport) group(name string) (*router.FQDNGroup, bool) {
	for i := range f.State.Groups {
		if f.State.Groups[i].Name == name {
			return &f.State.Groups[i], true
		}
	}
	return nil, false
}

func (f *Transport) applyGroup(neg bool, name string, rest []string, raw string) error {
	g, ok := f.group(name)
	if len(rest) == 0 {
		if neg {
			if !ok {
				return &router.CommandError{Command: raw, Message: "no such group"}
			}
			kept := f.State.Groups[:0]
			for _, x := range f.State.Groups {
				if x.Name != name {
					kept = append(kept, x)
				}
			}
			f.State.Groups = kept
			keptR := f.State.Routes[:0]
			for _, r := range f.State.Routes {
				if r.Group != name {
					keptR = append(keptR, r)
				}
			}
			f.State.Routes = keptR
			return nil
		}
		if !ok {
			f.State.Groups = append(f.State.Groups, router.FQDNGroup{Name: name})
		}
		return nil
	}
	if !ok {
		return &router.CommandError{Command: raw, Message: "no such group"}
	}
	switch rest[0] {
	case "description":
		if neg {
			g.Description = ""
		} else {
			g.Description = strings.Join(rest[1:], " ")
		}
	case "include", "exclude":
		if len(rest) < 2 {
			return &router.CommandError{Command: raw, Message: "missing argument"}
		}
		list := &g.Include
		if rest[0] == "exclude" {
			list = &g.Exclude
		}
		*list = without(*list, rest[1])
		if !neg {
			*list = append(*list, rest[1])
		}
	default:
		return &router.CommandError{Command: raw, Message: "fake: unknown sub-command"}
	}
	return nil
}

func without(list []string, e string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x != e {
			out = append(out, x)
		}
	}
	return out
}
