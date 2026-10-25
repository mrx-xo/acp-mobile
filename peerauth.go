package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// Peer authentication: a hub calls a node's HTTP API over the tailnet
// without the phone's authkey cookie. The node admits the call when
// `tailscale whois` reports the same Tailscale login as the node itself.

const peerCacheTTL = 60 * time.Second

var tailnetPrefix = netip.MustParsePrefix("100.64.0.0/10")

// whoisLogin returns the Tailscale login name owning ip. A package var so
// tests can swap it.
var whoisLogin = func(ip string) (string, error) {
	out, err := command("tailscale", "whois", "--json", ip).Output()
	if err != nil {
		return "", err
	}
	var who struct {
		UserProfile struct {
			LoginName string `json:"LoginName"`
		} `json:"UserProfile"`
	}
	if err := json.Unmarshal(out, &who); err != nil {
		return "", err
	}
	return who.UserProfile.LoginName, nil
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
	cache     map[string]cacheEntry
	mu        sync.Mutex
}

func newPeerAuth(selfLogin, selfIP string) *peerAuth {
	return &peerAuth{selfLogin: selfLogin, selfIP: selfIP, cache: map[string]cacheEntry{}}
}

// allow reports whether remoteIP is a tailnet peer owned by the same login
// as this node. It fails closed on every error and caches both verdicts.
func (p *peerAuth) allow(remoteIP string) bool {
	if p == nil || p.selfLogin == "" || !isTailnetAddr(remoteIP) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if e, ok := p.cache[remoteIP]; ok && now.Before(e.expires) {
		return e.ok
	}
	login, err := whoisLogin(remoteIP)
	ok := err == nil && login != "" && login == p.selfLogin
	p.cache[remoteIP] = cacheEntry{ok: ok, expires: now.Add(peerCacheTTL)}
	return ok
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
