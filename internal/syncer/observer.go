package syncer

import (
	"context"
	"time"
)

// Operation kinds reported to observers.
const (
	KindSync     = "sync"
	KindRollback = "rollback"
	KindManual   = "manual"
)

// Triggers: who started an operation.
const (
	TriggerCLI      = "cli"
	TriggerSchedule = "schedule"
	TriggerStartup  = "startup"
	TriggerAPI      = "api"
)

// Manual edit operations.
const (
	ManualAdd    = "add"
	ManualRemove = "remove"
	ManualClear  = "clear"
	ManualUnpin  = "unpin"
)

// Observer is told about every finished operation. Implementations (journal,
// notifications) handle their own errors: an observer cannot fail a sync.
type Observer interface {
	Observe(ctx context.Context, r *Report)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(ctx context.Context, r *Report)

// Observe calls f.
func (f ObserverFunc) Observe(ctx context.Context, r *Report) { f(ctx, r) }

// Report describes one finished operation.
type Report struct {
	Kind    string
	Trigger string
	DryRun  bool
	// Groups is the group filter of a sync (empty = all groups).
	Groups   []string
	Started  time.Time
	Duration time.Duration
	// Result is nil when the run failed before building groups (bad spec).
	Result *Result
	Err    error
	// Manual is set for KindManual.
	Manual *ManualEdit
}

// ManualEdit is a change of a group's manual entries or pin made by the operator.
type ManualEdit struct {
	Group   string
	Op      string // ManualAdd, ManualRemove, ManualClear, ManualUnpin
	Entries []string
}

func (s *Syncer) report(ctx context.Context, r *Report) {
	if r.Trigger == "" {
		r.Trigger = s.DefaultTrigger
	}
	if r.Trigger == "" {
		r.Trigger = TriggerCLI
	}
	// Observers run even when the operation was cancelled: the journal must
	// still record it.
	ctx = context.WithoutCancel(ctx)
	for _, o := range s.Observers {
		o.Observe(ctx, r)
	}
}

func (s *Syncer) reportManual(m *ManualEdit) {
	s.report(context.Background(), &Report{Kind: KindManual, Started: time.Now(), Manual: m})
}
