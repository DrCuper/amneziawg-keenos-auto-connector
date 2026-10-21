package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

const sampleConfig = `! $$$ Model: Keenetic Ultra
! $$$ Version: 5.0.4
system
    hostname Keenetic
object-group fqdn test
    description "my list"
    include A.com
    include b.com
    exclude x.a.com
object-group fqdn other
    include c.com
object-group fqdn empty
dns-proxy
    rebind-protect auto
    route object-group test Wireguard2 auto reject
    route object-group test 10.0.0.1 ISP auto
    route object-group other OpkgTun10
    tls upstream 9.9.9.9
interface Wireguard2
    description awg
`

func TestParse(t *testing.T) {
	st := Parse(sampleConfig)
	wantGroups := []router.FQDNGroup{
		{Name: "test", Description: "my list", Include: []string{"a.com", "b.com"}, Exclude: []string{"x.a.com"}},
		{Name: "other", Include: []string{"c.com"}},
		{Name: "empty"},
	}
	if !reflect.DeepEqual(st.Groups, wantGroups) {
		t.Errorf("groups:\n got %+v\nwant %+v", st.Groups, wantGroups)
	}
	wantRoutes := []router.DNSRoute{
		{Group: "test", Interface: "Wireguard2", Auto: true, Reject: true},
		{Group: "test", Gateway: "10.0.0.1", Via: "ISP", Auto: true},
		{Group: "other", Interface: "OpkgTun10"},
	}
	if !reflect.DeepEqual(st.Routes, wantRoutes) {
		t.Errorf("routes:\n got %+v\nwant %+v", st.Routes, wantRoutes)
	}
	if st.Raw != sampleConfig {
		t.Error("raw config not preserved")
	}
}

func TestParseEmpty(t *testing.T) {
	st := Parse("system\n    hostname x\n")
	if len(st.Groups) != 0 || len(st.Routes) != 0 {
		t.Errorf("expected empty state, got %+v", st)
	}
}

func TestParseOneLineForms(t *testing.T) {
	st := Parse("object-group fqdn one include a.com\ndns-proxy route object-group one Wireguard0 auto\n")
	if len(st.Groups) != 1 || st.Groups[0].Include[0] != "a.com" {
		t.Errorf("groups: %+v", st.Groups)
	}
	if len(st.Routes) != 1 || st.Routes[0].Interface != "Wireguard0" || !st.Routes[0].Auto {
		t.Errorf("routes: %+v", st.Routes)
	}
}

func TestFields(t *testing.T) {
	got := Fields(`description "my  list" x`)
	want := []string{"description", "my  list", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func desired(groups ...router.DesiredGroup) *router.Desired {
	d := &router.Desired{Groups: groups}
	if err := d.Normalize(router.Limits{}); err != nil {
		panic(err)
	}
	return d
}

func TestDiffNewGroup(t *testing.T) {
	cur := Parse("")
	want := desired(router.DesiredGroup{
		Name: "vpn", Description: "my list", Include: []string{"a.com", "b.com"}, Exclude: []string{"x.a.com"},
		RoutesSpec: []router.RouteSpec{{Interface: "Wireguard2"}},
	})
	p := Diff(cur, want)
	wantCmds := []string{
		"object-group fqdn vpn",
		`object-group fqdn vpn description "my list"`,
		"object-group fqdn vpn include a.com",
		"object-group fqdn vpn include b.com",
		"object-group fqdn vpn exclude x.a.com",
		"dns-proxy route object-group vpn Wireguard2 auto",
	}
	if !reflect.DeepEqual(p.Commands, wantCmds) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(p.Commands, "\n"), strings.Join(wantCmds, "\n"))
	}
}

func TestDiffPartialUpdate(t *testing.T) {
	cur := Parse("object-group fqdn vpn\n    include a.com\n    include b.com\n")
	want := desired(router.DesiredGroup{Name: "vpn", Include: []string{"b.com", "c.com"}})
	p := Diff(cur, want)
	wantCmds := []string{
		"object-group fqdn vpn include c.com",
		"no object-group fqdn vpn include a.com",
	}
	if !reflect.DeepEqual(p.Commands, wantCmds) {
		t.Errorf("got %q want %q", p.Commands, wantCmds)
	}
}

func TestDiffNoChanges(t *testing.T) {
	cur := Parse(sampleConfig)
	want := desired(router.DesiredGroup{
		Name: "test", Description: "my list", Include: []string{"B.COM", "a.com"}, Exclude: []string{"x.a.com"},
		RoutesSpec: []router.RouteSpec{{Gateway: "10.0.0.1", Via: "ISP"}, {Interface: "Wireguard2", Reject: true}},
	})
	p := Diff(cur, want)
	if !p.Empty() {
		t.Errorf("expected empty plan, got:\n%s", p)
	}
}

func TestDiffRoutes(t *testing.T) {
	cur := Parse("object-group fqdn vpn\n    include a.com\ndns-proxy\n    route object-group vpn Wireguard1 auto\n    route object-group vpn Wireguard2\n")
	f := false
	want := desired(router.DesiredGroup{Name: "vpn", Include: []string{"a.com"},
		RoutesSpec: []router.RouteSpec{{Interface: "Wireguard2", Auto: &f, Reject: true}, {Interface: "Wireguard3"}}})
	p := Diff(cur, want)
	wantCmds := []string{
		"no dns-proxy route object-group vpn Wireguard2",
		"dns-proxy route object-group vpn Wireguard2 reject",
		"dns-proxy route object-group vpn Wireguard3 auto",
		"no dns-proxy route object-group vpn Wireguard1",
	}
	if !reflect.DeepEqual(p.Commands, wantCmds) {
		t.Errorf("got %q want %q", p.Commands, wantCmds)
	}
}

func TestDiffPrune(t *testing.T) {
	cur := Parse(sampleConfig)
	want := desired(router.DesiredGroup{Name: "test", Description: "my list", Include: []string{"a.com", "b.com"}, Exclude: []string{"x.a.com"},
		RoutesSpec: []router.RouteSpec{{Interface: "Wireguard2", Reject: true}, {Gateway: "10.0.0.1", Via: "ISP"}}})
	// without prune: untouched groups stay
	if p := Diff(cur, want); !p.Empty() {
		t.Errorf("expected empty plan without prune, got:\n%s", p)
	}
	want.Prune = true
	p := Diff(cur, want)
	wantCmds := []string{
		"no dns-proxy route object-group other OpkgTun10",
		"no object-group fqdn other",
		"no object-group fqdn empty",
	}
	if !reflect.DeepEqual(p.Commands, wantCmds) {
		t.Errorf("got %q want %q", p.Commands, wantCmds)
	}
}

func TestDiffDescription(t *testing.T) {
	cur := Parse("object-group fqdn vpn\n    description old\n")
	p := Diff(cur, desired(router.DesiredGroup{Name: "vpn"}))
	if got := p.Commands; len(got) != 1 || got[0] != "no object-group fqdn vpn description" {
		t.Errorf("got %q", got)
	}
}
