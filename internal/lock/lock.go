// Package lock guards router mutations against concurrent runs: a process-wide
// mutex for goroutines inside one binary (cron + HTTP handlers) and an optional
// flock(2) file for separate processes (service + a manual CLI run).
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ErrBusy is returned when the lock could not be acquired within the timeout.
var ErrBusy = errors.New("lock is held by another operation")

// Lock combines an in-process mutex with an optional file lock.
type Lock struct {
	mu   chan struct{}
	path string
}

// New returns a lock. path may be empty to disable the file lock.
func New(path string) *Lock {
	return &Lock{mu: make(chan struct{}, 1), path: path}
}

// Acquire takes the lock, waiting at most timeout (0 = try once). The returned
// release function is idempotent.
func (l *Lock) Acquire(ctx context.Context, timeout time.Duration) (release func(), err error) {
	deadline := time.Now().Add(timeout)
	// in-process part
	if timeout <= 0 {
		select {
		case l.mu <- struct{}{}:
		default:
			return nil, ErrBusy
		}
	} else {
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case l.mu <- struct{}{}:
		case <-t.C:
			return nil, ErrBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	releaseMu := func() { <-l.mu }

	if l.path == "" {
		var once sync.Once
		return func() { once.Do(releaseMu) }, nil
	}

	// file part
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path comes from operator config
	if err != nil {
		releaseMu()
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	for {
		err := flock(f)
		if err == nil {
			break
		}
		if !errors.Is(err, errWouldBlock) {
			_ = f.Close()
			releaseMu()
			return nil, fmt.Errorf("lock %s: %w", l.path, err)
		}
		if timeout <= 0 || time.Now().After(deadline) {
			_ = f.Close()
			releaseMu()
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			releaseMu()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = funlock(f)
			_ = f.Close()
			releaseMu()
		})
	}, nil
}
