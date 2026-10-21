package domains

import (
	"fmt"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/source"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
)

// OnLimit says what to do when a group exceeds its entry limit.
type OnLimit string

const (
	OnLimitError    OnLimit = "error"
	OnLimitTruncate OnLimit = "truncate"
)

// Limits are the effective bounds for a build.
type Limits struct {
	MaxEntries int // 0 = unlimited
	OnLimit    OnLimit
}

// FetchedSource is one source's list after fetching, before parsing.
type FetchedSource struct {
	Source  Source
	Body    string
	Fetched time.Time
	Stale   bool
	Err     error // fetch error when Stale (cache used) — informational
}

// Input is everything Build needs besides the group definition.
type Input struct {
	Sources []FetchedSource
	// ManualInclude / ManualRemove come from the store (CLI edits).
	ManualInclude []string
	ManualRemove  []string
}

// Built is the result of a build: the content that becomes a version.
type Built struct {
	Entries   []string
	Exclude   []string
	Hash      string
	Sources   []store.SourceStat
	Truncated int
	Collapsed int
	Dropped   int // invalid entries dropped by normalization
	Removed   int // entries dropped by remove lists
}

// Version converts the build into a store.Version (number assigned by the store).
func (b *Built) Version(reason string) *store.Version {
	return &store.Version{
		Hash:      b.Hash,
		Entries:   b.Entries,
		Exclude:   b.Exclude,
		Sources:   b.Sources,
		Truncated: b.Truncated,
		Reason:    reason,
	}
}

// ErrLimit is wrapped by Build when the group exceeds its limit with OnLimitError.
type ErrLimit struct {
	Group, Limit, Count int
}

func (e *ErrLimit) Error() string {
	return fmt.Sprintf("%d entries exceed limit %d", e.Count, e.Limit)
}

// Build assembles the group's content. Priority order (what survives
// truncation first): YAML include → CLI manual include → sources in order.
// Remove lists (YAML + CLI) win over everything.
func Build(g *Group, in Input, limits Limits) (*Built, error) {
	b := &Built{}

	// Collect in priority order, tagging per-source counts.
	// Operator-written entries may be single-label names (`youtube`);
	// sources may not.
	all, dropped := NormalizeManual(append(append([]string{}, g.Include...), in.ManualInclude...))
	b.Dropped += dropped
	for _, fs := range in.Sources {
		format, _ := source.ParseFormat(fs.Source.Format)
		entries, st := source.Parse(format, fs.Body)
		valid, dropped := Normalize(entries)
		b.Dropped += dropped
		stat := store.SourceStat{
			Name: fs.Source.Label(), Ref: fs.Source.Ref("").String(),
			Lines: st.Lines, Parsed: st.Parsed, Skipped: st.Skipped, Valid: len(valid),
			Stale: fs.Stale, Fetched: fs.Fetched,
		}
		if fs.Err != nil {
			stat.Error = fs.Err.Error()
		}
		b.Sources = append(b.Sources, stat)
		all = append(all, valid...)
	}

	// Everything was normalized above, so this pass only deduplicates.
	entries, _ := NormalizeManual(all)

	// Remove lists.
	remove, _ := NormalizeManual(append(append([]string{}, g.Remove...), in.ManualRemove...))
	if len(remove) > 0 {
		rm := map[string]bool{}
		for _, e := range remove {
			rm[e] = true
		}
		kept := entries[:0]
		for _, e := range entries {
			if rm[e] {
				b.Removed++
				continue
			}
			kept = append(kept, e)
		}
		entries = kept
	}

	if g.Collapse() {
		entries, b.Collapsed = Collapse(entries)
	}

	limit := limits.MaxEntries
	if g.MaxEntries > 0 {
		limit = g.MaxEntries
	}
	exclude, _ := NormalizeManual(g.Exclude)
	if limit > 0 && len(entries)+len(exclude) > limit {
		switch limits.OnLimit {
		case OnLimitTruncate:
			keep := limit - len(exclude)
			if keep < 0 {
				keep = 0
			}
			b.Truncated = len(entries) - keep
			entries = entries[:keep]
		default:
			return nil, &ErrLimit{Limit: limit, Count: len(entries) + len(exclude)}
		}
	}

	b.Entries = entries
	b.Exclude = exclude
	b.Hash = store.Hash(entries, exclude)
	return b, nil
}
