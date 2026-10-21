package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileFirstRun(t *testing.T) {
	s, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.GroupState("a")
	if err != nil || st.Applied != 0 {
		t.Fatalf("empty state: %+v %v", st, err)
	}
	if v, err := s.Latest("a"); err != nil || v != nil {
		t.Fatalf("latest on empty: %v %v", v, err)
	}
	if err := s.SaveVersion("a", &Version{Entries: []string{"x.com"}}, 10); err != nil {
		t.Fatal(err)
	}
	v, err := s.Latest("a")
	if err != nil || v.Number != 1 || v.Hash == "" || v.CreatedAt.IsZero() {
		t.Fatalf("latest: %+v %v", v, err)
	}
	groups, _ := s.Groups()
	if len(groups) != 1 || groups[0] != "a" {
		t.Fatalf("groups %v", groups)
	}
}

func TestFilePruneKeepsAppliedAndPinned(t *testing.T) {
	s, _ := NewFile(t.TempDir())
	for i := 0; i < 3; i++ {
		if err := s.SaveVersion("g", &Version{Entries: []string{"x.com"}}, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetGroupState("g", GroupState{Applied: 1, Pinned: 2}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.SaveVersion("g", &Version{Entries: []string{"y.com"}}, 2); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := s.Versions("g")
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, v := range vs {
		nums = append(nums, v.Number)
	}
	want := []int{1, 2, 5, 6}
	if len(nums) != len(want) {
		t.Fatalf("versions %v want %v", nums, want)
	}
	for i := range want {
		if nums[i] != want[i] {
			t.Fatalf("versions %v want %v", nums, want)
		}
	}
	if _, err := s.Version("g", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("version 3 should be pruned: %v", err)
	}
}

func TestFileBrokenGroupDoesNotAffectOthers(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFile(dir)
	_ = s.SaveVersion("ok", &Version{Entries: []string{"x.com"}}, 10)
	bad := filepath.Join(dir, "groups", "bad")
	_ = os.MkdirAll(bad, 0o750)
	_ = os.WriteFile(filepath.Join(bad, "state.json"), []byte("{not json"), 0o600)
	if _, err := s.GroupState("bad"); err == nil {
		t.Fatal("expected error for broken state")
	}
	if v, err := s.Latest("ok"); err != nil || v == nil {
		t.Fatalf("ok group affected: %v %v", v, err)
	}
}

func TestFileCache(t *testing.T) {
	s, _ := NewFile(t.TempDir())
	if _, err := s.GetCache("http://x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.PutCache(&CacheEntry{Key: "http://x", ETag: "e", Body: "a.com\n"}); err != nil {
		t.Fatal(err)
	}
	e, err := s.GetCache("http://x")
	if err != nil || e.ETag != "e" || e.Body != "a.com\n" {
		t.Fatalf("cache: %+v %v", e, err)
	}
}

func TestHashOrderIndependent(t *testing.T) {
	if Hash([]string{"a", "b"}, nil) != Hash([]string{"b", "a"}, nil) {
		t.Fatal("hash depends on order")
	}
	if Hash([]string{"a"}, nil) == Hash(nil, []string{"a"}) {
		t.Fatal("include and exclude must hash differently")
	}
}
