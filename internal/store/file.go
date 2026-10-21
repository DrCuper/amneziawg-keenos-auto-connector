package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// File is the JSON-on-disk Store.
//
//	<dir>/groups/<name>/state.json
//	<dir>/groups/<name>/v<N>.json
//	<dir>/cache/<sha256(key)>.json
type File struct {
	dir string
	mu  sync.Mutex
	Now func() time.Time
}

var _ Store = (*File)(nil)

// NewFile opens (creating if needed) a file store at dir.
func NewFile(dir string) (*File, error) {
	if dir == "" {
		return nil, errors.New("state_dir is empty")
	}
	for _, d := range []string{dir, filepath.Join(dir, "groups"), filepath.Join(dir, "cache")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("create state dir: %w", err)
		}
	}
	return &File{dir: dir, Now: time.Now}, nil
}

// Dir returns the root directory.
func (f *File) Dir() string { return f.dir }

func (f *File) groupDir(name string) string {
	// Group names are validated by router.ValidateGroupName (printable ASCII,
	// no whitespace) but may still contain path separators; neutralize them.
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(name)
	return filepath.Join(f.dir, "groups", safe)
}

func (f *File) Groups() ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(f.dir, "groups"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *File) GroupState(name string) (GroupState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var st GroupState
	err := readJSON(filepath.Join(f.groupDir(name), "state.json"), &st)
	if errors.Is(err, os.ErrNotExist) {
		return GroupState{}, nil
	}
	if err != nil {
		return GroupState{}, fmt.Errorf("group %s state: %w", name, err)
	}
	return st, nil
}

func (f *File) SetGroupState(name string, st GroupState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st.UpdatedAt = f.Now().UTC()
	dir := f.groupDir(name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "state.json"), st)
}

func (f *File) Versions(name string) ([]*Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	nums, err := f.versionNumbers(name)
	if err != nil {
		return nil, err
	}
	out := make([]*Version, 0, len(nums))
	for _, n := range nums {
		v, err := f.readVersion(name, n)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (f *File) Version(name string, number int) (*Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readVersion(name, number)
}

func (f *File) Latest(name string) (*Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	nums, err := f.versionNumbers(name)
	if err != nil || len(nums) == 0 {
		return nil, err
	}
	return f.readVersion(name, nums[len(nums)-1])
}

func (f *File) SaveVersion(name string, v *Version, keep int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	nums, err := f.versionNumbers(name)
	if err != nil {
		return err
	}
	v.Number = 1
	if len(nums) > 0 {
		v.Number = nums[len(nums)-1] + 1
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = f.Now().UTC()
	}
	if v.Hash == "" {
		v.Hash = Hash(v.Entries, v.Exclude)
	}
	dir := f.groupDir(name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, versionFile(v.Number)), v); err != nil {
		return err
	}
	if keep <= 0 {
		return nil
	}
	// Prune: keep the newest `keep`, plus applied and pinned.
	var st GroupState
	if err := readJSON(filepath.Join(dir, "state.json"), &st); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil //nolint:nilerr // a broken state file must not block saving a version
	}
	nums = append(nums, v.Number)
	for i, n := range nums {
		if i >= len(nums)-keep || n == st.Applied || n == st.Pinned {
			continue
		}
		if err := os.Remove(filepath.Join(dir, versionFile(n))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (f *File) GetCache(key string) (*CacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var e CacheEntry
	err := readJSON(f.cachePath(key), &e)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (f *File) PutCache(e *CacheEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return writeJSON(f.cachePath(e.Key), e)
}

func (f *File) cachePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(f.dir, "cache", hex.EncodeToString(sum[:])+".json")
}

func (f *File) versionNumbers(name string) ([]int, error) {
	ents, err := os.ReadDir(f.groupDir(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var nums []int
	for _, e := range ents {
		n, ok := parseVersionFile(e.Name())
		if ok {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	return nums, nil
}

func (f *File) readVersion(name string, n int) (*Version, error) {
	var v Version
	err := readJSON(filepath.Join(f.groupDir(name), versionFile(n)), &v)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("group %s version %d: %w", name, n, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("group %s version %d: %w", name, n, err)
	}
	return &v, nil
}

func versionFile(n int) string { return fmt.Sprintf("v%06d.json", n) }

func parseVersionFile(s string) (int, bool) {
	if !strings.HasPrefix(s, "v") || !strings.HasSuffix(s, ".json") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(s, "v"), ".json"))
	return n, err == nil && n > 0
}

// Hash is the content hash of a version: order-independent.
func Hash(entries, exclude []string) string {
	a := append([]string(nil), entries...)
	b := append([]string(nil), exclude...)
	sort.Strings(a)
	sort.Strings(b)
	h := sha256.New()
	for _, e := range a {
		h.Write([]byte("+" + e + "\n"))
	}
	for _, e := range b {
		h.Write([]byte("-" + e + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path) //nolint:gosec // paths are built from state_dir
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// writeJSON writes atomically: temp file in the same directory, then rename.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
