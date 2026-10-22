package remote

import (
	"context"
	"errors"
	"testing"
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
