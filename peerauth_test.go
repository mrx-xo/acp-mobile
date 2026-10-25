package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPeerAuthAllowsSameLogin(t *testing.T) {
	calls := 0
	whoisLogin = func(ip string) (string, error) { calls++; return "me@example.com", nil }
	p := newPeerAuth("me@example.com", "100.100.0.1")
	if !p.allow("100.100.0.3") {
		t.Fatal("same login must be allowed")
	}
	if !p.allow("100.100.0.3") {
		t.Fatal("cached verdict must hold")
	}
	if calls != 1 {
		t.Fatalf("whois calls = %d, want 1 (cached)", calls)
	}
}

func TestPeerAuthRejectsForeignLogin(t *testing.T) {
	whoisLogin = func(ip string) (string, error) { return "stranger@example.com", nil }
	p := newPeerAuth("me@example.com", "100.100.0.1")
	if p.allow("100.100.0.9") {
		t.Fatal("foreign login must be rejected")
	}
}

func TestPeerAuthSkipsNonTailnetAddresses(t *testing.T) {
	whoisLogin = func(ip string) (string, error) { t.Fatal("whois must not run for non-tailnet"); return "", nil }
	p := newPeerAuth("me@example.com", "100.100.0.1")
	for _, ip := range []string{"127.0.0.1", "192.168.1.20", "::1", "garbage", ""} {
		if p.allow(ip) {
			t.Fatalf("%s must not be allowed", ip)
		}
	}
}

func TestPeerAuthEmptySelfLoginNeverAllows(t *testing.T) {
	whoisLogin = func(ip string) (string, error) { return "", nil }
	p := newPeerAuth("", "")
	if p.allow("100.100.0.3") {
		t.Fatal("no login known must fail closed")
	}
}

func TestPeerAuthWhoisErrorFailsClosed(t *testing.T) {
	whoisLogin = func(ip string) (string, error) { return "", fmt.Errorf("tailscale absent") }
	p := newPeerAuth("me@example.com", "100.100.0.1")
	if p.allow("100.100.0.3") {
		t.Fatal("whois error must fail closed")
	}
}

func TestIsTailnetAddr(t *testing.T) {
	for ip, want := range map[string]bool{
		"100.64.0.0": true, "100.100.0.3": true, "100.127.255.255": true,
		"100.63.255.255": false, "100.128.0.0": false, "10.0.0.1": false,
		"fd7a:115c:a1e0::1": false, "x": false,
	} {
		if got := isTailnetAddr(ip); got != want {
			t.Errorf("isTailnetAddr(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestAuthMiddlewarePeerBypassesCookie(t *testing.T) {
	whoisLogin = func(ip string) (string, error) { return "me@example.com", nil }
	h := buildAuthHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, isPeerRequest(r))
	}), "secret", newRateLimiter(), map[string]bool{"100.100.0.1": true, "127.0.0.1": true}, newPeerAuth("me@example.com", "100.100.0.1"))
	req := httptest.NewRequest("POST", "http://100.100.0.1:8090/api/sessions", nil)
	req.RemoteAddr = "100.100.0.3:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "true" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}

	req.RemoteAddr = "127.0.0.1:5555"
	req.Host = "127.0.0.1:8090"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("loopback without cookie: code=%d, want 401", rec.Code)
	}
}
