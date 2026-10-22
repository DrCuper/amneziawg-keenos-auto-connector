// Package journal keeps the persistent history of what the service did to the
// domain groups and the router: every sync, rollback, manual edit and
// `router apply`, dry-run or real, with its result and a diff summary.
package journal

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// Kinds recorded outside the syncer.
const (
	// KindRouterApply is `router apply` with a hand-written desired file.
	KindRouterApply = "router-apply"
	// KindAWGApply / KindAWGRemove are AWG tunnel imports and removals.
	KindAWGApply  = "awg-apply"
	KindAWGRemove = "awg-remove"
)

// Entry statuses.
const (
	StatusOK      = "ok"
	StatusPartial = "partial" // some groups failed, the rest were applied
	StatusFailed  = "failed"
	StatusSkipped = "skipped" // router locked by another operation
)

// Entry is one journal record (one line of journal.jsonl).
type Entry struct {
	ID         int64     `json:"id"`
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	Trigger    string    `json:"trigger,omitempty"`
	DryRun     bool      `json:"dry_run"`
	Status     string    `json:"status"`
	Changed    bool      `json:"changed"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`

	Groups   []Group `json:"groups,omitempty"`
	Commands int     `json:"commands,omitempty"` // planned (dry-run) or attempted
	Applied  int     `json:"applied,omitempty"`  // actually sent
	Saved    bool    `json:"saved,omitempty"`
	Backup   string  `json:"backup,omitempty"`
	Pruned   bool    `json:"prune,omitempty"`

	Changes        []Change `json:"changes,omitempty"`
	ChangesOmitted int      `json:"changes_omitted,omitempty"`

	Manual *Manual `json:"manual,omitempty"`
}

// Group is the per-group part of an entry.
type Group struct {
	Name        string `json:"name"`
	Connection  string `json:"connection,omitempty"`
	Version     int    `json:"version,omitempty"`
	PrevApplied int    `json:"prev_applied,omitempty"`
	Entries     int    `json:"entries,omitempty"`
	Added       int    `json:"added,omitempty"`
	Removed     int    `json:"removed,omitempty"`
	NewVersion  bool   `json:"new_version,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Change is one line of the router diff.
type Change struct {
	Group string `json:"group"`
	Text  string `json:"text"`
	// Kind is "interface" for Wireguard interface lines (Group is then the
	// interface name); empty for groups.
	Kind string `json:"kind,omitempty"`
}

// Manual describes a manual edit.
type Manual struct {
	Group   string   `json:"group"`
	Op      string   `json:"op"`
	Entries []string `json:"entries,omitempty"`
}

// HasGroup reports whether the entry concerns the group.
func (e *Entry) HasGroup(name string) bool {
	if e.Manual != nil && e.Manual.Group == name {
		return true
	}
	for _, g := range e.Groups {
		if g.Name == name {
			return true
		}
	}
	for _, c := range e.Changes {
		if c.Group == name {
			return true
		}
	}
	return false
}

// FromReport converts a syncer report. maxChanges caps the diff lines kept
// (0 = keep all).
func FromReport(r *syncer.Report, maxChanges int) *Entry {
	e := &Entry{
		Time: r.Started.UTC(), Kind: r.Kind, Trigger: r.Trigger, DryRun: r.DryRun,
		DurationMS: r.Duration.Milliseconds(),
	}
	if r.Manual != nil {
		e.Manual = &Manual{Group: r.Manual.Group, Op: r.Manual.Op, Entries: r.Manual.Entries}
		e.Status, e.Changed = StatusOK, true
		if r.Err != nil {
			e.Status, e.Error = StatusFailed, r.Err.Error()
		}
		return e
	}
	e.Status = status(r.Result, r.Err)
	if r.Err != nil {
		e.Error = r.Err.Error()
	}
	if res := r.Result; res != nil {
		e.Pruned = res.Prune
		groups := append([]syncer.GroupResult(nil), res.Groups...)
		sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
		for _, g := range groups {
			jg := Group{
				Name: g.Name, Connection: g.Connection, Version: g.Version, PrevApplied: g.PrevApplied,
				Entries: g.Entries, Added: g.Added, Removed: g.Removed, NewVersion: g.NewVersion, Pinned: g.Pinned,
			}
			if g.Err != nil {
				jg.Error = g.Err.Error()
			}
			e.Groups = append(e.Groups, jg)
		}
		e.addApply(res.Apply, maxChanges)
	}
	return e
}

// FromApply converts a `router apply` result.
func FromApply(started time.Time, dur time.Duration, dryRun bool, res *router.ApplyResult, err error, maxChanges int) *Entry {
	e := &Entry{
		Time: started.UTC(), Kind: KindRouterApply, Trigger: syncer.TriggerCLI, DryRun: dryRun,
		DurationMS: dur.Milliseconds(), Status: status(nil, err),
	}
	if err != nil {
		e.Error = err.Error()
	}
	e.addApply(res, maxChanges)
	return e
}

func (e *Entry) addApply(a *router.ApplyResult, maxChanges int) {
	if a == nil {
		return
	}
	e.Applied, e.Saved, e.Backup = a.Applied, a.Saved, a.BackupPath
	if a.Plan == nil {
		return
	}
	e.Commands = len(a.Plan.Commands)
	e.Changed = e.Commands > 0 && (e.DryRun || e.Applied > 0)
	for i, c := range a.Plan.Changes {
		if maxChanges > 0 && i >= maxChanges {
			e.ChangesOmitted = len(a.Plan.Changes) - maxChanges
			break
		}
		e.Changes = append(e.Changes, Change{Group: c.Group, Text: c.Text, Kind: c.Kind})
	}
}

func status(res *syncer.Result, err error) string {
	switch {
	case err == nil:
		return StatusOK
	case errors.Is(err, router.ErrLocked):
		return StatusSkipped
	case errors.Is(err, syncer.ErrGroupsFailed) && res != nil && len(res.Failed()) < len(res.Groups):
		return StatusPartial
	default:
		return StatusFailed
	}
}

// Observer records syncer reports into a journal.
type Observer struct {
	J          *Journal
	MaxChanges int
}

var _ syncer.Observer = (*Observer)(nil)

// Observe implements syncer.Observer.
func (o *Observer) Observe(_ context.Context, r *syncer.Report) {
	if _, err := o.J.Append(FromReport(r, o.MaxChanges)); err != nil {
		o.J.logger().Warn("journal: cannot record operation", "kind", r.Kind, "err", err)
	}
}
