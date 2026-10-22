// Package sshx implements router.Transport over SSH to the KeeneticOS CLI.
//
// Every command runs in its own exec session (`ssh admin@router "<cmd>"`),
// which the Keenetic console accepts. The running configuration is read with
// `show running-config`.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/cli"
)

// Config for the SSH transport.
type Config struct {
	Host     string // host or host:port; default port 22
	Username string
	Password string
	KeyFile  string // PEM private key; used in addition to password if both set
	// KnownHostsFile verifies the host key. Required unless InsecureHostKey.
	KnownHostsFile string
	// InsecureHostKey accepts any host key. Only for lab use.
	InsecureHostKey bool
	// DialTimeout bounds the TCP+handshake phase. 0 = 10s.
	DialTimeout time.Duration
}

// Transport is the SSH transport. It keeps one connection open and reconnects
// on failure. Safe for concurrent use.
type Transport struct {
	cfg  Config
	addr string
	ccfg *ssh.ClientConfig

	mu     sync.Mutex
	client *ssh.Client
}

var _ router.Transport = (*Transport)(nil)

// New validates the configuration and prepares the ssh.ClientConfig. It does
// not connect.
func New(cfg Config) (*Transport, error) {
	if cfg.Host == "" || cfg.Username == "" {
		return nil, errors.New("ssh: host and username are required")
	}
	addr := cfg.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22")
	}
	var auth []ssh.AuthMethod
	if cfg.KeyFile != "" {
		pem, err := os.ReadFile(cfg.KeyFile) //nolint:gosec // operator-supplied path
		if err != nil {
			return nil, fmt.Errorf("ssh: read key file: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, fmt.Errorf("ssh: parse key file: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		auth = append(auth, ssh.Password(cfg.Password))
		auth = append(auth, ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			ans := make([]string, len(questions))
			for i := range ans {
				ans[i] = cfg.Password
			}
			return ans, nil
		}))
	}
	if len(auth) == 0 {
		return nil, errors.New("ssh: password or key file is required")
	}
	var hostKey ssh.HostKeyCallback
	switch {
	case cfg.InsecureHostKey:
		hostKey = ssh.InsecureIgnoreHostKey() //nolint:gosec // explicit operator opt-in
	case cfg.KnownHostsFile != "":
		cb, err := knownhosts.New(cfg.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("ssh: known_hosts: %w", err)
		}
		hostKey = cb
	default:
		return nil, errors.New("ssh: known_hosts_file is required unless insecure_host_key is set")
	}
	dial := cfg.DialTimeout
	if dial <= 0 {
		dial = 10 * time.Second
	}
	return &Transport{
		cfg:  cfg,
		addr: addr,
		ccfg: &ssh.ClientConfig{User: cfg.Username, Auth: auth, HostKeyCallback: hostKey, Timeout: dial},
	}, nil
}

// Close drops the connection.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		err := t.client.Close()
		t.client = nil
		return err
	}
	return nil
}

// RunningConfig runs `show running-config`.
func (t *Transport) RunningConfig(ctx context.Context) (string, error) {
	out, err := t.run(ctx, cli.ShowRunningConfig)
	if err != nil {
		return "", err
	}
	if msg := errorLine(out); msg != "" {
		return "", &router.TransportError{Op: cli.ShowRunningConfig, Err: errors.New(msg)}
	}
	return out, nil
}

// Exec runs commands one per session, stopping at the first error.
func (t *Transport) Exec(ctx context.Context, commands []string) ([]string, error) {
	var outputs []string
	for _, c := range commands {
		out, err := t.run(ctx, c)
		if err != nil {
			return outputs, err
		}
		if msg := errorLine(out); msg != "" {
			return outputs, &router.CommandError{Command: c, Message: msg}
		}
		outputs = append(outputs, strings.TrimSpace(out))
	}
	return outputs, nil
}

// errorRe matches the Keenetic console's error reports, e.g.
//
//	Command::Base error[7405600]: argument parse error.
//	Core::Configurator error[...]: ...
//	Error: ...
var errorRe = regexp.MustCompile(`(?m)^\s*(?:[A-Za-z:]+ error\[[^\]]*\]:|Error:)\s*(.*)$`)

func errorLine(out string) string {
	if m := errorRe.FindStringSubmatch(out); m != nil {
		return strings.TrimSpace(m[0])
	}
	return ""
}

func (t *Transport) run(ctx context.Context, cmd string) (string, error) {
	client, err := t.connect(ctx)
	if err != nil {
		return "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		t.drop(client)
		return "", &router.TransportError{Op: "new session", Err: err}
	}
	defer func() { _ = sess.Close() }()

	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := sess.CombinedOutput(cmd)
		done <- result{out, err}
	}()
	select {
	case <-ctx.Done():
		t.drop(client)
		return "", &router.TransportError{Op: cmd, Err: ctx.Err()}
	case r := <-done:
		if r.err != nil {
			var exitErr *ssh.ExitError
			if errors.As(r.err, &exitErr) {
				// The console exited non-zero: treat the output as the message.
				msg := strings.TrimSpace(string(r.out))
				if msg == "" {
					msg = r.err.Error()
				}
				return "", &router.CommandError{Command: cmd, Message: msg}
			}
			t.drop(client)
			return "", &router.TransportError{Op: cmd, Err: r.err}
		}
		return string(r.out), nil
	}
}

func (t *Transport) connect(ctx context.Context) (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		return t.client, nil
	}
	d := net.Dialer{Timeout: t.ccfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return nil, &router.TransportError{Op: "dial " + t.addr, Err: err}
	}
	_ = conn.SetDeadline(time.Now().Add(t.ccfg.Timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, t.addr, t.ccfg)
	if err != nil {
		_ = conn.Close()
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("ssh: authentication failed for user %q: %w", t.cfg.Username, err)
		}
		return nil, &router.TransportError{Op: "handshake " + t.addr, Err: err}
	}
	_ = conn.SetDeadline(time.Time{})
	t.client = ssh.NewClient(c, chans, reqs)
	return t.client, nil
}

func (t *Transport) drop(c *ssh.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == c {
		_ = c.Close()
		t.client = nil
	}
}

// InterfaceStatus runs `show interface <name>` and parses the console tree.
func (t *Transport) InterfaceStatus(ctx context.Context, name string) (*router.WGStatus, error) {
	cmd := cli.ShowInterface(name)
	out, err := t.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if msg := errorLine(out); msg != "" {
		return nil, &router.CommandError{Command: cmd, Message: msg}
	}
	return router.ParseWGStatus(name, cli.ParseShow(out)), nil
}
