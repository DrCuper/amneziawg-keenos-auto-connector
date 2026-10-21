package router

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	d := &Desired{Groups: []DesiredGroup{{
		Name:       " vpn ",
		Include:    []string{"A.com", "a.com.", " b.com ", "", "10.0.0.0/24"},
		Exclude:    []string{"X.A.COM"},
		RoutesSpec: []RouteSpec{{Interface: "Wireguard2"}, {Gateway: "10.0.0.1", Via: "ISP", Reject: true}},
	}}}
	if err := d.Normalize(Limits{MaxEntries: 10}); err != nil {
		t.Fatal(err)
	}
	g := d.Groups[0]
	if g.Name != "vpn" {
		t.Errorf("name %q", g.Name)
	}
	if want := []string{"a.com", "b.com", "10.0.0.0/24"}; strings.Join(g.Include, ",") != strings.Join(want, ",") {
		t.Errorf("include %q", g.Include)
	}
	if len(g.Routes) != 2 || !g.Routes[0].Auto || g.Routes[0].Interface != "Wireguard2" || g.Routes[1].Via != "ISP" || !g.Routes[1].Reject {
		t.Errorf("routes %+v", g.Routes)
	}
}

func TestNormalizeErrors(t *testing.T) {
	cases := []struct {
		name string
		d    Desired
		want string
	}{
		{"empty name", Desired{Groups: []DesiredGroup{{Name: ""}}}, "group name is empty"},
		{"space in name", Desired{Groups: []DesiredGroup{{Name: "a b"}}}, "invalid character"},
		{"dup", Desired{Groups: []DesiredGroup{{Name: "a"}, {Name: "a"}}}, "listed twice"},
		{"bad domain", Desired{Groups: []DesiredGroup{{Name: "a", Include: []string{"-bad.com"}}}}, "not a valid domain"},
		{"limit", Desired{Groups: []DesiredGroup{{Name: "a", Include: []string{"a.com", "b.com"}}}}, "exceed limit"},
		{"route both", Desired{Groups: []DesiredGroup{{Name: "a", RoutesSpec: []RouteSpec{{Interface: "x", Gateway: "1.1.1.1"}}}}}, "exactly one"},
		{"route none", Desired{Groups: []DesiredGroup{{Name: "a", RoutesSpec: []RouteSpec{{}}}}}, "exactly one"},
		{"via w/o gw", Desired{Groups: []DesiredGroup{{Name: "a", RoutesSpec: []RouteSpec{{Interface: "x", Via: "y"}}}}}, "only valid with gateway"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.d.Normalize(Limits{MaxEntries: 1})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestValidateEntry(t *testing.T) {
	ok := []string{"example.com", "sub.example.co.uk", "xn--d1acufc.xn--p1ai", "1.1.1.1", "10.0.0.0/8", "2001:db8::/32", "a_b.example"}
	for _, e := range ok {
		if err := ValidateEntry(e); err != nil {
			t.Errorf("%q: %v", e, err)
		}
	}
	bad := []string{"", "a b", "a..b", "-a.com", "a-.com", "a.com/x"}
	for _, e := range bad {
		if err := ValidateEntry(e); err == nil {
			t.Errorf("%q: expected error", e)
		}
	}
}
