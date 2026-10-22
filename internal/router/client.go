package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/lock"
)

// Planner turns (current state, desired state) into a plan and parses
// running-config. It is satisfied by package cli; it is an interface only so
// that package router does not import cli (which imports router).
type Planner interface {
	Parse(runningConfig string) *State
	Diff(cur *State, want *Desired) *Plan
	SaveCommand() string
}

// Options tune the client.
type Options struct {
	Timeout     time.Duration // per transport call
	Retries     int           // transport-error retries (0 = single attempt)
	RetryDelay  time.Duration // first backoff; doubles each attempt, capped at 10s
	LockFile    string        // "" = in-process lock only
	LockTimeout time.Duration // 0 = fail immediately when busy
	BackupDir   string        // "" = no backups
	SaveConfig  bool          // run `system configuration save` after apply
	Limits      Limits
	Logger      *slog.Logger
	// Now is overridable for tests (backup file names).
	Now func() time.Time
	// Sleep is overridable for tests (retry backoff).
	Sleep func(context.Context, time.Duration) error
}

// Client orchestrates lock → read → backup → plan → apply → verify → save.
type Client struct {
	t    Transport
	p    Planner
	o    Options
	lock *lock.Lock
}

// NewClient wires a transport with a planner.
func NewClient(t Transport, p Planner, o Options) *Client {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = 500 * time.Millisecond
	}
	return &Client{t: t, p: p, o: o, lock: lock.New(o.LockFile)}
}

// Close closes the transport.
func (c *Client) Close() error { return c.t.Close() }

// ReadState fetches and parses running-config.
func (c *Client) ReadState(ctx context.Context) (*State, error) {
	var text string
	err := c.retry(ctx, "running-config", func(ctx context.Context) error {
		var err error
		text, err = c.t.RunningConfig(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c.p.Parse(text), nil
}

// Plan reads the state and returns the diff against want (after Normalize).
func (c *Client) Plan(ctx context.Context, want *Desired) (*State, *Plan, error) {
	if err := want.Normalize(c.o.Limits); err != nil {
		return nil, nil, fmt.Errorf("invalid desired state: %w", err)
	}
	st, err := c.ReadState(ctx)
	if err != nil {
		return nil, nil, err
	}
	return st, c.p.Diff(st, want), nil
}

// ApplyResult reports what Apply did.
type ApplyResult struct {
	Plan       *Plan
	DryRun     bool
	Applied    int    // commands sent
	Saved      bool   // `system configuration save` ran
	BackupPath string // "" when no backup was written
}

// Apply reads the router, builds the plan and, unless dryRun, executes it
// under the lock, verifies the result and saves the configuration.
func (c *Client) Apply(ctx context.Context, want *Desired, dryRun bool) (*ApplyResult, error) {
	if err := want.Normalize(c.o.Limits); err != nil {
		return nil, fmt.Errorf("invalid desired state: %w", err)
	}
	if dryRun {
		st, err := c.ReadState(ctx)
		if err != nil {
			return nil, err
		}
		return &ApplyResult{Plan: c.p.Diff(st, want), DryRun: true}, nil
	}

	release, err := c.lock.Acquire(ctx, c.o.LockTimeout)
	if err != nil {
		if errors.Is(err, lock.ErrBusy) {
			return nil, ErrLocked
		}
		return nil, err
	}
	defer release()

	st, err := c.ReadState(ctx)
	if err != nil {
		return nil, err
	}
	plan := c.p.Diff(st, want)
	res := &ApplyResult{Plan: plan}
	if plan.Empty() {
		c.o.Logger.Info("router: nothing to apply")
		return res, nil
	}

	if c.o.BackupDir != "" {
		res.BackupPath, err = c.backup(st.Raw)
		if err != nil {
			return nil, fmt.Errorf("backup before apply: %w", err)
		}
		c.o.Logger.Info("router: backup written", "path", res.BackupPath)
	}

	c.o.Logger.Info("router: applying", "commands", len(plan.Commands))
	if err := c.exec(ctx, plan.Commands, plan.RedactErr); err != nil {
		return res, plan.RedactErr(err)
	}
	res.Applied = len(plan.Commands)

	after, err := c.ReadState(ctx)
	if err != nil {
		return res, fmt.Errorf("re-read after apply: %w", err)
	}
	if rest := c.p.Diff(after, want); rest.Verifiable() {
		return res, &VerifyError{Remaining: rest}
	}

	if c.o.SaveConfig {
		if err := c.Save(ctx); err != nil {
			return res, err
		}
		res.Saved = true
	}
	return res, nil
}

// Save persists the running configuration.
func (c *Client) Save(ctx context.Context) error {
	c.o.Logger.Info("router: saving configuration")
	return c.exec(ctx, []string{c.p.SaveCommand()}, nil)
}

// exec runs commands with retries on transport errors. Re-sending a batch is
// safe because every command this service emits is idempotent. redact (may
// be nil) masks secrets in errors before they are logged.
func (c *Client) exec(ctx context.Context, cmds []string, redact func(error) error) error {
	return c.retryRedact(ctx, "exec", redact, func(ctx context.Context) error {
		_, err := c.t.Exec(ctx, cmds)
		return err
	})
}

func (c *Client) retry(ctx context.Context, op string, fn func(context.Context) error) error {
	return c.retryRedact(ctx, op, nil, fn)
}

func (c *Client) retryRedact(ctx context.Context, op string, redact func(error) error, fn func(context.Context) error) error {
	if redact == nil {
		redact = func(err error) error { return err }
	}
	var last error
	for attempt := 0; attempt <= c.o.Retries; attempt++ {
		if attempt > 0 {
			d := c.o.RetryDelay << (attempt - 1)
			if d > 10*time.Second {
				d = 10 * time.Second
			}
			d += time.Duration(rand.Int64N(int64(d/4 + 1))) //nolint:gosec // jitter, not security
			c.o.Logger.Warn("router: retrying", "op", op, "attempt", attempt, "delay", d, "err", redact(last))
			if err := c.o.Sleep(ctx, d); err != nil {
				return err
			}
		}
		actx, cancel := context.WithTimeout(ctx, c.o.Timeout)
		err := fn(actx)
		cancel()
		if err == nil {
			return nil
		}
		if !IsRetryable(err) {
			return err
		}
		last = err
	}
	return fmt.Errorf("%s failed after %d attempt(s): %w", op, c.o.Retries+1, last)
}

func (c *Client) backup(raw string) (string, error) {
	if err := os.MkdirAll(c.o.BackupDir, 0o750); err != nil {
		return "", err
	}
	name := "running-config-" + c.o.Now().UTC().Format("20060102-150405") + ".txt"
	path := filepath.Join(c.o.BackupDir, name)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
