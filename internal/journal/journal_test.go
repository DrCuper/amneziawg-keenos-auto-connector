package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

func open(t *testing.T) *Journal {
	t.Helper()
	j, err := Open(filepath.Join(t.TempDir(), "state", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestAppendQueryGet(t *testing.T) {
	j := open(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	entries := []*Entry{
		{Time: now.Add(-48 * time.Hour), Kind: syncer.KindSync, Status: StatusOK, Groups: []Group{{Name: "a"}}},
		{Time: now.Add(-1 * time.Hour), Kind: syncer.KindSync, DryRun: true, Status: StatusOK, Groups: []Group{{Name: "a"}}},
		{Time: now.Add(-30 * time.Minute), Kind: syncer.KindSync, Status: StatusFailed, Groups: []Group{{Name: "b"}}},
		{Time: now.Add(-10 * time.Minute), Kind: syncer.KindManual, Status: StatusOK, Manual: &Manual{Group: "a", Op: "add"}},
	}
	for i, e := range entries {
		id, err := j.Append(e)
		if err != nil || id != int64(i+1) {
			t.Fatalf("append %d: id=%d err=%v", i, id, err)
		}
	}

	got, err := j.Query(Filter{Group: "a", Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 4 || got[1].ID != 2 {
		t.Fatalf("group+since: %+v", ids(got))
	}
	got, _ = j.Query(Filter{DryRun: &no, Status: StatusOK})
	if fmt.Sprint(ids(got)) != "[4 1]" {
		t.Errorf("real ok: %v", ids(got))
	}
	got, _ = j.Query(Filter{DryRun: &yes})
	if fmt.Sprint(ids(got)) != "[2]" {
		t.Errorf("dry-run: %v", ids(got))
	}
	got, _ = j.Query(Filter{Limit: 2})
	if fmt.Sprint(ids(got)) != "[4 3]" {
		t.Errorf("limit: %v", ids(got))
	}
	if e, err := j.Get(3); err != nil || e.Status != StatusFailed {
		t.Errorf("get 3: %+v %v", e, err)
	}
	if _, err := j.Get(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("get 999: %v", err)
	}
}

func ids(es []*Entry) []int64 {
	var out []int64
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func TestCorruptLineAndLostSeq(t *testing.T) {
	j := open(t)
	_, _ = j.Append(&Entry{Kind: "sync", Status: StatusOK})
	f, _ := os.OpenFile(j.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()
	_, _ = j.Append(&Entry{Kind: "sync", Status: StatusOK})
	_ = os.Remove(j.Path() + ".seq")
	id, err := j.Append(&Entry{Kind: "sync", Status: StatusOK})
	if err != nil || id != 3 {
		t.Fatalf("id after lost seq: %d %v", id, err)
	}
	got, err := j.Query(Filter{})
	if err != nil || len(got) != 3 {
		t.Fatalf("query: %v %v", ids(got), err)
	}
}

func TestConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	// Two Journal values = two "processes" sharing the file lock.
	j1, _ := Open(path)
	j2, _ := Open(path)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		j := j1
		if i%2 == 1 {
			j = j2
		}
		go func() {
			defer wg.Done()
			if _, err := j.Append(&Entry{Kind: "sync", Status: StatusOK, Changes: make([]Change, 50)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := j1.Query(Filter{})
	if err != nil || len(got) != 20 {
		t.Fatalf("entries: %d %v", len(got), err)
	}
	seen := map[int64]bool{}
	for _, e := range got {
		if seen[e.ID] {
			t.Fatalf("duplicate id %d", e.ID)
		}
		seen[e.ID] = true
	}
}

func TestPrune(t *testing.T) {
	j := open(t)
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	j.Now = func() time.Time { return now }
	for d := 40; d >= 0; d -= 10 { // 40, 30, 20, 10, 0 days ago
		_, _ = j.Append(&Entry{Time: now.Add(-time.Duration(d) * 24 * time.Hour), Kind: "sync", Status: StatusOK})
	}
	j.KeepDays = 25
	n, err := j.Prune()
	if err != nil || n != 2 {
		t.Fatalf("prune keep_days: n=%d err=%v", n, err)
	}
	got, _ := j.Query(Filter{})
	if fmt.Sprint(ids(got)) != "[5 4 3]" {
		t.Errorf("after prune: %v", ids(got))
	}
	j.MaxEntries = 1
	if n, _ := j.Prune(); n != 2 {
		t.Errorf("prune max_entries: %d", n)
	}
	if id, _ := j.Append(&Entry{Kind: "sync", Status: StatusOK}); id != 6 {
		t.Errorf("ids must keep growing after prune: %d", id)
	}
}

func TestAutoPrune(t *testing.T) {
	j := open(t)
	j.AutoPruneBytes = 300
	j.MaxEntries = 2
	for i := 0; i < 10; i++ {
		_, _ = j.Append(&Entry{Kind: "sync", Status: StatusOK, Error: "padding padding padding padding"})
	}
	got, _ := j.Query(Filter{})
	if len(got) > 3 || got[0].ID != 10 {
		t.Errorf("auto-prune: %v", ids(got))
	}
}

func TestFromReport(t *testing.T) {
	started := time.Date(2026, 10, 21, 6, 0, 0, 0, time.UTC)
	plan := &router.Plan{}
	for i := 0; i < 5; i++ {
		plan.Add("a", fmt.Sprintf("+ include d%d.com", i), "cmd")
	}
	res := &syncer.Result{
		Groups: []syncer.GroupResult{
			{Name: "b", Err: errors.New("source x: 404")},
			{Name: "a", Connection: "awg", Version: 7, PrevApplied: 6, Entries: 100, Added: 5, NewVersion: true},
		},
		Apply: &router.ApplyResult{Plan: plan, Applied: 5, Saved: true, BackupPath: "/b/x.txt"},
	}
	err := fmt.Errorf("%w: group b: source x: 404", syncer.ErrGroupsFailed)
	e := FromReport(&syncer.Report{Kind: syncer.KindSync, Trigger: syncer.TriggerSchedule, Started: started, Duration: 1500 * time.Millisecond, Result: res, Err: err}, 3)
	if e.Status != StatusPartial || !e.Changed || e.Applied != 5 || e.Commands != 5 || e.Backup != "/b/x.txt" || e.DurationMS != 1500 {
		t.Errorf("entry: %+v", e)
	}
	if len(e.Changes) != 3 || e.ChangesOmitted != 2 {
		t.Errorf("changes: %d omitted %d", len(e.Changes), e.ChangesOmitted)
	}
	if e.Groups[0].Name != "a" || e.Groups[0].Added != 5 || e.Groups[1].Error == "" {
		t.Errorf("groups: %+v", e.Groups)
	}

	// All groups failed -> failed, not partial.
	res.Groups = res.Groups[:1]
	if e := FromReport(&syncer.Report{Kind: syncer.KindSync, Result: res, Err: err}, 0); e.Status != StatusFailed {
		t.Errorf("all failed: %s", e.Status)
	}
	if e := FromReport(&syncer.Report{Kind: syncer.KindSync, Err: fmt.Errorf("apply: %w", router.ErrLocked)}, 0); e.Status != StatusSkipped {
		t.Errorf("locked: %s", e.Status)
	}
	// Dry-run with a non-empty plan is "changed" even though nothing was sent.
	dry := FromReport(&syncer.Report{Kind: syncer.KindSync, DryRun: true, Result: &syncer.Result{Apply: &router.ApplyResult{Plan: plan, DryRun: true}}}, 0)
	if !dry.Changed || dry.Applied != 0 || !dry.DryRun {
		t.Errorf("dry-run: %+v", dry)
	}
	m := FromReport(&syncer.Report{Kind: syncer.KindManual, Manual: &syncer.ManualEdit{Group: "a", Op: syncer.ManualAdd, Entries: []string{"x.com"}}}, 0)
	if !m.HasGroup("a") || m.Manual.Op != "add" || m.Status != StatusOK {
		t.Errorf("manual: %+v", m)
	}
}

func TestObserverWithSyncerRun(t *testing.T) {
	// A broken spec still yields a failed journal entry.
	j := open(t)
	s := &syncer.Syncer{SpecPath: filepath.Join(t.TempDir(), "missing.yaml"), Observers: []syncer.Observer{&Observer{J: j}}}
	if _, err := s.Run(t.Context(), syncer.Options{DryRun: true}); err == nil {
		t.Fatal("expected error")
	}
	got, _ := j.Query(Filter{})
	if len(got) != 1 || got[0].Status != StatusFailed || !got[0].DryRun || got[0].Trigger != syncer.TriggerCLI || got[0].Error == "" {
		t.Fatalf("entry: %+v", got)
	}
}
