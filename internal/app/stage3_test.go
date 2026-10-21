package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/config"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

// TestSyncJournalAndAlerts wires a real App (fake router, webhook server) and
// walks through: ok sync -> router down x2 -> recovery.
func TestSyncJournalAndAlerts(t *testing.T) {
	var mu sync.Mutex
	var events []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m notify.Message
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		events = append(events, m.Event+":"+m.Key)
		mu.Unlock()
	}))
	defer hook.Close()

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "domains.yaml"), []byte("connections:\n  awg:\n    routes: [{interface: Wireguard0}]\ngroups:\n  - name: a\n    connection: awg\n    sources: [{file: a.txt}]\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a1.com\na2.com\n"), 0o600)

	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.Sync.DomainsFile = filepath.Join(dir, "domains.yaml")
	cfg.Notify.Webhooks = []config.Webhook{{URL: hook.URL}}
	cfg.Notify.Retries = 0
	f := fake.New("")
	a := &App{Config: cfg, Transport: f, Logger: newLogger(config.Log{Level: "error"})}
	a.Client = router.NewClient(f, cli.Planner{}, router.Options{Timeout: time.Second, Retries: 1, SaveConfig: true,
		Sleep: func(context.Context, time.Duration) error { return nil }})
	st, err := store.NewFile(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	s := a.NewSyncer(st)
	ctx := context.Background()

	if _, err := s.Run(ctx, syncer.Options{}); err != nil {
		t.Fatal(err)
	}
	f.FailReads = 100
	for i := 0; i < 2; i++ {
		if _, err := s.Run(ctx, syncer.Options{Trigger: syncer.TriggerSchedule}); err == nil {
			t.Fatal("expected transport error")
		}
	}
	if _, err := s.Run(ctx, syncer.Options{DryRun: true}); err == nil {
		t.Fatal("expected transport error in dry-run")
	}
	f.FailReads = 0
	if _, err := s.Run(ctx, syncer.Options{}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "firing:router_unreachable" || got[1] != "resolved:router_unreachable" {
		t.Fatalf("webhook events: %v", got)
	}

	es, err := a.Journal().Query(journal.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 5 {
		t.Fatalf("journal entries: %d", len(es))
	}
	// newest first: ok, dry-run failed, failed(schedule) x2, ok(changed)
	if es[4].Status != journal.StatusOK || !es[4].Changed || es[4].Applied == 0 || len(es[4].Changes) == 0 ||
		es[3].Status != journal.StatusFailed || es[3].Trigger != syncer.TriggerSchedule ||
		!es[1].DryRun || es[0].Status != journal.StatusOK || es[0].Changed {
		for _, e := range es {
			t.Logf("%+v", e)
		}
		t.Fatal("unexpected journal")
	}
}
