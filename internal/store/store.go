// Package store persists the service's own state: per-group version history,
// which version is applied/pinned, manual edits made through the CLI and the
// cache of downloaded source lists. The file implementation keeps everything
// as JSON under one directory; the Store interface exists so that an SQLite
// implementation can replace it later without touching the syncer.
package store

import (
	"errors"
	"time"
)

// ErrNotFound is returned for a missing version or cache entry.
var ErrNotFound = errors.New("not found")

// GroupState is the small, frequently updated part of a group's state.
type GroupState struct {
	// Applied is the version number currently on the router (0 = never).
	Applied int `json:"applied_version"`
	// Pinned, when non-zero, freezes the group at that version: sync uses it
	// instead of building from sources (rollback).
	Pinned int `json:"pinned_version,omitempty"`
	// ManualInclude / ManualRemove are entries added through `groups add|remove`.
	ManualInclude []string  `json:"manual_include,omitempty"`
	ManualRemove  []string  `json:"manual_remove,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SourceStat records how one source contributed to a version.
type SourceStat struct {
	Name    string    `json:"name"`
	Ref     string    `json:"ref"` // URL or file path
	Lines   int       `json:"lines"`
	Parsed  int       `json:"parsed"`
	Skipped int       `json:"skipped"`
	Valid   int       `json:"valid"` // entries that passed normalization
	Stale   bool      `json:"stale,omitempty"`
	Error   string    `json:"error,omitempty"`
	Fetched time.Time `json:"fetched_at,omitempty"`
}

// Version is a full snapshot of a group's content at one point in time.
type Version struct {
	Number    int          `json:"number"`
	CreatedAt time.Time    `json:"created_at"`
	Hash      string       `json:"hash"` // sha256 of sorted entries + exclude
	Entries   []string     `json:"entries"`
	Exclude   []string     `json:"exclude,omitempty"`
	Sources   []SourceStat `json:"sources,omitempty"`
	Truncated int          `json:"truncated,omitempty"`
	// Reason is free text: "sources", "manual", "rollback".
	Reason string `json:"reason,omitempty"`
}

// CacheEntry is the last successful download of a URL source.
type CacheEntry struct {
	Key          string    `json:"key"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	FetchedAt    time.Time `json:"fetched_at"`
	Body         string    `json:"body"`
}

// Store is what the syncer needs from persistence.
type Store interface {
	// Groups lists group names that have any state.
	Groups() ([]string, error)
	// GroupState returns the state; a group without state yields the zero value.
	GroupState(name string) (GroupState, error)
	SetGroupState(name string, st GroupState) error

	// Versions lists all stored versions, oldest first, with entries loaded.
	Versions(name string) ([]*Version, error)
	// Version loads one version; ErrNotFound when absent.
	Version(name string, number int) (*Version, error)
	// Latest returns the newest version or nil when the group has none.
	Latest(name string) (*Version, error)
	// SaveVersion assigns the next number, stores v and prunes old versions,
	// keeping at least `keep` newest ones plus the applied and pinned ones.
	SaveVersion(name string, v *Version, keep int) error

	GetCache(key string) (*CacheEntry, error)
	PutCache(e *CacheEntry) error
}
