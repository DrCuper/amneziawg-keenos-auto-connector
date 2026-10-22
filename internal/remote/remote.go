// Package remote runs shell commands on an AmneziaWG server host: over SSH
// for a VPS, or locally when awgkeen runs on the server itself. It is used
// by package server to install, configure and query amneziawg.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Runner executes a POSIX shell script and returns its stdout. A non-zero
// exit status is an *ExitError carrying stderr.
type Runner interface {
	Run(ctx context.Context, script string, stdin []byte) (string, error)
	Close() error
}

// ExitError is a command that ran and failed.
type ExitError struct {
	Status int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 500 {
		msg = "…" + msg[len(msg)-500:]
	}
	if msg == "" {
		return fmt.Sprintf("exit status %d", e.Status)
	}
	return fmt.Sprintf("exit status %d: %s", e.Status, msg)
}

// Local runs scripts with /bin/sh on this machine.
type Local struct {
	// Sudo prefixes scripts with `sudo -n`.
	Sudo bool
}

// Run implements Runner.
func (l Local) Run(ctx context.Context, script string, stdin []byte) (string, error) {
	argv := []string{"/bin/sh", "-c", script}
	if l.Sudo {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // scripts are built by package server
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), &ExitError{Status: ee.ExitCode(), Stderr: errb.String()}
	}
	return out.String(), err
}

// Close implements Runner.
func (Local) Close() error { return nil }

// SSHConfig configures an SSH runner.
type SSHConfig struct {
	Host            string // host[:port]
	User            string
	Password        string
	KeyFile         string
	KnownHostsFile  string
	InsecureHostKey bool
	Sudo            bool // prefix scripts with `sudo -n` (non-root user)
	Timeout         time.Duration
}

// SSH runs scripts in exec sessions over one kept-alive connection.
type SSH struct {
	cfg  SSHConfig
	addr string
	ccfg *ssh.ClientConfig

	mu     sync.Mutex
	client *ssh.Client
}

// NewSSH validates the configuration; it does not connect.
func NewSSH(cfg SSHConfig) (*SSH, error) {
	if cfg.Host == "" || cfg.User == "" {
		return nil, errors.New("ssh: host and user are required")
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
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	return &SSH{cfg: cfg, addr: addr, ccfg: &ssh.ClientConfig{User: cfg.User, Auth: auth, HostKeyCallback: hostKey, Timeout: cfg.Timeout}}, nil
}

// Close drops the connection.
func (s *SSH) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil
	}
	err := s.client.Close()
	s.client = nil
	return err
}

func (s *SSH) connect(ctx context.Context) (*ssh.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	d := net.Dialer{Timeout: s.cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("ssh: dial %s: %w", s.addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(s.cfg.Timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, s.addr, s.ccfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh: %s: %w", s.addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	s.client = ssh.NewClient(c, chans, reqs)
	return s.client, nil
}

func (s *SSH) drop(c *ssh.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == c {
		_ = c.Close()
		s.client = nil
	}
}

// Run implements Runner. The script runs under `sh -c` on the remote host.
func (s *SSH) Run(ctx context.Context, script string, stdin []byte) (string, error) {
	c, err := s.connect(ctx)
	if err != nil {
		return "", err
	}
	sess, err := c.NewSession()
	if err != nil {
		s.drop(c)
		return "", fmt.Errorf("ssh: new session: %w", err)
	}
	defer func() { _ = sess.Close() }()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	cmd := "sh -c " + ShellQuote(script)
	if s.cfg.Sudo {
		cmd = "sudo -n " + cmd
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		s.drop(c)
		return "", ctx.Err()
	case err := <-done:
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return out.String(), &ExitError{Status: ee.ExitStatus(), Stderr: errb.String()}
		}
		if err != nil {
			s.drop(c)
			return out.String(), fmt.Errorf("ssh: %w", err)
		}
		return out.String(), nil
	}
}

// ShellQuote wraps s in single quotes for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
