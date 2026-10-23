package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/lock"
)

// ErrNotInitialized is returned before `server init`.
var ErrNotInitialized = errors.New("server is not initialized (run `awgkeen server init`)")

// Peer is one client of the server. Its private key is kept so that the
// client configuration can be exported again (the state file is 0600).
type Peer struct {
	Name      string    `json:"name"`
	Address   string    `json:"address"` // 10.8.1.2/32
	Keys      KeyPair   `json:"keys"`
	PSK       string    `json:"preshared_key"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	RotatedAt time.Time `json:"rotated_at,omitzero"`
}

// State is everything generated for one server.
type State struct {
	Keys   KeyPair `json:"keys"`
	Params Params  `json:"params"`
	// Address is the server's tunnel address with the subnet length.
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
	Peers     []Peer    `json:"peers"`
	// DeployedHash is the sha256 of the last configuration written to the host.
	DeployedHash string    `json:"deployed_hash,omitempty"`
	DeployedAt   time.Time `json:"deployed_at,omitzero"`
	// Amnezia is set for a server installed by the AmneziaVPN app and adopted
	// from its admin key: its interface stays as the app made it.
	Amnezia *AmneziaHost `json:"amnezia,omitempty"`
}

// AmneziaHost locates the AWG container of an Amnezia server.
type AmneziaHost struct {
	Container  string `json:"container"`   // amnezia-awg
	Interface  string `json:"interface"`   // wg0
	ConfigPath string `json:"config_path"` // inside the container
	ListenPort int    `json:"listen_port"`
	Subnet     string `json:"subnet"` // 10.8.1.0/24
}

// Peer returns the peer by name.
func (s *State) Peer(name string) (*Peer, bool) {
	for i := range s.Peers {
		if s.Peers[i].Name == name {
			return &s.Peers[i], true
		}
	}
	return nil, false
}

// NextAddress returns the lowest free host address in the subnet; extra
// lists addresses taken by peers awgkeen does not manage.
func (s *State) NextAddress(subnet netip.Prefix, extra ...netip.Addr) (netip.Addr, error) {
	used := map[netip.Addr]bool{}
	for _, a := range extra {
		used[a] = true
	}
	if p, err := netip.ParsePrefix(s.Address); err == nil {
		used[p.Addr()] = true
	}
	for _, p := range s.Peers {
		if a, err := netip.ParsePrefix(p.Address); err == nil {
			used[a.Addr()] = true
		}
	}
	last := lastAddr(subnet)
	for a := subnet.Addr().Next(); a.IsValid() && a.Less(last); a = a.Next() {
		if !used[a] {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("subnet %s is full", subnet)
}

// lastAddr is the broadcast address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= (1 << host) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Store keeps a server's State in a JSON file under a file lock.
type Store struct {
	Path string
	lk   *lock.Lock
}

// NewStore returns the store for <dir>/<name>.json.
func NewStore(dir, name string) *Store {
	p := filepath.Join(dir, name+".json")
	return &Store{Path: p, lk: lock.New(p + ".lock")}
}

// Load reads the state.
func (st *Store) Load() (*State, error) {
	data, err := os.ReadFile(st.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", st.Path, err)
	}
	return &s, nil
}

// Update loads the state (nil when not initialized), lets fn change it and
// writes it back atomically, all under the lock.
func (st *Store) Update(ctx context.Context, fn func(*State) (*State, error)) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(st.Path), 0o700); err != nil {
		return nil, err
	}
	release, err := st.lk.Acquire(ctx, time.Minute)
	if err != nil {
		return nil, err
	}
	defer release()
	cur, err := st.Load()
	if err != nil && !errors.Is(err, ErrNotInitialized) {
		return nil, err
	}
	next, err := fn(cur)
	if err != nil || next == nil {
		return cur, err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := st.Path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	return next, os.Rename(tmp, st.Path)
}
