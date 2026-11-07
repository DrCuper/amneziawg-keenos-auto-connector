package server

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Decode the actual share wire format, including Qt's uncompressed-length prefix.
func decodeVPNShare(t *testing.T, key, wantContainer string) (map[string]any, map[string]any, map[string]any) {
	t.Helper()
	if !strings.HasPrefix(key, "vpn://") {
		t.Fatal("share lacks vpn scheme")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, "vpn://"))
	if err != nil || len(raw) < 4 {
		t.Fatal("invalid base64url or missing Qt length prefix")
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw[4:]))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if uint32(len(data)) != binary.BigEndian.Uint32(raw[:4]) {
		t.Fatal("Qt length prefix does not match JSON length")
	}
	var doc struct {
		DefaultContainer string `json:"defaultContainer"`
		Containers       []struct {
			Container string         `json:"container"`
			AWG       map[string]any `json:"awg"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.DefaultContainer != wantContainer || len(doc.Containers) != 1 || doc.Containers[0].Container != wantContainer {
		t.Fatal("unexpected protocol/container identity")
	}
	awg := doc.Containers[0].AWG
	lastJSON, ok := awg["last_config"].(string)
	if !ok {
		t.Fatal("last_config is not a JSON string")
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lastJSON), &last); err != nil {
		t.Fatal(err)
	}
	conf, ok := last["config"].(string)
	if !ok {
		t.Fatal("last_config lacks canonical config")
	}
	canonical := map[string]any{}
	for _, line := range strings.Split(conf, "\n") {
		if k, v, ok := strings.Cut(line, " = "); ok {
			canonical[k] = v
		}
	}
	return awg, last, canonical
}

func vpnShareFixture() (Config, *State, *Peer) {
	// Synthetic values only; never load managed state or print share/key material.
	c := Config{Name: "test", Endpoint: "vpn.example.test", AWG2: true}.WithDefaults()
	p := Peer{Name: "phone", Address: "10.8.1.2/32",
		Keys: KeyPair{Private: "synthetic-client-private", Public: "synthetic-client-public"}, PSK: "synthetic-psk"}
	s := &State{Keys: KeyPair{Private: "synthetic-server-private", Public: "synthetic-server-public"},
		Address: "10.8.1.1/24", Peers: []Peer{p},
		Params: Params{Jc: 4, Jmin: 10, Jmax: 50, S1: 101, S2: 69, S3: 23, S4: 47,
			H1: "1000-1999", H2: "2000-2999", H3: "3000-3999", H4: "4000-4999"}}
	return c, s, &s.Peers[0]
}

func TestVPNKeyAWG2ContainerIdentity(t *testing.T) {
	c, s, p := vpnShareFixture()
	key, err := VPNKey(c, s, p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, "vpn://"))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw[4:]))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var doc struct {
		DefaultContainer string `json:"defaultContainer"`
		Containers       []struct {
			Container string         `json:"container"`
			AWG       map[string]any `json:"awg"`
			AWG2      map[string]any `json:"awg2"`
		} `json:"containers"`
	}
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.DefaultContainer != "amnezia-awg2" {
		t.Errorf("defaultContainer = %q, want amnezia-awg2", doc.DefaultContainer)
	}
	if len(doc.Containers) != 1 {
		t.Fatal("want exactly one container")
	}
	container := doc.Containers[0]
	if container.Container != "amnezia-awg2" {
		t.Errorf("container = %q, want amnezia-awg2", container.Container)
	}
	if container.AWG == nil || container.AWG["last_config"] == nil || container.AWG2 != nil {
		t.Error("modern container must retain protocol metadata under awg, not awg2")
	}
}

func TestVPNKeyAWG2Metadata(t *testing.T) {
	c, s, p := vpnShareFixture()
	checkVPNKeyMetadata(t, c, s, p, "amnezia-awg2")
}

func TestVPNKeyAWG2ZeroPadding(t *testing.T) {
	for _, padding := range [][2]int{{0, 47}, {23, 0}, {0, 0}} {
		c, s, p := vpnShareFixture()
		s.Params.S3, s.Params.S4 = padding[0], padding[1]
		checkVPNKeyMetadata(t, c, s, p, "amnezia-awg2")
	}
}

func TestVPNKeyImportedAWG2WithoutGenerationFlag(t *testing.T) {
	c, s, p := vpnShareFixture()
	c.AWG2 = false // This flag controls generation, not imported/exported state.
	importConf := strings.Replace(amneziaConf, "H1 = 1111", "H1 = 1000-1999", 1)
	importConf = strings.Replace(importConf, "S2 = 69", "S2 = 69\nS3 = 23\nS4 = 47\nI1 = <b 0x01>\nI2 = <b 0x02>\nI3 = <b 0x03>\nI4 = <b 0x04>\nI5 = <b 0x05>", 1)
	si, err := parseServerInterface(importConf)
	if err != nil {
		t.Fatal(err)
	}
	s.Params = si.Params
	checkVPNKeyMetadata(t, c, s, p, "amnezia-awg2")
}

func TestVPNKeyAWG1Compatibility(t *testing.T) {
	c, s, p := vpnShareFixture()
	c.AWG2 = false
	s.Params.S3, s.Params.S4 = 0, 0
	s.Params.H1, s.Params.H2, s.Params.H3, s.Params.H4 = "1111", "2222", "3333", "4444"
	checkVPNKeyMetadata(t, c, s, p, "amnezia-awg")
	conf := ClientConf(c, s, p)
	if strings.Contains(conf, "S3 =") || strings.Contains(conf, "S4 =") || strings.Contains(conf, "1000-1999") {
		t.Error("AWG1 canonical configuration gained AWG2 parameters")
	}
}

func TestVPNKeyContainerIdentityFromPersistedParams(t *testing.T) {
	for _, tc := range []struct {
		name           string
		generationFlag bool
		s3, s4         int
		rangeIndex     int
		signature      bool
		want           string
	}{
		{name: "AWG1 flag drift", generationFlag: true, want: "amnezia-awg"},
		{name: "S3 only", s3: 23, want: "amnezia-awg2"},
		{name: "S4 only", s4: 47, want: "amnezia-awg2"},
		{name: "H1 range zero padding", rangeIndex: 1, want: "amnezia-awg2"},
		{name: "H2 range zero padding", rangeIndex: 2, want: "amnezia-awg2"},
		{name: "H3 range zero padding", rangeIndex: 3, want: "amnezia-awg2"},
		{name: "H4 range zero padding", rangeIndex: 4, want: "amnezia-awg2"},
		{name: "imported signature only", signature: true, want: "amnezia-awg2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s, p := vpnShareFixture()
			c.AWG2 = tc.generationFlag
			s.Params.S3, s.Params.S4 = tc.s3, tc.s4
			hs := [4]string{"1111", "2222", "3333", "4444"}
			if tc.rangeIndex != 0 {
				hs[tc.rangeIndex-1] += "-4999"
			}
			s.Params.H1, s.Params.H2, s.Params.H3, s.Params.H4 = hs[0], hs[1], hs[2], hs[3]
			if tc.signature {
				s.Params.I[4] = "<b 0x05>"
			}
			checkVPNKeyMetadata(t, c, s, p, tc.want)
		})
	}
}

func checkVPNKeyMetadata(t *testing.T, c Config, s *State, p *Peer, wantContainer string) {
	t.Helper()
	before, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	conf := ClientConf(c, s, p)
	serverConf := ServerConf(c, s, "")
	key, err := VPNKey(c, s, p)
	if err != nil {
		t.Fatal(err)
	}
	awg, last, canonical := decodeVPNShare(t, key, wantContainer)
	for _, field := range []string{"Jc", "Jmin", "Jmax", "S1", "S2", "S3", "S4", "H1", "H2", "H3", "H4"} {
		want, ok := canonical[field].(string)
		// Params.Lines omits S3/S4 when both are zero; their effective value is zero.
		if !ok && (field == "S3" || field == "S4") && s.Params.S3 == 0 && s.Params.S4 == 0 {
			want, ok = "0", true
		}
		if !ok {
			t.Fatalf("canonical config missing %s", field)
		}
		for name, metadata := range map[string]map[string]any{"awg": awg, "last_config": last} {
			if got, ok := metadata[field].(string); !ok || got != want {
				t.Errorf("%s.%s missing or differs from canonical config (want string %q)", name, field, want)
			}
		}
	}
	for i, field := range []string{"I1", "I2", "I3", "I4", "I5"} {
		if s.Params.I[i] != "" && canonical[field] != s.Params.I[i] {
			t.Errorf("canonical %s differs from imported state", field)
		}
	}
	if last["config"] != conf {
		t.Error("embedded config differs from ClientConf")
	}
	for field, want := range map[string]string{
		"client_priv_key": p.Keys.Private, "client_pub_key": p.Keys.Public,
		"server_pub_key": s.Keys.Public, "psk_key": p.PSK,
	} {
		if last[field] != want {
			t.Errorf("%s differs from state", field)
		}
	}
	if canonical["PrivateKey"] != p.Keys.Private || canonical["PublicKey"] != s.Keys.Public || canonical["PresharedKey"] != p.PSK {
		t.Error("canonical config key material differs from state")
	}
	after, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || ClientConf(c, s, p) != conf || ServerConf(c, s, "") != serverConf {
		t.Error("export changed state or canonical configurations")
	}
}
