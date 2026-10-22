package router_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router/fake"
)

const (
	privKey = "YUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="
	peerKey = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="
)

func wgWant(setKeys bool) *router.Desired {
	return &router.Desired{Interfaces: []router.WGInterface{{
		Name: "Wireguard0", Description: "t", Address: "10.0.0.2/32", AdjustMSS: true, Up: true,
		PrivateKey: privKey, SetKeys: setKeys,
		Peers: []router.WGPeer{{PublicKey: peerKey, Endpoint: "h:1", AllowedIPs: []string{"0.0.0.0/0"}}},
	}}}
}

func TestApplyInterfaceVerifiesDespiteHiddenKeys(t *testing.T) {
	f := fake.New("")
	c := newClient(t, f, nil)
	res, err := c.Apply(context.Background(), wgWant(true), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied == 0 || f.Saved != 1 {
		t.Errorf("result %+v saved %d", res, f.Saved)
	}
	w, ok := f.State.Interface("Wireguard0")
	if !ok || w.PrivateKey != privKey || !w.Up {
		t.Fatalf("interface %+v", w)
	}
	// Second run without SetKeys: nothing to do.
	res, err = c.Apply(context.Background(), wgWant(false), false)
	if err != nil || res.Applied != 0 {
		t.Errorf("second apply: %+v %v", res, err)
	}
}

func TestApplyRedactsKeysInErrors(t *testing.T) {
	f := fake.New("")
	f.RejectCommand = "interface Wireguard0 wireguard private-key " + privKey
	c := newClient(t, f, nil)
	_, err := c.Apply(context.Background(), wgWant(true), false)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), privKey) {
		t.Errorf("error leaks the key: %v", err)
	}
	var ce *router.CommandError
	if !errors.As(err, &ce) || strings.Contains(ce.Command, privKey) {
		t.Errorf("command error %+v", ce)
	}
}

func TestInterfaceStatusMissing(t *testing.T) {
	c := newClient(t, fake.New(""), nil)
	s, err := c.InterfaceStatus(context.Background(), "Wireguard5")
	if err != nil || s.Exists {
		t.Errorf("status %+v err %v", s, err)
	}
}

func TestDesiredValidation(t *testing.T) {
	d := &router.Desired{Interfaces: []router.WGInterface{{Name: "Wg0"}, {Name: "Wireguard1", PrivateKey: "short", ASC: []string{"1"}}}}
	err := d.Normalize(router.Limits{})
	if err == nil || !strings.Contains(err.Error(), "Wireguard<N>") || !strings.Contains(err.Error(), "asc needs") {
		t.Errorf("err %v", err)
	}
	if strings.Contains(err.Error(), "short") {
		t.Errorf("error echoes the key: %v", err)
	}
}

func TestCanonicalPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"0.0.0.0 0.0.0.0":          "0.0.0.0/0",
		"10.8.1.2 255.255.255.255": "10.8.1.2/32",
		"10.8.1.2":                 "10.8.1.2/32",
		"192.168.0.0/16":           "192.168.0.0/16",
		"::/0":                     "::/0",
	} {
		if got, err := router.CanonicalPrefix(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := router.CanonicalPrefix("10.0.0.0 255.0.255.0"); err == nil {
		t.Error("non-contiguous mask accepted")
	}
	if got := router.PrefixMask("10.0.0.0/8"); got != "10.0.0.0 255.0.0.0" {
		t.Errorf("PrefixMask %q", got)
	}
}
