package router_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
)

const base = "system\n    hostname k\nobject-group fqdn vpn\n    include a.com\n    include b.com\ndns-proxy\n    route object-group vpn Wireguard1 auto\n"

func newClient(t *testing.T, f *fake.Transport, mod func(*router.Options)) *router.Client {
	t.Helper()
	o := router.Options{
		Timeout: time.Second, Retries: 2, RetryDelay: time.Millisecond, SaveConfig: true,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	if mod != nil {
		mod(&o)
	}
	return router.NewClient(f, cli.Planner{}, o)
}

func want() *router.Desired {
	return &router.Desired{Groups: []router.DesiredGroup{{
		Name: "vpn", Include: []string{"b.com", "c.com"},
		RoutesSpec: []router.RouteSpec{{Interface: "Wireguard2"}},
	}}}
}

func TestReadState(t *testing.T) {
	f := fake.New(base)
	c := newClient(t, f, nil)
	st, err := c.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := st.Group("vpn"); !ok || len(g.Include) != 2 {
		t.Errorf("state %+v", st.Groups)
	}
}

func TestDryRunSendsNoCommands(t *testing.T) {
	f := fake.New(base)
	c := newClient(t, f, nil)
	res, err := c.Apply(context.Background(), want(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Plan.Empty() || res.Applied != 0 || res.Saved {
		t.Errorf("result %+v", res)
	}
	if len(f.Commands) != 0 || f.Saved != 0 {
		t.Errorf("dry-run sent commands: %q", f.Commands)
	}
	if !strings.Contains(res.Plan.String(), "+ include c.com") {
		t.Errorf("plan text:\n%s", res.Plan)
	}
}

func TestApplyVerifiesAndSaves(t *testing.T) {
	f := fake.New(base)
	c := newClient(t, f, nil)
	res, err := c.Apply(context.Background(), want(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 4 || !res.Saved || f.Saved != 1 {
		t.Errorf("result %+v saved=%d", res, f.Saved)
	}
	// Idempotent: second run is a no-op and does not save.
	res, err = c.Apply(context.Background(), want(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Plan.Empty() || res.Applied != 0 || res.Saved || f.Saved != 1 {
		t.Errorf("second apply not idempotent: %+v saved=%d", res, f.Saved)
	}
}

func TestApplyNoSave(t *testing.T) {
	f := fake.New(base)
	c := newClient(t, f, func(o *router.Options) { o.SaveConfig = false })
	if _, err := c.Apply(context.Background(), want(), false); err != nil {
		t.Fatal(err)
	}
	if f.Saved != 0 {
		t.Error("saved although SaveConfig=false")
	}
}

func TestCommandErrorStopsApply(t *testing.T) {
	f := fake.New(base)
	f.RejectCommand = "object-group fqdn vpn include c.com"
	c := newClient(t, f, nil)
	_, err := c.Apply(context.Background(), want(), false)
	var ce *router.CommandError
	if !errors.As(err, &ce) || ce.Command != f.RejectCommand || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("got %v", err)
	}
	if f.Saved != 0 {
		t.Error("saved after a command error")
	}
	if len(f.Commands) != 0 {
		t.Errorf("commands after the failure were sent: %q", f.Commands)
	}
}

func TestVerifyErrorOnSilentNoop(t *testing.T) {
	f := fake.New(base)
	f.Ignore = map[string]bool{"object-group fqdn vpn include c.com": true}
	c := newClient(t, f, nil)
	_, err := c.Apply(context.Background(), want(), false)
	var ve *router.VerifyError
	if !errors.As(err, &ve) || len(ve.Remaining.Commands) != 1 {
		t.Fatalf("got %v", err)
	}
	if f.Saved != 0 {
		t.Error("saved despite verification failure")
	}
}

func TestRetriesTransportErrors(t *testing.T) {
	f := fake.New(base)
	f.FailReads = 1
	f.FailExec = 1
	c := newClient(t, f, nil)
	if _, err := c.Apply(context.Background(), want(), false); err != nil {
		t.Fatal(err)
	}
	if f.Reads != 3 { // 1 failed + 1 ok before apply, 1 verify
		t.Errorf("reads %d", f.Reads)
	}
}

func TestRetriesExhausted(t *testing.T) {
	f := fake.New(base)
	f.FailReads = 10
	c := newClient(t, f, func(o *router.Options) { o.Retries = 2 })
	_, err := c.ReadState(context.Background())
	if err == nil || !strings.Contains(err.Error(), "after 3 attempt") || !router.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
}

func TestCommandErrorNotRetried(t *testing.T) {
	f := fake.New(base)
	f.RejectCommand = cli.Save
	c := newClient(t, f, func(o *router.Options) { o.Retries = 5 })
	err := c.Save(context.Background())
	var ce *router.CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "attempt") {
		t.Errorf("command error was retried: %v", err)
	}
}

func TestBackupBeforeApply(t *testing.T) {
	dir := t.TempDir()
	f := fake.New(base)
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := newClient(t, f, func(o *router.Options) { o.BackupDir = dir; o.Now = func() time.Time { return fixed } })
	res, err := c.Apply(context.Background(), want(), false)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dir, "running-config-20260102-030405.txt")
	if res.BackupPath != wantPath {
		t.Errorf("backup path %q", res.BackupPath)
	}
	before := fake.New(base).Render()
	data, err := os.ReadFile(wantPath)
	if err != nil || string(data) != before {
		t.Errorf("backup content mismatch: %v %q", err, data)
	}
	// No backup when nothing changes.
	res, _ = c.Apply(context.Background(), want(), false)
	if res.BackupPath != "" {
		t.Error("backup written for empty plan")
	}
}

func TestLockRejectsConcurrentApply(t *testing.T) {
	f := fake.New(base)
	started := make(chan struct{})
	proceed := make(chan struct{})
	slow := &blockingTransport{Transport: f, started: started, proceed: proceed}
	c := router.NewClient(slow, cli.Planner{}, router.Options{Timeout: time.Second, SaveConfig: false,
		Sleep: func(context.Context, time.Duration) error { return nil }})
	done := make(chan error, 1)
	go func() { _, err := c.Apply(context.Background(), want(), false); done <- err }()
	<-started
	_, err := c.Apply(context.Background(), want(), false)
	if !errors.Is(err, router.ErrLocked) {
		t.Errorf("second apply: got %v, want ErrLocked", err)
	}
	close(proceed)
	if err := <-done; err != nil {
		t.Errorf("first apply: %v", err)
	}
}

func TestInvalidDesired(t *testing.T) {
	c := newClient(t, fake.New(base), nil)
	_, err := c.Apply(context.Background(), &router.Desired{Groups: []router.DesiredGroup{{Name: "bad name"}}}, true)
	if err == nil || !strings.Contains(err.Error(), "invalid desired state") {
		t.Errorf("got %v", err)
	}
}

// blockingTransport pauses the first Exec until told to proceed so a second
// Apply can observe the lock.
type blockingTransport struct {
	*fake.Transport
	started chan struct{}
	proceed chan struct{}
	once    bool
}

func (b *blockingTransport) Exec(ctx context.Context, cmds []string) ([]string, error) {
	if !b.once {
		b.once = true
		close(b.started)
		<-b.proceed
	}
	return b.Transport.Exec(ctx, cmds)
}
