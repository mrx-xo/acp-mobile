package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// testHubs lists the hub every stub whois answers with.
var testHubs = []string{"mrx.example.ts.net"}

// stubWhois swaps whoisPeer and the peer clock for one test.
func stubWhois(t *testing.T, fn func(ip string) (string, string, error)) {
	t.Helper()
	oldWhois, oldNow := whoisPeer, peerNow
	whoisPeer = fn
	t.Cleanup(func() { whoisPeer, peerNow = oldWhois, oldNow })
}

func TestPeerAuthAllowsSameLogin(t *testing.T) {
	calls := 0
	whoisPeer = func(ip string) (string, string, error) { calls++; return "me@example.com", "mrx.example.ts.net.", nil }
	p := newPeerAuth("me@example.com", "100.100.0.1", testHubs)
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
	whoisPeer = func(ip string) (string, string, error) { return "stranger@example.com", "mrx.example.ts.net.", nil }
	p := newPeerAuth("me@example.com", "100.100.0.1", testHubs)
	if p.allow("100.100.0.9") {
		t.Fatal("foreign login must be rejected")
	}
}

func TestPeerAuthSkipsNonTailnetAddresses(t *testing.T) {
	whoisPeer = func(ip string) (string, string, error) {
		t.Fatal("whois must not run for non-tailnet")
		return "", "", nil
	}
	p := newPeerAuth("me@example.com", "100.100.0.1", testHubs)
	for _, ip := range []string{"127.0.0.1", "192.168.1.20", "::1", "garbage", ""} {
		if p.allow(ip) {
			t.Fatalf("%s must not be allowed", ip)
		}
	}
}

func TestPeerAuthEmptySelfLoginNeverAllows(t *testing.T) {
	whoisPeer = func(ip string) (string, string, error) { return "", "", nil }
	p := newPeerAuth("", "", testHubs)
	if p.allow("100.100.0.3") {
		t.Fatal("no login known must fail closed")
	}
}

func TestPeerAuthWhoisErrorFailsClosed(t *testing.T) {
	whoisPeer = func(ip string) (string, string, error) { return "", "", fmt.Errorf("tailscale absent") }
	p := newPeerAuth("me@example.com", "100.100.0.1", testHubs)
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
	whoisPeer = func(ip string) (string, string, error) { return "me@example.com", "mrx.example.ts.net.", nil }
	h := buildAuthHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, isPeerRequest(r))
	}), "secret", newRateLimiter(), map[string]bool{"100.100.0.1": true, "127.0.0.1": true}, newPeerAuth("me@example.com", "100.100.0.1", testHubs))
	req := httptest.NewRequest("POST", "http://100.100.0.1:8090/api/sessions", nil)
	req.Header.Set(peerHeader, "1")
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

func peerTestHandler(t *testing.T, login string) http.Handler {
	t.Helper()
	return buildAuthHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, isPeerRequest(r))
	}), "secret", newRateLimiter(), map[string]bool{"100.100.0.1": true, "127.0.0.1": true}, newPeerAuth(login, "100.100.0.1", testHubs))
}

func peerReq(hdr map[string]string) *http.Request {
	req := httptest.NewRequest("POST", "http://100.100.0.1:8090/api/sessions", nil)
	req.RemoteAddr = "100.100.0.3:5555"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

func TestAuthMiddlewarePeerRequiresHeader(t *testing.T) {
	whoisPeer = func(ip string) (string, string, error) {
		t.Fatal("whois must not run without the peer header")
		return "", "", nil
	}
	rec := httptest.NewRecorder()
	peerTestHandler(t, "me@example.com").ServeHTTP(rec, peerReq(nil))
	if rec.Code != 401 {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

func TestAuthMiddlewarePeerRejectsBrowserFetchMetadata(t *testing.T) {
	whoisPeer = func(ip string) (string, string, error) {
		t.Fatal("whois must not run for browser requests")
		return "", "", nil
	}
	rec := httptest.NewRecorder()
	peerTestHandler(t, "me@example.com").ServeHTTP(rec, peerReq(map[string]string{peerHeader: "1", "Sec-Fetch-Site": "cross-site"}))
	if rec.Code != 401 {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

func TestAuthMiddlewareForeignLoginGets403(t *testing.T) {
	whoisPeer = func(ip string) (string, string, error) { return "stranger@example.com", "mrx.example.ts.net.", nil }
	rec := httptest.NewRecorder()
	peerTestHandler(t, "me@example.com").ServeHTTP(rec, peerReq(map[string]string{peerHeader: "1"}))
	if rec.Code != 403 {
		t.Fatalf("code=%d, want 403", rec.Code)
	}
}

func TestPeerAuthCachesErrorAsDeny(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	stubWhois(t, func(ip string) (string, string, error) { calls++; return "", "", fmt.Errorf("timeout") })
	peerNow = func() time.Time { return now }
	p := newPeerAuth("me@example.com", "100.100.0.1", testHubs)
	if p.allow("100.100.0.3") || p.allow("100.100.0.3") {
		t.Fatal("error must deny")
	}
	now = now.Add(4 * time.Second)
	p.allow("100.100.0.3")
	if calls != 1 {
		t.Fatalf("whois calls = %d, want 1 (error cached within 5s)", calls)
	}
	now = now.Add(2 * time.Second)
	p.allow("100.100.0.3")
	if calls != 2 {
		t.Fatalf("whois calls = %d, want 2 (error expires after 5s)", calls)
	}
}

func TestPeerAuthCachesVerdictForAMinute(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	stubWhois(t, func(ip string) (string, string, error) { calls++; return "me@example.com", "mrx.example.ts.net.", nil })
	peerNow = func() time.Time { return now }
	p := newPeerAuth("me@example.com", "100.100.0.1", testHubs)
	p.allow("100.100.0.3")
	now = now.Add(59 * time.Second)
	p.allow("100.100.0.3")
	if calls != 1 {
		t.Fatalf("whois calls = %d, want 1 within 60s", calls)
	}
	now = now.Add(2 * time.Second)
	p.allow("100.100.0.3")
	if calls != 2 {
		t.Fatalf("whois calls = %d, want 2 after 60s", calls)
	}
}

func TestPeerAuthRequiresListedHub(t *testing.T) {
	stubWhois(t, func(ip string) (string, string, error) { return "me@example.com", "kids-ipad.example.ts.net.", nil })
	p := newPeerAuth("me@example.com", "100.100.0.1", testHubs)
	if p.allow("100.100.0.3") {
		t.Fatal("same login on an unlisted node must be rejected")
	}
}

func TestPeerAuthHubMatchIgnoresCaseAndTrailingDot(t *testing.T) {
	stubWhois(t, func(ip string) (string, string, error) { return "me@example.com", "MrX.Example.TS.net.", nil })
	p := newPeerAuth("me@example.com", "100.100.0.1", []string{"mrx.example.ts.net."})
	if !p.allow("100.100.0.3") {
		t.Fatal("hub match must ignore case and trailing dots")
	}
}

func TestPeerAuthEmptyHubsNeverAllows(t *testing.T) {
	stubWhois(t, func(ip string) (string, string, error) { return "me@example.com", "mrx.example.ts.net.", nil })
	for _, hubs := range [][]string{nil, {}} {
		if newPeerAuth("me@example.com", "100.100.0.1", hubs).allow("100.100.0.3") {
			t.Fatalf("hubs=%v must fail closed", hubs)
		}
	}
}

func TestPeerAuthRejectsSelfIP(t *testing.T) {
	stubWhois(t, func(ip string) (string, string, error) { return "me@example.com", "mrx.example.ts.net.", nil })
	p := newPeerAuth("me@example.com", "100.100.0.1", testHubs)
	if p.allow("100.100.0.1") {
		t.Fatal("this node's own IP must be rejected")
	}
}

func TestPeerAuthResolvesSelfLoginLazily(t *testing.T) {
	selfCalls := 0
	ready := false
	now := time.Unix(1000, 0)
	stubWhois(t, func(ip string) (string, string, error) {
		if ip == "100.100.0.1" {
			selfCalls++
			if !ready {
				return "", "", fmt.Errorf("tailscaled not up")
			}
			return "me@example.com", "node.example.ts.net.", nil
		}
		return "me@example.com", "mrx.example.ts.net.", nil
	})
	peerNow = func() time.Time { return now }
	p := newPeerAuth("", "100.100.0.1", testHubs)
	if p.allow("100.100.0.3") {
		t.Fatal("unknown self login must deny")
	}
	ready = true
	now = now.Add(10 * time.Second)
	if p.allow("100.100.0.3") {
		t.Fatal("self whois must not retry within 30s")
	}
	if selfCalls != 1 {
		t.Fatalf("self whois calls = %d, want 1", selfCalls)
	}
	now = now.Add(25 * time.Second)
	if !p.allow("100.100.0.3") {
		t.Fatal("allow must succeed once self whois answers")
	}
	if !p.allow("100.100.0.4") || selfCalls != 2 {
		t.Fatalf("self login must stick; self calls = %d", selfCalls)
	}
}

func TestWSHandshakePeerFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Peer") == "1" {
			r = withPeerFlag(r)
		}
		websocket.Server{
			Handshake: wsHandshake,
			Handler:   func(ws *websocket.Conn) { ws.Close() },
		}.ServeHTTP(w, r)
	}))
	defer srv.Close()
	dial := func(peer bool) error {
		cfg, err := websocket.NewConfig("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", "http://evil.example")
		if err != nil {
			t.Fatal(err)
		}
		if peer {
			cfg.Header = http.Header{"X-Test-Peer": {"1"}}
		}
		ws, err := cfg.DialContext(context.Background())
		if err == nil {
			ws.Close()
		}
		return err
	}
	if dial(false) == nil {
		t.Fatal("foreign Origin without peer flag must be rejected")
	}
	if err := dial(true); err != nil {
		t.Fatalf("peer-flagged upgrade must be accepted: %v", err)
	}
}
