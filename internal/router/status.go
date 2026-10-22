package router

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// WGStatus is the runtime state of a Wireguard interface (`show interface`).
type WGStatus struct {
	Name      string
	Exists    bool
	Up        bool // state/link up
	Connected bool
	Peers     []WGPeerStatus
}

// WGPeerStatus is the runtime state of one peer.
type WGPeerStatus struct {
	PublicKey string
	Endpoint  string // remote:port as seen by the router
	Online    bool
	// LastHandshake is the time since the last handshake; negative when
	// there was none.
	LastHandshake time.Duration
	RxBytes       uint64
	TxBytes       uint64
}

// StatusTransport is implemented by transports that can read interface
// status. RCI, SSH and the fake all do.
type StatusTransport interface {
	InterfaceStatus(ctx context.Context, name string) (*WGStatus, error)
}

// ErrStatusUnsupported is returned when the transport cannot read status.
var ErrStatusUnsupported = errors.New("transport cannot read interface status")

// InterfaceStatus reads the runtime state of an interface, with retries.
// A router rejecting the command (no such interface) yields Exists=false.
func (c *Client) InterfaceStatus(ctx context.Context, name string) (*WGStatus, error) {
	st, ok := c.t.(StatusTransport)
	if !ok {
		return nil, ErrStatusUnsupported
	}
	var out *WGStatus
	err := c.retry(ctx, "show interface", func(ctx context.Context) error {
		var err error
		out, err = st.InterfaceStatus(ctx, name)
		return err
	})
	var ce *CommandError
	if errors.As(err, &ce) {
		return &WGStatus{Name: name}, nil
	}
	return out, err
}

// neverHandshake is what the firmware reports when there was no handshake
// (seconds since the epoch or int32 max, depending on the version).
const neverHandshake = 1 << 30

// ParseWGStatus extracts the status from a `show interface` tree: the JSON
// object returned by RCI or the key/value tree of the console output.
func ParseWGStatus(name string, tree map[string]any) *WGStatus {
	s := &WGStatus{Name: name}
	if len(tree) == 0 {
		return s
	}
	if _, ok := tree["id"]; !ok {
		if _, ok := tree["state"]; !ok {
			return s // error answer, no interface
		}
	}
	s.Exists = true
	s.Up = str(tree["state"]) == "up" || (str(tree["state"]) == "" && str(tree["link"]) == "up")
	s.Connected = truthy(tree["connected"])
	wg, _ := tree["wireguard"].(map[string]any)
	var peers []any
	switch p := wg["peer"].(type) {
	case []any:
		peers = p
	case map[string]any:
		peers = []any{p}
	}
	for _, x := range peers {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		ps := WGPeerStatus{PublicKey: str(m["public-key"]), Online: truthy(m["online"]), LastHandshake: -1}
		if r := str(m["remote"]); r != "" && r != "0.0.0.0" {
			ps.Endpoint = r
			if port := str(m["remote-port"]); port != "" && port != "0" {
				ps.Endpoint += ":" + port
			}
		}
		ps.RxBytes, _ = strconv.ParseUint(str(m["rxbytes"]), 10, 64)
		ps.TxBytes, _ = strconv.ParseUint(str(m["txbytes"]), 10, 64)
		if hs, err := strconv.ParseInt(str(m["last-handshake"]), 10, 64); err == nil && hs >= 0 && hs < neverHandshake {
			if hs > 0 || ps.RxBytes > 0 {
				ps.LastHandshake = time.Duration(hs) * time.Second
			}
		}
		s.Peers = append(s.Peers, ps)
	}
	return s
}

func str(v any) string {
	switch v := v.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	}
	return ""
}

func truthy(v any) bool {
	switch str(v) {
	case "yes", "true", "up", "1":
		return true
	}
	return false
}
