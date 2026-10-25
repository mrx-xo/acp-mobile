package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Peer authentication: a hub calls a node's HTTP API over the tailnet
// without the phone's authkey cookie. The node admits the call when
// `tailscale whois` reports the same Tailscale login as the node itself
// AND the caller's Tailscale node name is listed in config `hubs`: the
// same login also owns non-fleet devices on this tailnet.

const (
	peerCacheTTL = 60 * time.Second
	peerErrorTTL = 5 * time.Second
	selfRetryTTL = 30 * time.Second
	whoisTimeout = 5 * time.Second

	// peerHeader marks a hub-to-node call. Browsers cannot set custom
	// headers on a WebSocket upgrade, and a custom header on a cross-origin
	// fetch forces a CORS preflight this server never answers, so only a
	// non-browser client can send it.
	peerHeader = "X-Acp-Peer"
)

var tailnetPrefix = netip.MustParsePrefix("100.64.0.0/10")

// whoisPeer returns the Tailscale login name owning ip and the node's
// name (its MagicDNS name, trailing dot stripped). A package var so tests
// can swap it.
var whoisPeer = func(ip string) (login, nodeName string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), whoisTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tailscale", "whois", "--json", ip)
	configureCommandEnv(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	var who struct {
		Node struct {
			Name string `json:"Name"`
		} `json:"Node"`
		UserProfile struct {
			LoginName string `json:"LoginName"`
		} `json:"UserProfile"`
	}
	if err := json.Unmarshal(out, &who); err != nil {
		return "", "", err
	}
	return who.UserProfile.LoginName, strings.TrimSuffix(who.Node.Name, "."), nil
}

// peerNow is the clock for cache expiry. A var so tests can move it.
var peerNow = time.Now

// normNode folds a node name for comparison: no trailing dot, lower case.
func normNode(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// isTailnetAddr reports whether ip is an IPv4 address in the Tailscale
// CGNAT range 100.64.0.0/10.
func isTailnetAddr(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() {
		return false
	}
	return tailnetPrefix.Contains(a)
}

type cacheEntry struct {
	ok      bool
	expires time.Time
}

type peerAuth struct {
	selfLogin string
	selfIP    string
	hubs      map[string]bool
	cache     map[string]cacheEntry
	// selfRetry is the earliest time a lazy self-login whois may run again.
	selfRetry time.Time
	mu        sync.Mutex
}

func newPeerAuth(selfLogin, selfIP string, hubs []string) *peerAuth {
	set := map[string]bool{}
	for _, h := range hubs {
		if n := normNode(h); n != "" {
			set[n] = true
		}
	}
	return &peerAuth{selfLogin: selfLogin, selfIP: selfIP, hubs: set, cache: map[string]cacheEntry{}}
}

// login returns this node's Tailscale login, resolving it lazily when
// startup could not (tailscaled not up yet).  At most one attempt per
// selfRetryTTL.
func (p *peerAuth) login() string {
	p.mu.Lock()
	if p.selfLogin != "" || p.selfIP == "" || peerNow().Before(p.selfRetry) {
		l := p.selfLogin
		p.mu.Unlock()
		return l
	}
	p.selfRetry = peerNow().Add(selfRetryTTL)
	p.mu.Unlock()
	login, _, err := whoisPeer(p.selfIP)
	if err != nil || login == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.selfLogin == "" {
		p.selfLogin = login
		log.Printf("peer auth: resolved self login %s", login)
	}
	return p.selfLogin
}

// allow reports whether remoteIP is a tailnet peer owned by the same login
// as this node and named in hubs. It fails closed on every error and
// caches both verdicts: an error for peerErrorTTL, a verdict for
// peerCacheTTL.
func (p *peerAuth) allow(remoteIP string) bool {
	if p == nil || len(p.hubs) == 0 || !isTailnetAddr(remoteIP) || remoteIP == p.selfIP {
		return false
	}
	self := p.login()
	if self == "" {
		return false
	}
	p.mu.Lock()
	e, ok := p.cache[remoteIP]
	p.mu.Unlock()
	if ok && peerNow().Before(e.expires) {
		return e.ok
	}
	// Run whois outside the mutex so a slow lookup never blocks other peers.
	login, node, err := whoisPeer(remoteIP)
	allowed := err == nil && login != "" && login == self && p.hubs[normNode(node)]
	ttl := peerCacheTTL
	if err != nil {
		ttl = peerErrorTTL
	}
	p.mu.Lock()
	p.cache[remoteIP] = cacheEntry{ok: allowed, expires: peerNow().Add(ttl)}
	p.mu.Unlock()
	return allowed
}

// peerEligible reports whether r carries the peer marker and no browser
// fetch metadata, i.e. it can only have come from a non-browser client.
func peerEligible(r *http.Request) bool {
	return r.Header.Get(peerHeader) == "1" && r.Header.Get("Sec-Fetch-Site") == ""
}

type ctxKey int

const ctxPeer ctxKey = 1

func withPeerFlag(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxPeer, true))
}

// isPeerRequest reports whether the middleware admitted r as a tailnet peer.
func isPeerRequest(r *http.Request) bool {
	v, _ := r.Context().Value(ctxPeer).(bool)
	return v
}
