// Package notify delivers alerts about the service — router unreachable, sync
// failed, AWG down — to Telegram and webhooks, once per incident with a
// recovery message, and optional informational messages about applied syncs.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Message events.
const (
	EventFiring   = "firing"
	EventReminder = "reminder"
	EventResolved = "resolved"
	EventInfo     = "info"
	EventTest     = "test"
)

// Message is what channels deliver; it is also the webhook JSON body.
type Message struct {
	Event    string     `json:"event"`
	Alert    string     `json:"alert,omitempty"` // router_unreachable, sync_failed, awg_down
	Key      string     `json:"key,omitempty"`   // alert + optional ":<check>"
	Title    string     `json:"title"`
	Text     string     `json:"text,omitempty"`
	Since    *time.Time `json:"since,omitempty"`
	Failures int        `json:"failures,omitempty"`
	Time     time.Time  `json:"time"`
	Host     string     `json:"host,omitempty"`
}

// Plain renders the message as plain text (Telegram, logs).
func (m *Message) Plain() string {
	icon := map[string]string{EventFiring: "🔴", EventReminder: "🔁", EventResolved: "✅", EventInfo: "ℹ️", EventTest: "🧪"}[m.Event]
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", icon, m.Title)
	if m.Host != "" {
		fmt.Fprintf(&b, " [%s]", m.Host)
	}
	if m.Text != "" {
		b.WriteString("\n" + m.Text)
	}
	if m.Since != nil {
		switch m.Event {
		case EventResolved:
			fmt.Fprintf(&b, "\nDuration: %s (since %s)", m.Time.Sub(*m.Since).Round(time.Second), m.Since.Local().Format("2006-01-02 15:04"))
		case EventFiring, EventReminder:
			fmt.Fprintf(&b, "\nSince %s, %d failure(s)", m.Since.Local().Format("2006-01-02 15:04"), m.Failures)
		}
	}
	return b.String()
}

// Channel delivers messages.
type Channel interface {
	Name() string
	Send(ctx context.Context, m *Message) error
}

// HTTPOptions are shared by the HTTP channels.
type HTTPOptions struct {
	Client  *http.Client
	Timeout time.Duration // per attempt, default 10s
	Retries int           // extra attempts on 5xx/429/network errors
	// RetryDelay is the first backoff step (doubles), default 1s.
	RetryDelay time.Duration
	// Sleep is overridable in tests.
	Sleep func(context.Context, time.Duration) error
}

func (o *HTTPOptions) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return http.DefaultClient
}

// retryAfterError asks post to wait before the next attempt.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }

// permanentError is not retried.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }

// do runs attempt with retries. The error returned never contains the URL.
func (o *HTTPOptions) do(ctx context.Context, attempt func(ctx context.Context) error) error {
	delay := o.RetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	var last error
	for i := 0; i <= o.Retries; i++ {
		if i > 0 {
			d := delay << (i - 1)
			var ra *retryAfterError
			if errors.As(last, &ra) && ra.after > 0 && ra.after < time.Minute {
				d = ra.after
			}
			if err := sleep(ctx, d); err != nil {
				return err
			}
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		err := attempt(actx)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		var pe *permanentError
		if errors.As(err, &pe) {
			return pe.err
		}
	}
	var ra *retryAfterError
	if errors.As(last, &ra) {
		last = ra.err
	}
	if o.Retries > 0 {
		return fmt.Errorf("after %d attempts: %w", o.Retries+1, last)
	}
	return last
}

// post sends body and classifies the response. Errors are scrubbed of the URL
// (net/http puts it, with any token, into *url.Error).
func (o *HTTPOptions) post(ctx context.Context, u string, body []byte, header http.Header, check func(status int, body []byte) error) error {
	return o.do(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return &permanentError{errors.New("invalid request URL")}
		}
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := o.client().Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err := check(resp.StatusCode, rb); err != nil {
			switch {
			case resp.StatusCode == http.StatusTooManyRequests:
				secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
				return &retryAfterError{err: err, after: time.Duration(secs) * time.Second}
			case resp.StatusCode >= 500:
				return err
			default:
				return &permanentError{err}
			}
		}
		return nil
	})
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Telegram sends messages through the Bot API.
type Telegram struct {
	Token  string
	ChatID string
	// APIURL defaults to https://api.telegram.org.
	APIURL string
	HTTPOptions
}

// telegramMaxText is the sendMessage text limit.
const telegramMaxText = 4096

// Name implements Channel.
func (t *Telegram) Name() string { return "telegram" }

// Send implements Channel.
func (t *Telegram) Send(ctx context.Context, m *Message) error {
	api := strings.TrimRight(t.APIURL, "/")
	if api == "" {
		api = "https://api.telegram.org"
	}
	text := m.Plain()
	if r := []rune(text); len(r) > telegramMaxText {
		text = string(r[:telegramMaxText-1]) + "…"
	}
	body, _ := json.Marshal(map[string]any{"chat_id": t.ChatID, "text": text, "disable_web_page_preview": true})
	h := http.Header{"Content-Type": {"application/json"}}
	err := t.post(ctx, api+"/bot"+t.Token+"/sendMessage", body, h, func(status int, rb []byte) error {
		var r struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(rb, &r)
		if status == http.StatusOK && r.OK {
			return nil
		}
		if r.Description == "" {
			r.Description = http.StatusText(status)
		}
		return fmt.Errorf("telegram: HTTP %d: %s", status, r.Description)
	})
	if err != nil && t.Token != "" {
		// Belt and braces: never let the token out in an error.
		err = errors.New(strings.ReplaceAll(err.Error(), t.Token, "***"))
	}
	return err
}

// Webhook posts the Message as JSON.
type Webhook struct {
	// Label names the webhook in logs; defaults to the URL host.
	Label   string
	URL     string
	Secret  string // HMAC-SHA256 key for X-Awgkeen-Signature
	Headers map[string]string
	HTTPOptions
}

// Name implements Channel.
func (w *Webhook) Name() string {
	if w.Label != "" {
		return "webhook " + w.Label
	}
	if u, err := url.Parse(w.URL); err == nil && u.Host != "" {
		return "webhook " + u.Host
	}
	return "webhook"
}

// Signature returns the X-Awgkeen-Signature value for body.
func Signature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Send implements Channel.
func (w *Webhook) Send(ctx context.Context, m *Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	h := http.Header{"Content-Type": {"application/json"}, "User-Agent": {"awgkeen"}, "X-Awgkeen-Event": {m.Event}}
	for k, v := range w.Headers {
		h.Set(k, v)
	}
	if w.Secret != "" {
		h.Set("X-Awgkeen-Signature", Signature(w.Secret, body))
	}
	err = w.post(ctx, w.URL, body, h, func(status int, _ []byte) error {
		if status >= 200 && status < 300 {
			return nil
		}
		return fmt.Errorf("HTTP %d", status)
	})
	if err != nil {
		return fmt.Errorf("%s: %w", w.Name(), err)
	}
	return nil
}
