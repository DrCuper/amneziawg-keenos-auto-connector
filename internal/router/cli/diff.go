package cli

import (
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// Diff computes the plan that moves the router from cur to want.
//
// Order of operations, per spec: create/update Wireguard interfaces → create groups and set entries → routes →
// remove routes/groups that are no longer wanted → remove interfaces. Entries are compared as sets,
// so the router's ordering never produces spurious changes.
func Diff(cur *router.State, want *router.Desired) *router.Plan {
	p := &router.Plan{}
	// Interfaces first: routes below may point at a new interface.
	diffInterfaces(p, cur, want)
	wanted := map[string]bool{}

	for _, dg := range want.Groups {
		wanted[dg.Name] = true
		cg, exists := cur.Group(dg.Name)
		if !exists {
			p.Add(dg.Name, "+ object-group fqdn "+dg.Name, CreateGroup(dg.Name))
		}
		if dg.Description != cg.Description {
			if dg.Description == "" {
				p.Add(dg.Name, "- description "+Quote(cg.Description), ClearDescription(dg.Name))
			} else {
				p.Add(dg.Name, "~ description "+Quote(dg.Description), SetDescription(dg.Name, dg.Description))
			}
		}
		diffEntries(p, dg.Name, "include", cg.Include, dg.Include,
			func(e string) string { return Include(dg.Name, e) },
			func(e string) string { return NoInclude(dg.Name, e) })
		diffEntries(p, dg.Name, "exclude", cg.Exclude, dg.Exclude,
			func(e string) string { return Exclude(dg.Name, e) },
			func(e string) string { return NoExclude(dg.Name, e) })
	}

	// Routes for managed groups are owned entirely by the desired state.
	curRoutes := map[string][]router.DNSRoute{}
	for _, r := range cur.Routes {
		curRoutes[r.Group] = append(curRoutes[r.Group], r)
	}
	for _, dg := range want.Groups {
		have := map[string]router.DNSRoute{}
		for _, r := range curRoutes[dg.Name] {
			have[r.Key()] = r
		}
		keep := map[string]bool{}
		for _, r := range dg.Routes {
			keep[r.Key()] = true
			old, ok := have[r.Key()]
			switch {
			case !ok:
				p.Add(dg.Name, "+ route "+routeText(r), AddRoute(r))
			case old.Auto != r.Auto || old.Reject != r.Reject:
				p.Add(dg.Name, "~ route "+routeText(r), RemoveRoute(old), AddRoute(r))
			}
		}
		for _, r := range curRoutes[dg.Name] {
			if !keep[r.Key()] {
				p.Add(dg.Name, "- route "+routeText(r), RemoveRoute(r))
			}
		}
	}

	if want.Prune {
		for _, cg := range cur.Groups {
			if wanted[cg.Name] {
				continue
			}
			for _, r := range curRoutes[cg.Name] {
				p.Add(cg.Name, "- route "+routeText(r), RemoveRoute(r))
			}
			p.Add(cg.Name, "- object-group fqdn "+cg.Name, DeleteGroup(cg.Name))
		}
	}
	removeInterfaces(p, cur, want.RemoveInterfaces)
	return p
}

func diffEntries(p *router.Plan, group, kind string, cur, want []string, add, del func(string) string) {
	have := map[string]bool{}
	for _, e := range cur {
		have[e] = true
	}
	wantSet := map[string]bool{}
	for _, e := range want {
		wantSet[e] = true
		if !have[e] {
			p.Add(group, "+ "+kind+" "+e, add(e))
		}
	}
	for _, e := range cur {
		if !wantSet[e] {
			p.Add(group, "- "+kind+" "+e, del(e))
		}
	}
}

func routeText(r router.DNSRoute) string {
	s := r.Destination()
	if r.Auto {
		s += " auto"
	}
	if r.Reject {
		s += " reject"
	}
	return s
}
