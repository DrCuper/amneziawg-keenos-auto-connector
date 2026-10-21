package domains

import (
	"errors"
	"strings"
	"testing"
)

const specYAML = `
connections:
  awg:
    routes:
      - interface: Wireguard0
  reality:
    routes:
      - gateway: 192.168.1.2
groups:
  - name: awg-main
    connection: awg
    sources:
      - url: https://example.com/list.txt
      - file: lists/extra.txt
        format: hosts
    include: [manual.example.com]
    remove: [bad.example.com]
    exclude: [cdn.example.com]
  - name: reality
    connection: reality
`

func TestParseSpec(t *testing.T) {
	sp, err := Parse([]byte(specYAML))
	if err != nil {
		t.Fatal(err)
	}
	g, ok := sp.Group("awg-main")
	if !ok || g.Connection != "awg" || len(g.Sources) != 2 {
		t.Fatalf("group: %+v", g)
	}
	if rs := sp.Routes(g); len(rs) != 1 || rs[0].Interface != "Wireguard0" {
		t.Fatalf("routes: %+v", rs)
	}
}

func TestParseSpecErrors(t *testing.T) {
	cases := map[string]string{
		"unknown connection":     "connections:\n  awg:\n    routes: [{interface: W0}]\ngroups:\n  - {name: a, connection: nope}\n",
		"listed twice":           "connections:\n  awg:\n    routes: [{interface: W0}]\ngroups:\n  - {name: a, connection: awg}\n  - {name: a, connection: awg}\n",
		"no routes":              "connections:\n  awg: {}\ngroups: []\n",
		"exactly one of url":     "connections:\n  awg:\n    routes: [{interface: W0}]\ngroups:\n  - {name: a, connection: awg, sources: [{url: http://x, file: y}]}\n",
		"unknown list format":    "connections:\n  awg:\n    routes: [{interface: W0}]\ngroups:\n  - {name: a, connection: awg, sources: [{url: http://x, format: xml}]}\n",
		"connection is required": "connections:\n  awg:\n    routes: [{interface: W0}]\ngroups:\n  - {name: a}\n",
		"field bogus not found":  "bogus: 1\n",
	}
	for want, y := range cases {
		_, err := Parse([]byte(y))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
}

func TestNormalize(t *testing.T) {
	in := []string{"Example.com", "example.com.", "EXAMPLE.COM", "*.sub.example.com", "пример.рф",
		"localhost", "local", "*", "com", "0.0.0.0", "127.0.0.1", "::1", "10.0.0.0/8", "bad domain", "-x.com", "a_b.example.com"}
	out, dropped := Normalize(in)
	want := []string{"example.com", "sub.example.com", "xn--e1afmkfd.xn--p1ai", "10.0.0.0/8", "a_b.example.com"}
	if strings.Join(out, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v want %v", out, want)
	}
	if dropped != 9 {
		t.Fatalf("dropped %d", dropped)
	}
}

func TestCollapse(t *testing.T) {
	out, removed := Collapse([]string{"api.example.com", "example.com", "cdn.example.com", "other.org", "x.y.other.org", "10.0.0.0/8", "example.co.uk"})
	want := "example.com other.org 10.0.0.0/8 example.co.uk"
	if strings.Join(out, " ") != want || removed != 3 {
		t.Fatalf("got %v (%d) want %s", out, removed, want)
	}
	// Order of survivors is preserved: the parent appears where it was.
	out, _ = Collapse([]string{"a.example.com", "example.com"})
	if strings.Join(out, " ") != "example.com" {
		t.Fatalf("got %v", out)
	}
}

func TestBuildPriorityRemoveAndLimit(t *testing.T) {
	g := &Group{Name: "g", Include: []string{"Manual.com"}, Remove: []string{"bad.com"}, Exclude: []string{"x.src1.com"}}
	in := Input{
		ManualInclude: []string{"cli.com"},
		ManualRemove:  []string{"cli-bad.com"},
		Sources: []FetchedSource{
			{Source: Source{Name: "s1", URL: "http://s1"}, Body: "src1.com\nbad.com\napi.src1.com\n???\n"},
			{Source: Source{Name: "s2", URL: "http://s2"}, Body: "0.0.0.0 src2.com\ncli-bad.com\nmanual.com\n", Stale: true, Err: errors.New("boom")},
		},
	}
	b, err := Build(g, in, Limits{MaxEntries: 100, OnLimit: OnLimitError})
	if err != nil {
		t.Fatal(err)
	}
	want := "manual.com cli.com src1.com src2.com"
	if strings.Join(b.Entries, " ") != want {
		t.Fatalf("entries %v want %s", b.Entries, want)
	}
	if len(b.Exclude) != 1 || b.Exclude[0] != "x.src1.com" {
		t.Fatalf("exclude %v", b.Exclude)
	}
	if b.Removed != 2 || b.Collapsed != 1 {
		t.Fatalf("removed=%d collapsed=%d", b.Removed, b.Collapsed)
	}
	if len(b.Sources) != 2 || b.Sources[0].Skipped != 1 || b.Sources[0].Valid != 3 || !b.Sources[1].Stale || b.Sources[1].Error != "boom" {
		t.Fatalf("stats %+v", b.Sources)
	}
	if b.Hash == "" {
		t.Fatal("no hash")
	}

	// Limit: error.
	_, err = Build(g, in, Limits{MaxEntries: 3, OnLimit: OnLimitError})
	var le *ErrLimit
	if !errors.As(err, &le) || le.Limit != 3 || le.Count != 5 {
		t.Fatalf("want ErrLimit, got %v", err)
	}
	// Limit: truncate keeps priority order, counts exclude against the limit.
	b, err = Build(g, in, Limits{MaxEntries: 3, OnLimit: OnLimitTruncate})
	if err != nil || strings.Join(b.Entries, " ") != "manual.com cli.com" || b.Truncated != 2 {
		t.Fatalf("truncate: %v %+v", err, b)
	}
	// Per-group limit overrides.
	g.MaxEntries = 4
	b, _ = Build(g, in, Limits{MaxEntries: 3, OnLimit: OnLimitTruncate})
	if len(b.Entries) != 3 {
		t.Fatalf("group limit: %v", b.Entries)
	}
}

func TestBuildHashStable(t *testing.T) {
	g := &Group{Name: "g"}
	a, _ := Build(g, Input{Sources: []FetchedSource{{Body: "a.com\nb.com\n"}}}, Limits{})
	b, _ := Build(g, Input{Sources: []FetchedSource{{Body: "b.com\na.com\n"}}}, Limits{})
	if a.Hash != b.Hash {
		t.Fatal("hash must not depend on order")
	}
}

func TestDiff(t *testing.T) {
	add, rm := Diff([]string{"a", "b"}, []string{"b", "c"})
	if strings.Join(add, ",") != "c" || strings.Join(rm, ",") != "a" {
		t.Fatalf("%v %v", add, rm)
	}
}
