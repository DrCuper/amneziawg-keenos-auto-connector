package lock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestInProcess(t *testing.T) {
	l := New("")
	rel, err := l.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(context.Background(), 20*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire: got %v, want ErrBusy", err)
	}
	rel()
	rel() // idempotent
	rel2, err := l.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	rel2()
}

func TestFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a := New(path)
	b := New(path) // separate instance = separate "process" for the mutex part
	rel, err := a.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(context.Background(), 30*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
	rel()
	rel2, err := b.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
}
