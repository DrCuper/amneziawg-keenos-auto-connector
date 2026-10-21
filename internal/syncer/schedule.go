package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// ParseSchedule validates a cron expression (5 fields, or @hourly / @every 6h).
func ParseSchedule(expr string) (cron.Schedule, error) {
	if expr == "" {
		return nil, errors.New("schedule is empty")
	}
	s, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: %w", expr, err)
	}
	return s, nil
}

// Scheduler runs Syncer.Run on a cron schedule. Overlapping runs are skipped;
// a run that fails is logged and the daemon keeps going.
type Scheduler struct {
	Syncer     *Syncer
	Schedule   string
	RunOnStart bool
	Logger     *slog.Logger
	// Run is overridable for tests; defaults to Syncer.Run with only the
	// trigger set.
	Run func(ctx context.Context, trigger string) error

	mu      sync.Mutex
	running bool
	wg      sync.WaitGroup
	skipped int
}

// Skipped reports how many runs were skipped because one was still going.
func (s *Scheduler) Skipped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipped
}

func (s *Scheduler) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// RunNow performs one scheduled run unless another is in flight (then it is
// skipped and false is returned).
func (s *Scheduler) RunNow(ctx context.Context) bool {
	return s.runOnce(ctx, TriggerSchedule)
}

func (s *Scheduler) runOnce(ctx context.Context, trigger string) bool {
	s.mu.Lock()
	if s.running {
		s.skipped++
		s.mu.Unlock()
		s.log().Warn("sync: previous run still in progress, skipping")
		return false
	}
	s.running = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()

	run := s.Run
	if run == nil {
		run = func(ctx context.Context, trigger string) error {
			_, err := s.Syncer.Run(ctx, Options{Trigger: trigger})
			return err
		}
	}
	start := time.Now()
	if err := run(ctx, trigger); err != nil {
		s.log().Error("sync: run failed", "err", err, "took", time.Since(start).Round(time.Millisecond))
		return true
	}
	s.log().Info("sync: run finished", "took", time.Since(start).Round(time.Millisecond))
	return true
}

// Start validates the schedule, runs once if RunOnStart, then blocks until ctx
// is done, waiting for an in-flight run to finish before returning.
func (s *Scheduler) Start(ctx context.Context) error {
	sched, err := ParseSchedule(s.Schedule)
	if err != nil {
		return err
	}
	if s.RunOnStart {
		s.runOnce(ctx, TriggerStartup)
	}
	c := cron.New(cron.WithLogger(cron.DiscardLogger))
	c.Schedule(sched, cron.FuncJob(func() { s.RunNow(ctx) }))
	c.Start()
	s.log().Info("sync: scheduler started", "schedule", s.Schedule, "next", sched.Next(time.Now()).Format(time.RFC3339))
	<-ctx.Done()
	<-c.Stop().Done()
	s.wg.Wait()
	return nil
}
