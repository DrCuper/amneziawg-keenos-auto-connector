package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// testServer is a tiny SSH server that answers exec requests like a
// Keenetic console would.
type testServer struct {
	addr   string
	pub    ssh.PublicKey
	execs  []string
	reject string
}

func startServer(t *testing.T) *testServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv := &testServer{pub: signer.PublicKey()}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == "admin" && string(pw) == "pw" {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.addr = ln.Addr().String()
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handle(conn, cfg)
		}
	}()
	return srv
}

func (s *testServer) handle(conn net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for r := range creqs {
				if r.Type != "exec" {
					_ = r.Reply(false, nil)
					continue
				}
				cmd := string(r.Payload[4:])
				s.execs = append(s.execs, cmd)
				_ = r.Reply(true, nil)
				switch cmd {
				case "show running-config":
					fmt.Fprint(ch, "object-group fqdn x\n    include a.com\n")
				case s.reject:
					fmt.Fprint(ch, "Command::Base error[7405600]: argument parse error.\n")
				default:
					fmt.Fprint(ch, "\n")
				}
				_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
				return
			}
		}()
	}
}

func knownHosts(t *testing.T, s *testServer) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "known_hosts")
	host, port, _ := net.SplitHostPort(s.addr)
	line := fmt.Sprintf("[%s]:%s %s\n", host, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.pub))))
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecAndRunningConfig(t *testing.T) {
	s := startServer(t)
	s.reject = "bad cmd"
	tr, err := New(Config{Host: s.addr, Username: "admin", Password: "pw", KnownHostsFile: knownHosts(t, s)})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	cfg, err := tr.RunningConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "include a.com") {
		t.Errorf("config %q", cfg)
	}
	out, err := tr.Exec(context.Background(), []string{"ok1", "bad cmd", "never"})
	var ce *router.CommandError
	if !errors.As(err, &ce) || ce.Command != "bad cmd" || !strings.Contains(ce.Message, "argument parse error") {
		t.Fatalf("got %v", err)
	}
	if len(out) != 1 {
		t.Errorf("outputs %q", out)
	}
	if strings.Join(s.execs, ",") != "show running-config,ok1,bad cmd" {
		t.Errorf("execs %q", s.execs)
	}
}

func TestHostKeyMismatch(t *testing.T) {
	s := startServer(t)
	p := filepath.Join(t.TempDir(), "known_hosts")
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	host, port, _ := net.SplitHostPort(s.addr)
	_ = os.WriteFile(p, []byte(fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(other.PublicKey()))), 0o600)
	tr, err := New(Config{Host: s.addr, Username: "admin", Password: "pw", KnownHostsFile: p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RunningConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("expected host key error, got %v", err)
	}
}

func TestBadPassword(t *testing.T) {
	s := startServer(t)
	tr, _ := New(Config{Host: s.addr, Username: "admin", Password: "nope", InsecureHostKey: true})
	_, err := tr.RunningConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || router.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
}

func TestConnectionRefusedIsTransport(t *testing.T) {
	tr, _ := New(Config{Host: "127.0.0.1:1", Username: "admin", Password: "x", InsecureHostKey: true})
	_, err := tr.RunningConfig(context.Background())
	if !router.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{Host: "h", Username: "u", Password: "p"}); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("got %v", err)
	}
	if _, err := New(Config{Host: "h", Username: "u", InsecureHostKey: true}); err == nil || !strings.Contains(err.Error(), "password or key") {
		t.Errorf("got %v", err)
	}
	if _, err := New(Config{Host: "h", Username: "u", KeyFile: "/nonexistent", InsecureHostKey: true}); err == nil {
		t.Error("expected key file error")
	}
}
