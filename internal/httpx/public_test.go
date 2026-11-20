package httpx

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestPublicAddrRefusesEveryInternalRange(t *testing.T) {
	for raw, want := range map[string]bool{
		"1.1.1.1":              true,
		"2606:4700:4700::1111": true,
		"127.0.0.1":            false,
		"10.0.0.8":             false,
		"172.16.4.4":           false,
		"192.168.1.1":          false,
		"169.254.169.254":      false,
		"100.64.0.1":           false,
		"0.0.0.0":              false,
		"::1":                  false,
		"fd00::1":              false,
		"fe80::1":              false,
		"::ffff:127.0.0.1":     false,
		"::ffff:192.168.0.1":   false,
		"64:ff9b::a00:1":       false,
		"224.0.0.1":            false,
		"fec0::1":              false,
		"2001::1":              false,
		"::7f00:1":             false,
		"255.255.255.255":      false,
	} {
		if got := PublicAddr(netip.MustParseAddr(raw)); got != want {
			t.Fatalf("%s: got %v", raw, got)
		}
	}
}

func TestRedirectsStayOnAllowedHostsAndSchemes(t *testing.T) {
	check := redirects(func(host string) bool { return host == "cdn.example.com" })
	req := func(raw string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, raw, nil)
		return r
	}
	if err := check(req("https://cdn.example.com/a.mp3"), make([]*http.Request, 1)); err != nil {
		t.Fatalf("an allowed redirect was refused: %v", err)
	}
	if err := check(req("https://elsewhere.net/a.mp3"), make([]*http.Request, 1)); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("a redirect left the allowlist: %v", err)
	}
	if err := check(req("ftp://cdn.example.com/a.mp3"), make([]*http.Request, 1)); err == nil {
		t.Fatal("a redirect to ftp was followed")
	}
	if err := check(req("https://cdn.example.com/a.mp3"), make([]*http.Request, maxRedirects)); err == nil {
		t.Fatal("redirects were not capped")
	}
}

func TestNotPublicErrorsNeverNameTheAddress(t *testing.T) {
	err := publicOnly("tcp", "10.0.3.4:443", nil)
	if !errors.Is(err, ErrNotPublic) || strings.Contains(err.Error(), "10.0.3.4") {
		t.Fatalf("%v", err)
	}
}
