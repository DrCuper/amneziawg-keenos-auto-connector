package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
)

type memCache struct{ m map[string]*store.CacheEntry }

func (c *memCache) GetCache(k string) (*store.CacheEntry, error) {
	if e, ok := c.m[k]; ok {
		return e, nil
	}
	return nil, store.ErrNotFound
}
func (c *memCache) PutCache(e *store.CacheEntry) error { c.m[e.Key] = e; return nil }

func newCache() *memCache { return &memCache{m: map[string]*store.CacheEntry{}} }

func TestFetchURLAndCache(t *testing.T) {
	var hits atomic.Int32
	var status atomic.Int32
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` && status.Load() == 200 {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if s := int(status.Load()); s != 200 {
			w.WriteHeader(s)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("a.com\nb.com\n"))
	}))
	defer srv.Close()

	c := newCache()
	f := &Fetcher{Cache: c}
	ctx := context.Background()

	res, err := f.Fetch(ctx, Ref{URL: srv.URL})
	if err != nil || res.Body != "a.com\nb.com\n" || res.Stale || res.NotModified {
		t.Fatalf("first fetch: %+v %v", res, err)
	}
	if c.m[srv.URL] == nil || c.m[srv.URL].ETag != `"v1"` {
		t.Fatalf("cache not populated: %+v", c.m[srv.URL])
	}

	res, err = f.Fetch(ctx, Ref{URL: srv.URL})
	if err != nil || !res.NotModified || res.Body != "a.com\nb.com\n" {
		t.Fatalf("304 fetch: %+v %v", res, err)
	}

	status.Store(500)
	res, err = f.Fetch(ctx, Ref{URL: srv.URL})
	if err != nil || !res.Stale || res.Err == nil || res.Body != "a.com\nb.com\n" {
		t.Fatalf("stale fetch: %+v %v", res, err)
	}

	// No cache: error propagates.
	f2 := &Fetcher{Cache: newCache()}
	if _, err := f2.Fetch(ctx, Ref{URL: srv.URL}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want HTTP 500 error, got %v", err)
	}
	if hits.Load() != 4 {
		t.Fatalf("hits %d", hits.Load())
	}
}

func TestFetchTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a.com\n", 100)))
	}))
	defer srv.Close()
	f := &Fetcher{MaxBytes: 50}
	if _, err := f.Fetch(context.Background(), Ref{URL: srv.URL}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestFetchHTMLRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>login</body></html>"))
	}))
	defer srv.Close()
	f := &Fetcher{}
	if _, err := f.Fetch(context.Background(), Ref{URL: srv.URL}); err == nil || !strings.Contains(err.Error(), "HTML") {
		t.Fatalf("want HTML error, got %v", err)
	}
}

func TestFetchFileRelative(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "lists"), 0o750)
	_ = os.WriteFile(filepath.Join(dir, "lists", "x.txt"), []byte("x.com\n"), 0o600)
	f := &Fetcher{}
	res, err := f.Fetch(context.Background(), Ref{File: "lists/x.txt", BaseDir: dir})
	if err != nil || res.Body != "x.com\n" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := f.Fetch(context.Background(), Ref{File: "missing.txt", BaseDir: dir}); err == nil {
		t.Fatal("expected error for missing file")
	}
}
