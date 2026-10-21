package syncer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	for _, ok := range []string{"0 */6 * * *", "@hourly", "@every 6h", "*/5 * * * *"} {
		if _, err := ParseSchedule(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "* * *", "0 0 0 0 0 0 0", "@weekly-ish"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestSchedulerSkipOverlapAndStop(t *testing.T) {
	var runs atomic.Int32
	triggers := make(chan string, 4)
	release := make(chan struct{})
	s := &Scheduler{Schedule: "@every 1h", RunOnStart: true, Run: func(ctx context.Context, trigger string) error {
		runs.Add(1)
		triggers <- trigger
		<-release
		return nil
	}}
	// Two concurrent manual runs: the second is skipped.
	firstDone := make(chan struct{})
	go func() { s.RunNow(context.Background()); close(firstDone) }()
	deadline := time.Now().Add(2 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ran := s.RunNow(context.Background()); ran || s.Skipped() != 1 {
		t.Fatalf("overlap not skipped: ran=%v skipped=%d", ran, s.Skipped())
	}
	close(release)
	<-firstDone

	// Start runs once on start, then stops cleanly on cancel.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	for runs.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop")
	}
	if runs.Load() != 2 {
		t.Fatalf("runs=%d", runs.Load())
	}
	if a, b := <-triggers, <-triggers; a != TriggerSchedule || b != TriggerStartup {
		t.Errorf("triggers %q, %q", a, b)
	}
}

func TestSchedulerBadExpression(t *testing.T) {
	s := &Scheduler{Schedule: "nope"}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
