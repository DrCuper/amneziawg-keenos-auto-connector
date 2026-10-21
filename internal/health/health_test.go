package health

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
)

func TestCommandCheck(t *testing.T) {
	ok := CommandCheck{Name: "awg", Command: []string{"sh", "-c", "exit 0"}}
	if err := ok.Run(context.Background()); err != nil {
		t.Errorf("ok: %v", err)
	}
	if s := ok.Signal(); s.Key() != "awg_down:awg" {
		t.Errorf("key %s", s.Key())
	}
	bad := CommandCheck{Name: "awg", Command: []string{"sh", "-c", "echo no handshake for 300s; exit 1"}}
	if err := bad.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "no handshake") {
		t.Errorf("bad: %v", err)
	}
	slow := CommandCheck{Name: "slow", Command: []string{"sleep", "5"}, Timeout: 100 * time.Millisecond}
	start := time.Now()
	if err := slow.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 3*time.Second {
		t.Errorf("timeout: %v after %s", err, time.Since(start))
	}
	// No shell: metacharacters are passed literally.
	lit := CommandCheck{Name: "lit", Command: []string{"true;", "false"}}
	if err := lit.Run(context.Background()); err == nil {
		t.Error("argv must not go through a shell")
	}
}

func TestRouterCheck(t *testing.T) {
	f := fake.New("")
	c := router.NewClient(f, cli.Planner{}, router.Options{Timeout: time.Second, Sleep: func(context.Context, time.Duration) error { return nil }})
	rc := RouterCheck{Router: c}
	if err := rc.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.FailReads = 10
	if err := rc.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
}

type updates struct {
	mu  sync.Mutex
	got [][]notify.Signal
}

func (u *updates) Update(_ context.Context, s ...notify.Signal) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.got = append(u.got, s)
}

type stub struct{ err error }

func (s stub) Signal() notify.Signal     { return notify.Signal{Alert: "x", Check: "y"} }
func (s stub) Run(context.Context) error { return s.err }

func TestMonitor(t *testing.T) {
	u := &updates{}
	m := &Monitor{Checks: []Check{stub{}, stub{err: errors.New("down")}}, Interval: 10 * time.Millisecond, Alerts: u}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		u.mu.Lock()
		n := len(u.got)
		u.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.got) < 2 || len(u.got[0]) != 2 || u.got[0][0].Err != nil || u.got[0][1].Err == nil {
		t.Fatalf("updates: %+v", u.got)
	}
}
