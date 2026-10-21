package source

import (
	"strings"
	"testing"
)

func TestParseFormats(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"example.com", "example.com"},
		{"*.example.com", "example.com"},
		{".example.com", "example.com"},
		{"0.0.0.0 example.com", "example.com"},
		{"127.0.0.1\texample.com", "example.com"},
		{"::1 example.com", "example.com"},
		{"server=/example.com/1.1.1.1", "example.com"},
		{"ipset=/example.com/vpn", "example.com"},
		{"nftset=/example.com/4#inet#fw#set", "example.com"},
		{"address=/example.com/0.0.0.0", "example.com"},
		{"domain:example.com", "example.com"},
		{"domain:example.com @ads", "example.com"},
		{"full:example.com", "example.com"},
		{"DOMAIN-SUFFIX,example.com", "example.com"},
		{"DOMAIN,example.com,Proxy", "example.com"},
		{"https://example.com/path?x=1", "example.com"},
		{"http://user@example.com:8080/", "example.com"},
		{"example.com # trailing comment", "example.com"},
		{"203.0.113.0/24", "203.0.113.0/24"},
		{"2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		got, st := Parse(FormatAuto, c.line)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("Parse(%q) = %v (%+v), want [%s]", c.line, got, st, c.want)
		}
	}
}

func TestParseSkipped(t *testing.T) {
	skipped := []string{
		"regexp:^foo", "keyword:google", "include:geolocation-cn",
		"IP-CIDR,10.0.0.0/8,Proxy", "DOMAIN-KEYWORD,google", "GEOIP,CN",
		"conf-dir=/etc/dnsmasq.d", "???", "some words here", "example.com/path",
	}
	for _, line := range skipped {
		got, st := Parse(FormatAuto, line)
		if len(got) != 0 || st.Skipped != 1 || st.Lines != 1 {
			t.Errorf("Parse(%q) = %v %+v, want skipped", line, got, st)
		}
	}
}

func TestParseMixedAndComments(t *testing.T) {
	text := strings.Join([]string{
		"# comment", "// another", "! adblock style", "; ini style", "",
		"example.com", "*.cdn.example.net", "0.0.0.0 ads.example.org",
		"server=/x.example.io/1.1.1.1", "DOMAIN-SUFFIX,y.example.io", "???",
	}, "\n")
	got, st := Parse(FormatAuto, text)
	want := []string{"example.com", "cdn.example.net", "ads.example.org", "x.example.io", "y.example.io"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if st.Lines != 6 || st.Parsed != 5 || st.Skipped != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestParseExplicitFormat(t *testing.T) {
	// In plain mode a hosts line is garbage, and vice versa.
	if got, _ := Parse(FormatPlain, "0.0.0.0 example.com"); len(got) != 0 {
		t.Fatalf("plain accepted hosts line: %v", got)
	}
	if got, _ := Parse(FormatHosts, "example.com"); len(got) != 0 {
		t.Fatalf("hosts accepted plain line: %v", got)
	}
	if got, _ := Parse(FormatClash, "example.com"); len(got) != 0 {
		t.Fatalf("clash accepted plain line: %v", got)
	}
	if got, _ := Parse(FormatV2fly, "example.com"); len(got) != 1 {
		t.Fatalf("v2fly should accept bare domains: %v", got)
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"", "auto", "Plain", "hosts", "dnsmasq", "v2fly", "clash"} {
		if _, err := ParseFormat(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("expected error for xml")
	}
}
