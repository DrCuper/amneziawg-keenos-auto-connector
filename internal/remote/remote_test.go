package remote

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestLocal(t *testing.T) {
	ctx := context.Background()
	out, err := Local{}.Run(ctx, "cat; echo "+ShellQuote("it's"), []byte("in:"))
	if err != nil || out != "in:it's\n" {
		t.Fatalf("out %q err %v", out, err)
	}
	_, err = Local{}.Run(ctx, "echo boom >&2; exit 3", nil)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Status != 3 || ee.Error() != "exit status 3: boom" {
		t.Fatalf("err %v", err)
	}
}

func TestNewSSHValidation(t *testing.T) {
	if _, err := NewSSH(SSHConfig{Host: "h", User: "root", Password: "p"}); err == nil {
		t.Error("missing host key policy accepted")
	}
	if _, err := NewSSH(SSHConfig{Host: "h", User: "root", InsecureHostKey: true}); err == nil {
		t.Error("missing auth accepted")
	}
	if _, err := NewSSH(SSHConfig{Host: "h", User: "root", Password: "p", InsecureHostKey: true}); err != nil {
		t.Error(err)
	}
}

func TestTOFU(t *testing.T) {
	path := t.TempDir() + "/kh"
	cb := tofu(path)
	k1 := testKey(t)
	k2 := testKey(t)
	addr := &net.TCPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 22}
	if err := cb("203.0.113.1:22", addr, k1); err != nil {
		t.Fatal(err)
	}
	if err := cb("203.0.113.1:22", addr, k1); err != nil {
		t.Fatalf("same key rejected: %v", err)
	}
	if err := cb("203.0.113.1:22", addr, k2); err == nil {
		t.Fatal("changed host key accepted")
	}
}

func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
