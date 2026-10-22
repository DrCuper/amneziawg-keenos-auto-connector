// Package api serves the REST API and the web UI of `awgkeen serve`: groups,
// versions and manual edits, sync and rollback, the change journal, alerts,
// health checks, the router's state and AWG tunnels. It is a thin layer over
// the same components the CLI uses; it adds authentication and keeps
// mutating operations from overlapping with the scheduler.
package api

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/awg"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/health"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/journal"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/notify"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/store"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/syncer"
)

//go:embed web
var webFS embed.FS

// StateReader is the part of router.Client the API reads.
type StateReader interface {
	ReadState(ctx context.Context) (*router.State, error)
}

// Server is the HTTP API. Fields are set once before Handler is called.
type Server struct {
	// Syncer is used for sync, rollback and manual edits; its reports carry
	// syncer.TriggerAPI.
	Syncer   *syncer.Syncer
	Store    store.Store
	SpecPath string
	Journal  *journal.Journal // nil = journal disabled
	Alerter  *notify.Alerter
	Tunnels  *awg.Manager // nil or empty = no tunnels configured
	Router   StateReader
	// Checks returns the latest health results (nil = none yet).
	Checks func() []health.Result
	// Record writes journal entries for tunnel operations.
	Record     func(*journal.Entry)
	MaxChanges int

	// Token grants full access, ReadToken GET only. Both empty = no auth
	// (only allowed on loopback, enforced by the config).
	Token     string
	ReadToken string
	UI        bool
	// Ops serializes mutating operations with the scheduler. A busy lock
	// answers 409.
	Ops     *sync.Mutex
	Version string
	Logger  *slog.Logger
	Now     func() time.Time

	started time.Time
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	if s.Ops == nil {
		s.Ops = &sync.Mutex{}
	}
	s.started = s.now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	read := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(false, h)) }
	write := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(true, s.exclusive(h))) }

	read("GET /api/v1/status", s.status)
	read("GET /api/v1/groups", s.groups)
	read("GET /api/v1/groups/{name}", s.group)
	read("GET /api/v1/groups/{name}/versions", s.versions)
	write("POST /api/v1/groups/{name}/manual", s.manual)
	write("POST /api/v1/groups/{name}/rollback", s.rollback)
	write("POST /api/v1/groups/{name}/unpin", s.unpin)
	write("POST /api/v1/sync", s.sync)
	read("GET /api/v1/journal", s.journalList)
	read("GET /api/v1/journal/{id}", s.journalEntry)
	read("GET /api/v1/alerts", s.alerts)
	read("GET /api/v1/checks", s.checks)
	read("GET /api/v1/router", s.routerState)
	read("GET /api/v1/tunnels", s.tunnels)
	read("GET /api/v1/tunnels/status", s.tunnelStatus)
	write("POST /api/v1/tunnels/apply", s.tunnelApply)
	read("GET /metrics", s.metrics)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, errors.New("no such endpoint"))
	})
	if s.UI {
		sub, _ := fs.Sub(webFS, "web")
		files := http.FileServerFS(sub)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
				return
			}
			files.ServeHTTP(w, r)
		})
	}
	return s.secure(mux)
}

// secure adds headers for the UI and the API.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

// auth checks the bearer token. Write endpoints need the full token.
func (s *Server) auth(write bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" && s.ReadToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case ok && s.Token != "" && equal(got, s.Token):
		case ok && !write && s.ReadToken != "" && equal(got, s.ReadToken):
		case ok && write && s.ReadToken != "" && equal(got, s.ReadToken):
			writeError(w, http.StatusForbidden, errors.New("read-only token"))
			return
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="awgkeen"`)
			writeError(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// exclusive runs a mutating handler under Ops, or answers 409.
func (s *Server) exclusive(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Ops.TryLock() {
			writeError(w, http.StatusConflict, errors.New("another operation (sync, rollback, tunnel apply) is in progress"))
			return
		}
		defer s.Ops.Unlock()
		s.logger().Info("api: operation", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		next(w, r)
	}
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // plans contain <secret>; served as JSON with nosniff
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, errorBody{Error: err.Error()})
}

// decode reads an optional JSON body into v; unknown fields are an error.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}
