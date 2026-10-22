package rci

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

func TestInterfaceStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth":
			w.WriteHeader(200) // local access without credentials
		case r.URL.Path == "/rci/show/interface" && r.URL.Query().Get("name") == "Wireguard0":
			_, _ = io.WriteString(w, `{"id":"Wireguard0","type":"Wireguard","link":"up","connected":"yes","state":"up",
				"wireguard":{"public-key":"x","peer":[{"public-key":"P1","remote":"203.0.113.1","remote-port":51820,
				"rxbytes":10,"txbytes":20,"last-handshake":7,"online":true}]}}`)
		default:
			_, _ = io.WriteString(w, `{"status":[{"status":"error","code":"7405600","message":"unable to find Wireguard9"}]}`)
		}
	}))
	defer srv.Close()
	tr, err := New(Config{URL: srv.URL, Username: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := tr.InterfaceStatus(context.Background(), "Wireguard0")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exists || !s.Up || len(s.Peers) != 1 || s.Peers[0].Endpoint != "203.0.113.1:51820" || s.Peers[0].LastHandshake.Seconds() != 7 || !s.Peers[0].Online {
		t.Errorf("status %+v", s)
	}
	_, err = tr.InterfaceStatus(context.Background(), "Wireguard9")
	var ce *router.CommandError
	if !errors.As(err, &ce) {
		t.Errorf("want CommandError, got %v", err)
	}
}
