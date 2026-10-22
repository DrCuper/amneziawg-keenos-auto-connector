package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"strconv"

	"golang.org/x/crypto/curve25519"
)

// KeyPair is a WireGuard key pair in base64.
type KeyPair struct {
	Private string `json:"private_key"`
	Public  string `json:"public_key"`
}

// NewKeyPair generates a clamped Curve25519 key pair.
func NewKeyPair() (KeyPair, error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return KeyPair{}, err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	pub, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Private: base64.StdEncoding.EncodeToString(k[:]), Public: base64.StdEncoding.EncodeToString(pub)}, nil
}

// NewPSK generates a preshared key.
func NewPSK() (string, error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), nil
}

// Params are the AmneziaWG obfuscation parameters shared by the server and
// all its clients.
type Params struct {
	Jc, Jmin, Jmax int
	S1, S2         int
	H1, H2, H3, H4 string // a number, or a low-high range (AWG 2.0)
	// AWG 2.0 only.
	S3, S4 int `json:",omitempty"`
}

// Lines returns the `Key = value` lines for an awg-quick [Interface].
func (p Params) Lines() []string {
	out := []string{
		"Jc = " + strconv.Itoa(p.Jc), "Jmin = " + strconv.Itoa(p.Jmin), "Jmax = " + strconv.Itoa(p.Jmax),
		"S1 = " + strconv.Itoa(p.S1), "S2 = " + strconv.Itoa(p.S2),
	}
	if p.S3 != 0 || p.S4 != 0 {
		out = append(out, "S3 = "+strconv.Itoa(p.S3), "S4 = "+strconv.Itoa(p.S4))
	}
	return append(out, "H1 = "+p.H1, "H2 = "+p.H2, "H3 = "+p.H3, "H4 = "+p.H4)
}

func randInt(lo, hi int) (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(hi-lo+1)))
	if err != nil {
		return 0, err
	}
	return lo + int(n.Int64()), nil
}

// NewParams generates random parameters within the AmneziaWG limits:
// Jc 3..10, Jmin < Jmax <= 1280, S1 <= 1132, S2 <= 1188 and S1+56 != S2,
// H1..H4 distinct values above the plain WireGuard message types 1..4. With
// awg2, H values become disjoint ranges and S3/S4 are set.
func NewParams(awg2 bool) (Params, error) {
	var p Params
	var err error
	pick := func(lo, hi int) int {
		if err != nil {
			return 0
		}
		var v int
		v, err = randInt(lo, hi)
		return v
	}
	p.Jc = pick(3, 10)
	p.Jmin = pick(10, 50)
	p.Jmax = pick(p.Jmin+20, 250)
	p.S1 = pick(15, 150)
	for {
		p.S2 = pick(15, 150)
		if err != nil || p.S1+56 != p.S2 {
			break
		}
	}
	if awg2 {
		p.S3 = pick(15, 100)
		p.S4 = pick(15, 100)
	}
	if err != nil {
		return Params{}, err
	}
	hs, err := headers(awg2)
	if err != nil {
		return Params{}, err
	}
	p.H1, p.H2, p.H3, p.H4 = hs[0], hs[1], hs[2], hs[3]
	return p, nil
}

// headers draws four distinct values (or disjoint ranges) in 5..2^31-1.
func headers(ranges bool) ([4]string, error) {
	var out [4]string
	if ranges {
		// Four disjoint quarter-bands, a random sub-range in each.
		const band = (1<<31 - 6) / 4
		for i := range out {
			lo := 5 + i*band
			a, err := randInt(lo, lo+band/2)
			if err != nil {
				return out, err
			}
			w, err := randInt(1000, band/4)
			if err != nil {
				return out, err
			}
			out[i] = fmt.Sprintf("%d-%d", a, a+w)
		}
		return out, nil
	}
	seen := map[uint32]bool{}
	for i := 0; i < 4; {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return out, err
		}
		v := binary.BigEndian.Uint32(b[:]) & 0x7fffffff
		if v < 5 || seen[v] {
			continue
		}
		seen[v] = true
		out[i] = strconv.FormatUint(uint64(v), 10)
		i++
	}
	return out, nil
}
