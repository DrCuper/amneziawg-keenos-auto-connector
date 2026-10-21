package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
)

// Cache is the subset of store.Store the fetcher uses.
type Cache interface {
	GetCache(key string) (*store.CacheEntry, error)
	PutCache(e *store.CacheEntry) error
}

// Ref points at one list: exactly one of URL or File is set. File is resolved
// relative to BaseDir when not absolute.
type Ref struct {
	URL     string
	File    string
	BaseDir string
}

// String renders the reference for logs and stats.
func (r Ref) String() string {
	if r.URL != "" {
		return r.URL
	}
	return r.File
}

// Result is a fetched list.
type Result struct {
	Body      string
	FetchedAt time.Time
	// Stale is set when the live fetch failed and the cached copy was used;
	// Err then holds the fetch error.
	Stale bool
	Err   error
	// NotModified is set when the server answered 304 (body from cache).
	NotModified bool
}

// Fetcher downloads lists with a size limit, conditional requests and a
// fallback to the last good copy.
type Fetcher struct {
	HTTP      *http.Client
	Cache     Cache // may be nil
	MaxBytes  int64 // 0 = 10 MiB
	UserAgent string
	Now       func() time.Time
}

// ErrTooLarge is returned when a list exceeds MaxBytes.
var ErrTooLarge = errors.New("list exceeds size limit")

// Fetch returns the list text. For URLs: a failed download falls back to the
// cache (Stale=true); without a cache the error is returned.
func (f *Fetcher) Fetch(ctx context.Context, ref Ref) (*Result, error) {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	switch {
	case ref.File != "":
		p := ref.File
		if !filepath.IsAbs(p) && ref.BaseDir != "" {
			p = filepath.Join(ref.BaseDir, p)
		}
		data, err := os.ReadFile(p) //nolint:gosec // operator-supplied path
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", ref.File, err)
		}
		if max := f.maxBytes(); int64(len(data)) > max {
			return nil, fmt.Errorf("%s: %w (%d > %d bytes)", ref.File, ErrTooLarge, len(data), max)
		}
		return &Result{Body: string(data), FetchedAt: now().UTC()}, nil
	case ref.URL != "":
		return f.fetchURL(ctx, ref.URL, now())
	default:
		return nil, errors.New("source has neither url nor file")
	}
}

func (f *Fetcher) fetchURL(ctx context.Context, url string, now time.Time) (*Result, error) {
	var cached *store.CacheEntry
	if f.Cache != nil {
		if e, err := f.Cache.GetCache(url); err == nil {
			cached = e
		}
	}
	res, err := f.download(ctx, url, cached, now)
	if err == nil {
		if f.Cache != nil && !res.NotModified {
			_ = f.Cache.PutCache(&store.CacheEntry{Key: url, ETag: res.etag, LastModified: res.lastModified, FetchedAt: now.UTC(), Body: res.Body})
		}
		return res.Result, nil
	}
	if cached != nil {
		return &Result{Body: cached.Body, FetchedAt: cached.FetchedAt, Stale: true, Err: err}, nil
	}
	return nil, err
}

type dlResult struct {
	*Result
	etag, lastModified string
}

func (f *Fetcher) download(ctx context.Context, url string, cached *store.CacheEntry, now time.Time) (*dlResult, error) {
	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	ua := f.UserAgent
	if ua == "" {
		ua = "awgkeen"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/plain, */*;q=0.5")
	if cached != nil {
		if cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		if cached.LastModified != "" {
			req.Header.Set("If-Modified-Since", cached.LastModified)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified && cached != nil {
		return &dlResult{Result: &Result{Body: cached.Body, FetchedAt: cached.FetchedAt, NotModified: true}}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	max := f.maxBytes()
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: read body: %w", url, err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("GET %s: %w (> %d bytes)", url, ErrTooLarge, max)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") && !looksLikeList(string(data)) {
		return nil, fmt.Errorf("GET %s: got an HTML page, not a list", url)
	}
	return &dlResult{
		Result:       &Result{Body: string(data), FetchedAt: now.UTC()},
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

func (f *Fetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return 10 << 20
}

func looksLikeList(s string) bool {
	head := strings.ToLower(strings.TrimSpace(s))
	if len(head) > 256 {
		head = head[:256]
	}
	return !strings.HasPrefix(head, "<!doctype") && !strings.HasPrefix(head, "<html")
}
