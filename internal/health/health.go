// Package health runs periodic checks — router reachability, AWG tunnels
// (built-in, stage 4) and external hook commands — and feeds their outcome
// to the alerter.
package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// Check is one health probe. Run returns nil when healthy.
type Check interface {
	// Signal returns the alert and check name the outcome is reported under.
	Signal() notify.Signal
	Run(ctx context.Context) error
}

// StateReader is the part of router.Client used by RouterCheck.
type StateReader interface {
	ReadState(ctx context.Context) (*router.State, error)
}

// RouterCheck reads running-config; any failure means the router is unreachable.
type RouterCheck struct {
	Router StateReader
}

// Signal implements Check.
func (RouterCheck) Signal() notify.Signal { return notify.Signal{Alert: notify.AlertRouterUnreachable} }

// Run implements Check.
func (c RouterCheck) Run(ctx context.Context) error {
	_, err := c.Router.ReadState(ctx)
	return err
}

// maxOutput is how much of a hook's output is kept in the error.
const maxOutput = 500

// CommandCheck runs an external command (argv, no shell); exit code 0 is healthy.
type CommandCheck struct {
	Name    string
	Alert   string // default notify.AlertAWGDown
	Command []string
	Timeout time.Duration // default 30s
}

// Signal implements Check.
func (c CommandCheck) Signal() notify.Signal {
	a := c.Alert
	if a == "" {
		a = notify.AlertAWGDown
	}
	return notify.Signal{Alert: a, Check: c.Name}
}

// Run implements Check.
func (c CommandCheck) Run(ctx context.Context) error {
	if len(c.Command) == 0 {
		return errors.New("empty command")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...) //nolint:gosec // operator-configured hook
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	if tail := tailOf(out.String(), maxOutput); tail != "" {
		return fmt.Errorf("%w: %s", err, tail)
	}
	return err
}

func tailOf(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

// Result is the outcome of one check.
type Result struct {
	Signal notify.Signal
	Took   time.Duration
}

// RunAll runs the checks sequentially.
func RunAll(ctx context.Context, checks []Check) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		start := time.Now()
		s := c.Signal()
		s.Err = c.Run(ctx)
		out = append(out, Result{Signal: s, Took: time.Since(start)})
	}
	return out
}

// Updater receives check outcomes (notify.Alerter).
type Updater interface {
	Update(ctx context.Context, signals ...notify.Signal)
}

// Monitor runs the checks every Interval until ctx is done.
type Monitor struct {
	Checks   []Check
	Interval time.Duration
	Alerts   Updater
	Logger   *slog.Logger
	// OnResults, if set, receives every round's results (API status page,
	// metrics).
	OnResults func([]Result)
}

// Run blocks until ctx is cancelled. The first round runs after one interval,
// so that startup is not slowed down and the initial sync goes first.
func (m *Monitor) Run(ctx context.Context) {
	if m.Interval <= 0 || len(m.Checks) == 0 {
		return
	}
	log := m.Logger
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var signals []notify.Signal
		results := RunAll(ctx, m.Checks)
		if m.OnResults != nil && ctx.Err() == nil {
			m.OnResults(results)
		}
		for _, r := range results {
			if ctx.Err() != nil {
				return
			}
			if r.Signal.Err != nil {
				log.Warn("health: check failed", "check", r.Signal.Key(), "err", r.Signal.Err)
			} else {
				log.Debug("health: check ok", "check", r.Signal.Key(), "took", r.Took)
			}
			signals = append(signals, r.Signal)
		}
		m.Alerts.Update(ctx, signals...)
	}
}
