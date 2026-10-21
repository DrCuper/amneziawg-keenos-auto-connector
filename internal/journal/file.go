package journal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/lock"
)

// ErrNotFound is returned by Get for a missing entry.
var ErrNotFound = errors.New("journal entry not found")

// DefaultAutoPruneBytes is the file size above which Append prunes.
const DefaultAutoPruneBytes = 16 << 20

// maxLine bounds one journal line when reading.
const maxLine = 8 << 20

// Journal is the JSON Lines file. Appends from several processes are
// serialized by a file lock; reads take no lock and tolerate a torn last line.
//
//	<path>        one JSON Entry per line
//	<path>.seq    last assigned ID
//	<path>.lock   flock for appends and pruning
type Journal struct {
	path string
	lock *lock.Lock

	// KeepDays and MaxEntries bound the journal on Prune (0 = unlimited).
	KeepDays   int
	MaxEntries int
	// AutoPruneBytes triggers Prune after an append once the file is larger;
	// 0 means DefaultAutoPruneBytes, negative disables.
	AutoPruneBytes int64
	Logger         *slog.Logger
	Now            func() time.Time
}

// Open prepares a journal at path (the directory is created).
func Open(path string) (*Journal, error) {
	if path == "" {
		return nil, errors.New("journal path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create journal dir: %w", err)
	}
	return &Journal{path: path, lock: lock.New(path + ".lock")}, nil
}

// Path returns the journal file path.
func (j *Journal) Path() string { return j.path }

func (j *Journal) logger() *slog.Logger {
	if j.Logger != nil {
		return j.Logger
	}
	return slog.Default()
}

func (j *Journal) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Journal) acquire() (func(), error) {
	return j.lock.Acquire(context.Background(), 10*time.Second)
}

// Append assigns the next ID to e and writes it.
func (j *Journal) Append(e *Entry) (int64, error) {
	release, err := j.acquire()
	if err != nil {
		return 0, err
	}
	defer release()

	id, err := j.lastID()
	if err != nil {
		return 0, err
	}
	e.ID = id + 1
	if e.Time.IsZero() {
		e.Time = j.now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path from config
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.WriteFile(j.path+".seq", []byte(strconv.FormatInt(e.ID, 10)+"\n"), 0o600); err != nil {
		return 0, err
	}

	limit := j.AutoPruneBytes
	if limit == 0 {
		limit = DefaultAutoPruneBytes
	}
	if fi, err := os.Stat(j.path); err == nil && limit > 0 && fi.Size() > limit {
		if _, err := j.prune(); err != nil {
			j.logger().Warn("journal: auto-prune failed", "err", err)
		}
	}
	return e.ID, nil
}

// lastID reads the sequence file, falling back to scanning the journal.
func (j *Journal) lastID() (int64, error) {
	data, err := os.ReadFile(j.path + ".seq")
	if err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			return n, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var last int64
	err = j.scan(func(e *Entry) bool {
		if e.ID > last {
			last = e.ID
		}
		return true
	})
	return last, err
}

// scan calls fn for every readable entry, oldest first, until fn returns false.
func (j *Journal) scan(fn func(*Entry) bool) error {
	f, err := os.Open(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 64<<10)
	lineNo, bad := 0, 0
	for {
		line, err := readLine(r)
		if len(line) > 0 {
			lineNo++
			var e Entry
			if jerr := json.Unmarshal(line, &e); jerr != nil || e.ID == 0 {
				bad++
			} else if !fn(&e) {
				return nil
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if bad > 0 {
		j.logger().Warn("journal: skipped unreadable lines", "path", j.path, "count", bad)
	}
	return nil
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			// Drain the rest of the oversized line and report it as unreadable.
			for isPrefix && err == nil {
				_, isPrefix, err = r.ReadLine()
			}
			return []byte("{}"), err
		}
		if !isPrefix || err != nil {
			return bytes.TrimSpace(buf), err
		}
	}
}

// Filter selects entries in Query.
type Filter struct {
	Group  string
	Kind   string
	Status string
	Since  time.Time
	// DryRun: nil = both, true = only dry-run, false = only real.
	DryRun *bool
	// Limit keeps the newest N matches (0 = all).
	Limit int
}

func (f *Filter) match(e *Entry) bool {
	switch {
	case f.Kind != "" && e.Kind != f.Kind,
		f.Status != "" && e.Status != f.Status,
		!f.Since.IsZero() && e.Time.Before(f.Since),
		f.DryRun != nil && e.DryRun != *f.DryRun,
		f.Group != "" && !e.HasGroup(f.Group):
		return false
	}
	return true
}

// Query returns matching entries, newest first.
func (j *Journal) Query(f Filter) ([]*Entry, error) {
	var out []*Entry
	err := j.scan(func(e *Entry) bool {
		if f.match(e) {
			out = append(out, e)
			if f.Limit > 0 && len(out) > f.Limit {
				out = out[1:]
			}
		}
		return true
	})
	for i, k := 0, len(out)-1; i < k; i, k = i+1, k-1 {
		out[i], out[k] = out[k], out[i]
	}
	return out, err
}

// Get returns the entry with the given ID.
func (j *Journal) Get(id int64) (*Entry, error) {
	var found *Entry
	err := j.scan(func(e *Entry) bool {
		if e.ID == id {
			found = e
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	return found, nil
}

// Prune drops entries older than KeepDays and beyond MaxEntries newest ones,
// rewriting the file atomically. It returns the number of removed entries.
func (j *Journal) Prune() (int, error) {
	release, err := j.acquire()
	if err != nil {
		return 0, err
	}
	defer release()
	return j.prune()
}

func (j *Journal) prune() (int, error) {
	var keep []*Entry
	total := 0
	var cutoff time.Time
	if j.KeepDays > 0 {
		cutoff = j.now().Add(-time.Duration(j.KeepDays) * 24 * time.Hour)
	}
	err := j.scan(func(e *Entry) bool {
		total++
		if cutoff.IsZero() || !e.Time.Before(cutoff) {
			keep = append(keep, e)
		}
		return true
	})
	if err != nil {
		return 0, err
	}
	if j.MaxEntries > 0 && len(keep) > j.MaxEntries {
		keep = keep[len(keep)-j.MaxEntries:]
	}
	if _, err := os.Stat(j.path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(j.path), ".journal-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	w := bufio.NewWriter(tmp)
	for _, e := range keep {
		line, err := json.Marshal(e)
		if err != nil {
			_ = tmp.Close()
			return 0, err
		}
		_, _ = w.Write(append(line, '\n'))
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, j.path); err != nil {
		return 0, err
	}
	return total - len(keep), nil
}
