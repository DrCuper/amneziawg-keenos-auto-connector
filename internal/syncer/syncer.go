// Package syncer is the orchestrator of stage 2: it loads the domains spec,
// builds every group from its sources (or its pinned version), records
// versions in the store and pushes the result to the router through the
// stage-1 client, which does the diffing, locking, backup and verification.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/domains"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/source"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
)

// Applier is the part of router.Client the syncer uses.
type Applier interface {
	Apply(ctx context.Context, want *router.Desired, dryRun bool) (*router.ApplyResult, error)
}

// Syncer wires spec, sources, store and router together.
type Syncer struct {
	SpecPath string
	Store    store.Store
	Fetcher  *source.Fetcher
	Router   Applier

	Limits domains.Limits
	// MaxShrink blocks a group whose new content is smaller than the applied
	// version by more than this fraction (0.5 = half). 0 disables.
	MaxShrink float64
	// MinShrinkBase is the applied size below which the shrink guard is off.
	MinShrinkBase int
	KeepVersions  int
	Logger        *slog.Logger
	Now           func() time.Time
}

// Options tune one run.
type Options struct {
	DryRun bool
	Force  bool // bypass the shrink guard
	Groups []string
	// pins overrides the stored pinned version (used by Rollback --dry-run).
	pins map[string]int
}

// GroupResult is the per-group outcome.
type GroupResult struct {
	Name        string
	Connection  string
	Version     int // version that was (or would be) applied
	PrevApplied int
	Entries     int
	Exclude     int
	NewVersion  bool // a new version was created (or would be, in dry-run)
	Pinned      bool
	Added       int // vs previously applied version
	Removed     int
	Sources     []store.SourceStat
	Truncated   int
	Collapsed   int
	Dropped     int
	Err         error
}

// Result is the outcome of Run.
type Result struct {
	Groups []GroupResult
	Apply  *router.ApplyResult
	DryRun bool
	// Prune reports whether pruning of unlisted groups was in effect.
	Prune bool
}

// Failed lists groups that could not be built or were blocked.
func (r *Result) Failed() []GroupResult {
	var out []GroupResult
	for _, g := range r.Groups {
		if g.Err != nil {
			out = append(out, g)
		}
	}
	return out
}

// ErrShrink is returned (wrapped in GroupResult.Err) by the shrink guard.
type ErrShrink struct {
	Applied, New int
}

func (e *ErrShrink) Error() string {
	pct := 100
	if e.Applied > 0 {
		pct = (e.Applied - e.New) * 100 / e.Applied
	}
	return fmt.Sprintf("group shrunk by %d%% (%d -> %d entries); use --force to apply", pct, e.Applied, e.New)
}

// ErrGroupsFailed wraps per-group errors so callers can distinguish "some
// groups failed, the rest were applied" from a hard failure.
var ErrGroupsFailed = errors.New("some groups failed")

func (s *Syncer) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Run builds the groups and applies them. The returned error is non-nil when
// the spec could not be loaded, the router apply failed, or any group failed
// (then it wraps ErrGroupsFailed and the Result still describes everything).
func (s *Syncer) Run(ctx context.Context, opts Options) (*Result, error) {
	spec, err := domains.Load(s.SpecPath)
	if err != nil {
		return nil, err
	}
	groups, err := selectGroups(spec, opts.Groups)
	if err != nil {
		return nil, err
	}

	res := &Result{DryRun: opts.DryRun}
	var contents []groupContent
	for _, g := range groups {
		gr, c := s.resolve(ctx, spec, g, opts)
		res.Groups = append(res.Groups, gr)
		if gr.Err == nil {
			contents = append(contents, c)
		}
	}

	// Pruning only when every listed group is present in Desired; otherwise
	// a failed or filtered-out group would be deleted from the router.
	res.Prune = spec.Prune && len(opts.Groups) == 0 && len(res.Failed()) == 0
	if spec.Prune && !res.Prune {
		s.logger().Warn("sync: prune disabled for this run (group filter or failed groups)")
	}

	want := &router.Desired{Prune: res.Prune}
	for _, c := range contents {
		want.Groups = append(want.Groups, router.DesiredGroup{
			Name: c.group.Name, Description: c.group.Description,
			Include: c.entries, Exclude: c.exclude, RoutesSpec: spec.Routes(c.group),
		})
	}

	var errs []error
	for _, g := range res.Failed() {
		errs = append(errs, fmt.Errorf("group %s: %w", g.Name, g.Err))
	}

	if len(want.Groups) > 0 {
		ar, err := s.Router.Apply(ctx, want, opts.DryRun)
		res.Apply = ar
		if err != nil {
			return res, errors.Join(append([]error{fmt.Errorf("apply: %w", err)}, errs...)...)
		}
		if !opts.DryRun {
			for _, c := range contents {
				if err := s.markApplied(c.group.Name, c.version); err != nil {
					errs = append(errs, fmt.Errorf("group %s: record applied version: %w", c.group.Name, err))
				}
			}
		}
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("%w: %w", ErrGroupsFailed, errors.Join(errs...))
	}
	return res, nil
}

type groupContent struct {
	group   *domains.Group
	entries []string
	exclude []string
	version int
}

func selectGroups(spec *domains.Spec, names []string) ([]*domains.Group, error) {
	if len(names) == 0 {
		out := make([]*domains.Group, 0, len(spec.Groups))
		for i := range spec.Groups {
			out = append(out, &spec.Groups[i])
		}
		return out, nil
	}
	var out []*domains.Group
	for _, n := range names {
		g, ok := spec.Group(n)
		if !ok {
			return nil, fmt.Errorf("group %q is not in %s", n, spec.BaseDir)
		}
		out = append(out, g)
	}
	return out, nil
}

// resolve produces the content for one group: from its pinned version or by
// building from sources, recording a new version when the content changed.
func (s *Syncer) resolve(ctx context.Context, spec *domains.Spec, g *domains.Group, opts Options) (GroupResult, groupContent) {
	gr := GroupResult{Name: g.Name, Connection: g.Connection}
	st, err := s.Store.GroupState(g.Name)
	if err != nil {
		gr.Err = err
		return gr, groupContent{}
	}
	gr.PrevApplied = st.Applied

	var applied *store.Version
	if st.Applied > 0 {
		if v, err := s.Store.Version(g.Name, st.Applied); err == nil {
			applied = v
		}
	}

	pinned := st.Pinned
	if p, ok := opts.pins[g.Name]; ok {
		pinned = p
	}
	if pinned > 0 {
		v, err := s.Store.Version(g.Name, pinned)
		if err != nil {
			gr.Err = fmt.Errorf("pinned version %d: %w", pinned, err)
			return gr, groupContent{}
		}
		gr.Pinned = true
		gr.Version = v.Number
		gr.Entries, gr.Exclude = len(v.Entries), len(v.Exclude)
		gr.Sources = v.Sources
		gr.Added, gr.Removed = countDiff(applied, v.Entries)
		return gr, groupContent{group: g, entries: v.Entries, exclude: v.Exclude, version: v.Number}
	}

	in := domains.Input{ManualInclude: st.ManualInclude, ManualRemove: st.ManualRemove}
	for _, src := range g.Sources {
		fr, err := s.Fetcher.Fetch(ctx, src.Ref(spec.BaseDir))
		if err != nil {
			gr.Err = fmt.Errorf("source %s: %w", src.Label(), err)
			return gr, groupContent{}
		}
		if fr.Stale {
			s.logger().Warn("sync: using cached copy of source", "group", g.Name, "source", src.Label(), "err", fr.Err)
		}
		in.Sources = append(in.Sources, domains.FetchedSource{Source: src, Body: fr.Body, Fetched: fr.FetchedAt, Stale: fr.Stale, Err: fr.Err})
	}
	built, err := domains.Build(g, in, s.Limits)
	if err != nil {
		gr.Err = err
		return gr, groupContent{}
	}
	gr.Entries, gr.Exclude = len(built.Entries), len(built.Exclude)
	gr.Sources, gr.Truncated, gr.Collapsed, gr.Dropped = built.Sources, built.Truncated, built.Collapsed, built.Dropped
	gr.Added, gr.Removed = countDiff(applied, built.Entries)

	latest, err := s.Store.Latest(g.Name)
	if err != nil {
		gr.Err = err
		return gr, groupContent{}
	}
	switch {
	case latest != nil && latest.Hash == built.Hash:
		gr.Version = latest.Number
	case opts.DryRun:
		gr.NewVersion = true
		gr.Version = 1
		if latest != nil {
			gr.Version = latest.Number + 1
		}
	default:
		v := built.Version("sources")
		v.CreatedAt = s.now().UTC()
		if err := s.Store.SaveVersion(g.Name, v, s.KeepVersions); err != nil {
			gr.Err = fmt.Errorf("save version: %w", err)
			return gr, groupContent{}
		}
		gr.NewVersion = true
		gr.Version = v.Number
		s.logger().Info("sync: new version", "group", g.Name, "version", v.Number, "entries", len(v.Entries))
	}

	if !opts.Force && applied != nil && s.MaxShrink > 0 {
		base := len(applied.Entries)
		minBase := s.MinShrinkBase
		if minBase <= 0 {
			minBase = 20
		}
		n := len(built.Entries)
		if base > 0 && (n == 0 || (base >= minBase && float64(base-n)/float64(base) > s.MaxShrink)) {
			gr.Err = &ErrShrink{Applied: base, New: n}
			return gr, groupContent{}
		}
	}
	return gr, groupContent{group: g, entries: built.Entries, exclude: built.Exclude, version: gr.Version}
}

func countDiff(applied *store.Version, entries []string) (added, removed int) {
	if applied == nil {
		return len(entries), 0
	}
	a, r := domains.Diff(applied.Entries, entries)
	return len(a), len(r)
}

func (s *Syncer) markApplied(group string, version int) error {
	st, err := s.Store.GroupState(group)
	if err != nil {
		return err
	}
	if st.Applied == version {
		return nil
	}
	st.Applied = version
	return s.Store.SetGroupState(group, st)
}

// Rollback pins the group to version `to` (0 = the version before the applied
// one) and applies it. With dryRun nothing is pinned or sent.
func (s *Syncer) Rollback(ctx context.Context, group string, to int, dryRun bool) (*Result, error) {
	st, err := s.Store.GroupState(group)
	if err != nil {
		return nil, err
	}
	if to == 0 {
		if st.Applied == 0 {
			return nil, fmt.Errorf("group %s: nothing has been applied yet", group)
		}
		vs, err := s.Store.Versions(group)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			if v.Number < st.Applied {
				to = v.Number
			}
		}
		if to == 0 {
			return nil, fmt.Errorf("group %s: no version before applied version %d", group, st.Applied)
		}
	}
	if _, err := s.Store.Version(group, to); err != nil {
		return nil, err
	}
	opts := Options{Groups: []string{group}, DryRun: dryRun, Force: true, pins: map[string]int{group: to}}
	if !dryRun {
		st.Pinned = to
		if err := s.Store.SetGroupState(group, st); err != nil {
			return nil, err
		}
	}
	return s.Run(ctx, opts)
}

// Unpin clears the pinned version so that the next sync builds from sources.
func (s *Syncer) Unpin(group string) error {
	st, err := s.Store.GroupState(group)
	if err != nil {
		return err
	}
	if st.Pinned == 0 {
		return nil
	}
	st.Pinned = 0
	return s.Store.SetGroupState(group, st)
}

// AddManual records entries to always include (and un-removes them).
func (s *Syncer) AddManual(group string, entries []string) ([]string, error) {
	return s.editManual(group, entries, true)
}

// RemoveManual records entries to always drop (and un-includes them).
func (s *Syncer) RemoveManual(group string, entries []string) ([]string, error) {
	return s.editManual(group, entries, false)
}

// ClearManual drops all manual edits of the group.
func (s *Syncer) ClearManual(group string) error {
	st, err := s.Store.GroupState(group)
	if err != nil {
		return err
	}
	st.ManualInclude, st.ManualRemove = nil, nil
	return s.Store.SetGroupState(group, st)
}

func (s *Syncer) editManual(group string, entries []string, include bool) ([]string, error) {
	norm, dropped := domains.Normalize(entries)
	if dropped > 0 || len(norm) == 0 {
		return nil, fmt.Errorf("invalid entries in %v", entries)
	}
	st, err := s.Store.GroupState(group)
	if err != nil {
		return nil, err
	}
	to, from := &st.ManualInclude, &st.ManualRemove
	if !include {
		to, from = from, to
	}
	*to = union(*to, norm)
	*from = without(*from, norm)
	return norm, s.Store.SetGroupState(group, st)
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range append(append([]string{}, a...), b...) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

func without(a, b []string) []string {
	rm := map[string]bool{}
	for _, e := range b {
		rm[e] = true
	}
	var out []string
	for _, e := range a {
		if !rm[e] {
			out = append(out, e)
		}
	}
	return out
}

// Summary renders the result for the CLI.
func (r *Result) Summary() string {
	var b strings.Builder
	groups := append([]GroupResult(nil), r.Groups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	for _, g := range groups {
		if g.Err != nil {
			fmt.Fprintf(&b, "group %s: ERROR: %v\n", g.Name, g.Err)
			continue
		}
		fmt.Fprintf(&b, "group %s -> %s: version %d", g.Name, g.Connection, g.Version)
		switch {
		case g.Pinned:
			b.WriteString(" (pinned)")
		case g.NewVersion && r.DryRun:
			b.WriteString(" (new, not saved: dry-run)")
		case g.NewVersion:
			b.WriteString(" (new)")
		}
		fmt.Fprintf(&b, ", %d entries", g.Entries)
		if g.Exclude > 0 {
			fmt.Fprintf(&b, ", %d exclude", g.Exclude)
		}
		if g.PrevApplied > 0 {
			fmt.Fprintf(&b, ", +%d/-%d vs applied v%d", g.Added, g.Removed, g.PrevApplied)
		}
		if g.Truncated > 0 {
			fmt.Fprintf(&b, ", truncated %d", g.Truncated)
		}
		if g.Collapsed > 0 {
			fmt.Fprintf(&b, ", collapsed %d", g.Collapsed)
		}
		if g.Dropped > 0 {
			fmt.Fprintf(&b, ", dropped %d invalid", g.Dropped)
		}
		b.WriteString("\n")
		for _, s := range g.Sources {
			fmt.Fprintf(&b, "  source %s: %d valid of %d lines", s.Name, s.Valid, s.Lines)
			if s.Skipped > 0 {
				fmt.Fprintf(&b, ", %d skipped", s.Skipped)
			}
			if s.Stale {
				fmt.Fprintf(&b, " (STALE: %s)", s.Error)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
