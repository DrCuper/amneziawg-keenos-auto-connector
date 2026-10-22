// Package rci implements router.Transport over the KeeneticOS RCI HTTP API.
//
// RCI is a JSON mirror of the CLI tree. This transport uses only two things
// from it, both stable across KeeneticOS 4.x/5.x:
//
//   - the challenge-response login on /auth;
//   - `POST /rci/` with `[{"parse": "<cli line>"}, ...]`, which executes CLI
//     commands exactly as the console would.
//
// The running configuration is fetched as CLI text from /ci/running-config.txt.
//
// Writing JSON into configuration branches (e.g. POST /rci/object-group/...)
// is deliberately not used: a misspelled field there is a silent no-op.
package rci

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // mandated by the Keenetic auth scheme
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// Config for the RCI transport.
type Config struct {
	// URL is the router base URL, e.g. http://192.168.1.1.
	URL      string
	Username string
	Password string
	// BatchSize is how many commands go into one POST /rci/ call. 0 = 50.
	BatchSize int
	// HTTPClient overrides the client (tests). Its Jar is replaced.
	HTTPClient *http.Client
}

// Transport is the RCI transport. Safe for concurrent use.
type Transport struct {
	cfg  Config
	base string
	hc   *http.Client

	mu     sync.Mutex
	authed bool
}

var _ router.Transport = (*Transport)(nil)

// New creates the transport. No network activity happens until first use.
func New(cfg Config) (*Transport, error) {
	if cfg.URL == "" || cfg.Username == "" {
		return nil, errors.New("rci: url and username are required")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	hc.Jar = jar
	return &Transport{cfg: cfg, base: strings.TrimRight(cfg.URL, "/"), hc: hc}, nil
}

// Close is a no-op; sessions expire on the router side.
func (t *Transport) Close() error { return nil }

// RunningConfig fetches /ci/running-config.txt.
func (t *Transport) RunningConfig(ctx context.Context) (string, error) {
	body, _, err := t.do(ctx, http.MethodGet, "/ci/running-config.txt", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Exec sends commands through `parse` in batches. If the router reports an
// error for command i, outputs for commands before i are returned together
// with a *router.CommandError. Commands after i in the same batch may have
// been executed by the router; callers verify state afterwards anyway.
func (t *Transport) Exec(ctx context.Context, commands []string) ([]string, error) {
	var outputs []string
	for start := 0; start < len(commands); start += t.cfg.BatchSize {
		end := min(start+t.cfg.BatchSize, len(commands))
		batch := commands[start:end]
		req := make([]map[string]string, len(batch))
		for i, c := range batch {
			req[i] = map[string]string{"parse": c}
		}
		payload, err := json.Marshal(req)
		if err != nil {
			return outputs, err
		}
		body, _, err := t.do(ctx, http.MethodPost, "/rci/", payload)
		if err != nil {
			return outputs, err
		}
		results, err := parseBatchResponse(body, len(batch))
		if err != nil {
			return outputs, &router.TransportError{Op: "parse response", Err: err}
		}
		for i, r := range results {
			if r.err != "" {
				return outputs, &router.CommandError{Command: batch[i], Message: r.err}
			}
			outputs = append(outputs, r.text)
		}
	}
	return outputs, nil
}

type cmdResult struct {
	text string
	err  string
}

// parseBatchResponse walks the response array and extracts, per command,
// the concatenated status messages and the first error, if any.
func parseBatchResponse(body []byte, n int) ([]cmdResult, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return make([]cmdResult, n), nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err != nil {
		// Some firmwares answer a single object for a single command.
		var one json.RawMessage
		if err2 := json.Unmarshal(body, &one); err2 != nil {
			return nil, fmt.Errorf("unexpected RCI response: %s", truncate(body))
		}
		arr = []json.RawMessage{one}
	}
	if len(arr) != n {
		// Be lenient: the router may collapse or expand; still scan for errors.
		var all []cmdResult
		for _, raw := range arr {
			all = append(all, collectStatus(raw))
		}
		out := make([]cmdResult, n)
		for i := range out {
			if i < len(all) {
				out[i] = all[i]
			}
		}
		for _, r := range all {
			if r.err != "" {
				out[0].err = r.err
				break
			}
		}
		return out, nil
	}
	out := make([]cmdResult, n)
	for i, raw := range arr {
		out[i] = collectStatus(raw)
	}
	return out, nil
}

// collectStatus finds every {"status": [...]} block anywhere in the value.
func collectStatus(raw json.RawMessage) cmdResult {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return cmdResult{}
	}
	var res cmdResult
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			if st, ok := x["status"].([]any); ok {
				for _, s := range st {
					m, ok := s.(map[string]any)
					if !ok {
						continue
					}
					msg, _ := m["message"].(string)
					if kind, _ := m["status"].(string); kind == "error" && res.err == "" {
						if code, _ := m["code"].(string); code != "" {
							msg = fmt.Sprintf("%s (code %s)", msg, code)
						}
						res.err = msg
					}
					if msg != "" {
						if res.text != "" {
							res.text += "\n"
						}
						res.text += msg
					}
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(v)
	return res
}

// do performs a request, authenticating on demand and once more on 401.
func (t *Transport) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := t.ensureAuth(ctx, attempt > 0); err != nil {
			return nil, 0, err
		}
		resp, data, err := t.raw(ctx, method, path, body)
		if err != nil {
			return nil, 0, &router.TransportError{Op: method + " " + path, Err: err}
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			t.mu.Lock()
			t.authed = false
			t.mu.Unlock()
			continue
		case resp.StatusCode >= 500:
			return nil, resp.StatusCode, &router.TransportError{Op: method + " " + path, Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(data))}
		case resp.StatusCode >= 400:
			return nil, resp.StatusCode, &router.TransportError{Op: method + " " + path, Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(data))}
		}
		return data, resp.StatusCode, nil
	}
	return nil, 0, &router.TransportError{Op: method + " " + path, Err: errors.New("authentication rejected twice")}
}

// rawResponse is the part of an HTTP response the transport needs; the body
// is always fully read and closed inside raw.
type rawResponse struct {
	StatusCode int
	Header     http.Header
}

func (t *Transport) raw(ctx context.Context, method, path string, body []byte) (rawResponse, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, rdr)
	if err != nil {
		return rawResponse{}, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/plain")
	resp, err := t.hc.Do(req)
	if err != nil {
		return rawResponse{}, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return rawResponse{}, nil, err
	}
	return rawResponse{StatusCode: resp.StatusCode, Header: resp.Header}, data, nil
}

// ensureAuth performs the x-ndw2 challenge-response login if needed.
func (t *Transport) ensureAuth(ctx context.Context, force bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.authed && !force {
		return nil
	}
	resp, _, err := t.raw(ctx, http.MethodGet, "/auth", nil)
	if err != nil {
		return &router.TransportError{Op: "GET /auth", Err: err}
	}
	if resp.StatusCode == http.StatusOK {
		t.authed = true // e.g. request from 127.0.0.1 needs no credentials
		return nil
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return &router.TransportError{Op: "GET /auth", Err: fmt.Errorf("unexpected HTTP %d", resp.StatusCode)}
	}
	realm := resp.Header.Get("X-NDM-Realm")
	challenge := resp.Header.Get("X-NDM-Challenge")
	if realm == "" || challenge == "" {
		return &router.TransportError{Op: "GET /auth", Err: errors.New("router did not send X-NDM-Realm/X-NDM-Challenge headers (remote access via KeenDNS is not supported)")}
	}
	key := Key(t.cfg.Username, realm, t.cfg.Password, challenge)
	payload, _ := json.Marshal(map[string]string{"login": t.cfg.Username, "password": key})
	resp, data, err := t.raw(ctx, http.MethodPost, "/auth", payload)
	if err != nil {
		return &router.TransportError{Op: "POST /auth", Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rci: authentication failed for user %q (HTTP %d: %s)", t.cfg.Username, resp.StatusCode, truncate(data))
	}
	t.authed = true
	return nil
}

// Key computes the x-ndw2 login hash:
//
//	sha256(challenge + md5(login + ":" + realm + ":" + password))
func Key(login, realm, password, challenge string) string {
	h := md5.Sum([]byte(login + ":" + realm + ":" + password)) //nolint:gosec // see package doc
	md5hex := hex.EncodeToString(h[:])
	s := sha256.Sum256([]byte(challenge + md5hex))
	return hex.EncodeToString(s[:])
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// InterfaceStatus reads GET /rci/show/interface?name=<name>.
func (t *Transport) InterfaceStatus(ctx context.Context, name string) (*router.WGStatus, error) {
	body, _, err := t.do(ctx, http.MethodGet, "/rci/show/interface?name="+url.QueryEscape(name), nil)
	if err != nil {
		return nil, err
	}
	var tree map[string]any
	if err := json.Unmarshal(body, &tree); err != nil {
		return nil, &router.TransportError{Op: "show interface", Err: fmt.Errorf("unexpected answer: %s", truncate(body))}
	}
	if r := collectStatus(body); r.err != "" {
		return nil, &router.CommandError{Command: "show interface " + name, Message: r.err}
	}
	return router.ParseWGStatus(name, tree), nil
}
