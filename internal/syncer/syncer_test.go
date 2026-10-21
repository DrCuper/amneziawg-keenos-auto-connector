package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/domains"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/source"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
)

type env struct {
	dir   string
	fake  *fake.Transport
	store *store.File
	s     *Syncer
}

const specTmpl = `
connections:
  awg:
    routes: [{interface: Wireguard0}]
  reality:
    routes: [{gateway: 192.168.1.2}]
groups:
  - name: a
    connection: awg
    sources: [{file: a.txt, name: a-list}]
    exclude: [x.a1.com]
  - name: b
    connection: reality
    sources: [{file: b.txt}]
    include: [manual.b.com]
%s
`

func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "domains.yaml"), []byte(fmt.Sprintf(specTmpl, extra)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a1.com\na2.com\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b1.com\n"), 0o600)
	st, err := store.NewFile(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New("")
	client := router.NewClient(f, cli.Planner{}, router.Options{Timeout: time.Second, SaveConfig: true,
		Sleep: func(context.Context, time.Duration) error { return nil }})
	s := &Syncer{
		SpecPath: filepath.Join(dir, "domains.yaml"), Store: st, Fetcher: &source.Fetcher{Cache: st},
		Router: client, Limits: domains.Limits{MaxEntries: 100, OnLimit: domains.OnLimitError},
		MaxShrink: 0.5, MinShrinkBase: 3, KeepVersions: 10,
	}
	return &env{dir: dir, fake: f, store: st, s: s}
}

func (e *env) setList(t *testing.T, name, body string) {
	t.Helper()
	_ = os.WriteFile(filepath.Join(e.dir, name), []byte(body), 0o600)
}

func TestRunFirstSyncAndIdempotent(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	res, err := e.s.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, res.Summary())
	}
	if len(res.Groups) != 2 || res.Apply == nil || res.Apply.Applied == 0 || !res.Apply.Saved {
		t.Fatalf("result: %+v", res)
	}
	g, ok := e.fake.State.Group("a")
	if !ok || strings.Join(g.Include, ",") != "a1.com,a2.com" || strings.Join(g.Exclude, ",") != "x.a1.com" {
		t.Fatalf("router group a: %+v", g)
	}
	if gb, _ := e.fake.State.Group("b"); strings.Join(gb.Include, ",") != "manual.b.com,b1.com" {
		t.Fatalf("router group b: %+v", gb)
	}
	if len(e.fake.State.Routes) != 2 {
		t.Fatalf("routes: %+v", e.fake.State.Routes)
	}
	st, _ := e.store.GroupState("a")
	if st.Applied != 1 {
		t.Fatalf("applied: %+v", st)
	}

	// Second run: nothing changes anywhere.
	saved := e.fake.Saved
	cmds := len(e.fake.Commands)
	res, err = e.s.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Apply.Plan.Empty() || e.fake.Saved != saved || len(e.fake.Commands) != cmds {
		t.Fatalf("second run not idempotent: %s", res.Apply.Plan)
	}
	for _, g := range res.Groups {
		if g.NewVersion || g.Version != 1 {
			t.Fatalf("version churn: %+v", g)
		}
	}
}

func TestRunDiffAndNewVersion(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if _, err := e.s.Run(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	e.setList(t, "a.txt", "a2.com\na3.com\n")
	cmds := len(e.fake.Commands)
	res, err := e.s.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(e.fake.Commands[cmds:], ";")
	if !strings.Contains(got, "object-group fqdn a include a3.com") || !strings.Contains(got, "no object-group fqdn a include a1.com") || strings.Contains(got, "a2.com") {
		t.Fatalf("commands: %s", got)
	}
	ga := res.Groups[0]
	if ga.Version != 2 || !ga.NewVersion || ga.Added != 1 || ga.Removed != 1 || ga.PrevApplied != 1 {
		t.Fatalf("group a: %+v", ga)
	}
	if gb := res.Groups[1]; gb.NewVersion || gb.Version != 1 {
		t.Fatalf("group b should be unchanged: %+v", gb)
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	res, err := e.s.Run(ctx, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || !res.Apply.DryRun || len(e.fake.Commands) != 0 {
		t.Fatalf("dry-run touched the router: %+v", res.Apply)
	}
	if v, _ := e.store.Latest("a"); v != nil {
		t.Fatalf("dry-run saved a version: %+v", v)
	}
	if st, _ := e.store.GroupState("a"); st.Applied != 0 {
		t.Fatalf("dry-run marked applied: %+v", st)
	}
	if !res.Groups[0].NewVersion || res.Groups[0].Version != 1 {
		t.Fatalf("dry-run should report would-be version 1: %+v", res.Groups[0])
	}
}

// Regression for issue #3: a group imported from the router with a
// single-label entry (`include youtube`) must not plan its deletion.
func TestDryRunKeepsImportedSingleLabel(t *testing.T) {
	e := newEnv(t, `
  - name: domain-list0
    connection: awg
    include: [youtube, youtube.com]
    collapse_subdomains: false
`)
	e.fake = fake.New("object-group fqdn domain-list0\n    include youtube\n    include youtube.com\n" +
		"dns-proxy\n    route object-group domain-list0 Wireguard0 auto\n")
	e.s.Router = router.NewClient(e.fake, cli.Planner{}, router.Options{Timeout: time.Second})
	res, err := e.s.Run(context.Background(), Options{DryRun: true, Groups: []string{"domain-list0"}})
	if err != nil {
		t.Fatal(err)
	}
	g := res.Groups[0]
	if g.Err != nil || g.Dropped != 0 || g.Entries != 2 {
		t.Fatalf("group: %+v", g)
	}
	for _, c := range res.Apply.Plan.Commands {
		if strings.Contains(c, "domain-list0") {
			t.Fatalf("unexpected change to domain-list0: %q", c)
		}
	}
}

func TestRunGroupFailureIsolated(t *testing.T) {
	e := newEnv(t, "prune: true")
	ctx := context.Background()
	_ = os.Remove(filepath.Join(e.dir, "b.txt"))
	res, err := e.s.Run(ctx, Options{})
	if !errors.Is(err, ErrGroupsFailed) {
		t.Fatalf("want ErrGroupsFailed, got %v", err)
	}
	if res.Prune {
		t.Fatal("prune must be off when a group failed")
	}
	if _, ok := e.fake.State.Group("a"); !ok {
		t.Fatal("group a should have been applied")
	}
	if _, ok := e.fake.State.Group("b"); ok {
		t.Fatal("group b should not exist")
	}
	if f := res.Failed(); len(f) != 1 || f[0].Name != "b" || !strings.Contains(f[0].Err.Error(), "b.txt") {
		t.Fatalf("failed: %+v", f)
	}
}

func TestRunApplyErrorKeepsAppliedVersion(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if _, err := e.s.Run(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	e.setList(t, "a.txt", "a1.com\na2.com\na3.com\n")
	e.fake.RejectCommand = "object-group fqdn a include a3.com"
	res, err := e.s.Run(ctx, Options{})
	var ce *router.CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("want CommandError, got %v", err)
	}
	if res.Groups[0].Version != 2 {
		t.Fatalf("version should be recorded: %+v", res.Groups[0])
	}
	if st, _ := e.store.GroupState("a"); st.Applied != 1 {
		t.Fatalf("applied must stay 1: %+v", st)
	}
}

func TestShrinkGuard(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.setList(t, "a.txt", "a1.com\na2.com\na3.com\na4.com\n")
	if _, err := e.s.Run(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	e.setList(t, "a.txt", "a1.com\n")
	res, err := e.s.Run(ctx, Options{})
	var se *ErrShrink
	if !errors.As(err, &se) || se.Applied != 4 || se.New != 1 {
		t.Fatalf("want ErrShrink, got %v", err)
	}
	if g, _ := e.fake.State.Group("a"); len(g.Include) != 4 {
		t.Fatalf("router changed despite guard: %+v", g)
	}
	if res.Groups[0].Version != 2 {
		t.Fatalf("shrunk version should still be recorded: %+v", res.Groups[0])
	}
	// Empty result is always blocked.
	e.setList(t, "a.txt", "# nothing\n")
	if _, err := e.s.Run(ctx, Options{}); !errors.As(err, &se) {
		t.Fatalf("empty group must be blocked: %v", err)
	}
	// --force applies.
	e.setList(t, "a.txt", "a1.com\n")
	if _, err := e.s.Run(ctx, Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.fake.State.Group("a"); len(g.Include) != 1 {
		t.Fatalf("force did not apply: %+v", g)
	}
}

func TestRollbackAndUnpin(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if _, err := e.s.Run(ctx, Options{}); err != nil { // v1: a1,a2
		t.Fatal(err)
	}
	e.setList(t, "a.txt", "a1.com\na2.com\na9.com\n")
	if _, err := e.s.Run(ctx, Options{}); err != nil { // v2
		t.Fatal(err)
	}
	// Dry-run rollback: nothing pinned, nothing sent.
	cmds := len(e.fake.Commands)
	res, err := e.s.Rollback(ctx, "a", 0, true)
	if err != nil || !res.Groups[0].Pinned || res.Groups[0].Version != 1 || len(e.fake.Commands) != cmds {
		t.Fatalf("dry rollback: %v %+v", err, res.Groups[0])
	}
	if st, _ := e.store.GroupState("a"); st.Pinned != 0 {
		t.Fatalf("dry-run pinned: %+v", st)
	}

	if _, err = e.s.Rollback(ctx, "a", 0, false); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.fake.State.Group("a"); strings.Join(g.Include, ",") != "a1.com,a2.com" {
		t.Fatalf("rollback did not apply v1: %+v", g)
	}
	st, _ := e.store.GroupState("a")
	if st.Pinned != 1 || st.Applied != 1 {
		t.Fatalf("state after rollback: %+v", st)
	}
	// Pinned group ignores sources on the next sync.
	e.setList(t, "a.txt", "zzz.com\n")
	res, err = e.s.Run(ctx, Options{})
	if err != nil || !res.Groups[0].Pinned || res.Groups[0].Version != 1 {
		t.Fatalf("pinned sync: %v %+v", err, res.Groups[0])
	}
	if g, _ := e.fake.State.Group("a"); strings.Join(g.Include, ",") != "a1.com,a2.com" {
		t.Fatalf("pinned group changed: %+v", g)
	}
	// Unknown version.
	if _, err := e.s.Rollback(ctx, "a", 42, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// Unpin, then sources win again.
	if err := e.s.Unpin("a"); err != nil {
		t.Fatal(err)
	}
	res, err = e.s.Run(ctx, Options{Force: true})
	if err != nil || res.Groups[0].Pinned || res.Groups[0].Version != 3 {
		t.Fatalf("after unpin: %v %+v", err, res.Groups[0])
	}
}

func TestManualEdits(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if _, err := e.s.AddManual("a", []string{"Extra.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RemoveManual("a", []string{"a2.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddManual("a", []string{"bad entry"}); err == nil {
		t.Fatal("invalid entry accepted")
	}
	if _, err := e.s.Run(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.fake.State.Group("a"); strings.Join(g.Include, ",") != "extra.com,a1.com" {
		t.Fatalf("manual edits not applied: %+v", g)
	}
	// Re-adding a removed entry un-removes it.
	if _, err := e.s.AddManual("a", []string{"a2.com"}); err != nil {
		t.Fatal(err)
	}
	st, _ := e.store.GroupState("a")
	if len(st.ManualRemove) != 0 || strings.Join(st.ManualInclude, ",") != "extra.com,a2.com" {
		t.Fatalf("state: %+v", st)
	}
	if err := e.s.ClearManual("a"); err != nil {
		t.Fatal(err)
	}
	st, _ = e.store.GroupState("a")
	if st.ManualInclude != nil || st.ManualRemove != nil {
		t.Fatalf("clear: %+v", st)
	}
}

func TestGroupFilter(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	res, err := e.s.Run(ctx, Options{Groups: []string{"b"}})
	if err != nil || len(res.Groups) != 1 || res.Groups[0].Name != "b" {
		t.Fatalf("%v %+v", err, res)
	}
	if _, ok := e.fake.State.Group("a"); ok {
		t.Fatal("group a should not be touched")
	}
	if _, err := e.s.Run(ctx, Options{Groups: []string{"nope"}}); err == nil {
		t.Fatal("unknown group accepted")
	}
}

func TestLimitError(t *testing.T) {
	e := newEnv(t, "")
	e.s.Limits.MaxEntries = 2
	_, err := e.s.Run(context.Background(), Options{})
	var le *domains.ErrLimit
	if !errors.As(err, &le) {
		t.Fatalf("want ErrLimit, got %v", err)
	}
	if _, ok := e.fake.State.Group("b"); !ok {
		t.Fatal("group b (2 entries) should still be applied")
	}
}
