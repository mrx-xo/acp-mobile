package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// machineState is one machine's reachability as the hub last saw it.
type machineState struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"` // "awake" or "asleep"
	LastSeen int64  `json:"lastSeen"`
}

// peerPort is the port every node serves on.  A var so tests can point
// the hub at an httptest port.
var peerPort = "8090"

// doError marks a failure of the HTTP round trip itself (dial, timeout),
// as opposed to a peer that answered badly.
type doError struct{ error }

func (e doError) Unwrap() error { return e.error }

var peerHTTP = &http.Client{Timeout: 2 * time.Second}

// tailscaleStatusJSON returns `tailscale status --json`.  A var for tests.
var tailscaleStatusJSON = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return commandContext(ctx, "tailscale", "status", "--json").Output()
}

// refreshPeerIP re-resolves a peer's tailnet IPv4 from Tailscale and
// updates the registry in place.  A peer matches when its DNSName equals
// the registry Host (several devices can share a HostName); HostName == id
// is only a fallback for entries without a Host.  Online peers win, and
// candidates are visited in sorted key order so the pick is deterministic.
func refreshPeerIP(id string) {
	out, err := tailscaleStatusJSON()
	if err != nil {
		return
	}
	var status struct {
		Peer map[string]struct {
			HostName     string
			DNSName      string
			Online       bool
			TailscaleIPs []string
		}
	}
	if json.Unmarshal(out, &status) != nil {
		return
	}
	fleet.mu.RLock()
	reg, _ := fleet.reg.byID(id)
	fleet.mu.RUnlock()
	host := strings.TrimSuffix(reg.Host, ".")

	keys := make([]string, 0, len(status.Peer))
	for k := range status.Peer {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var candidates []string
	for _, k := range keys {
		p := status.Peer[k]
		if host != "" {
			if !strings.EqualFold(strings.TrimSuffix(p.DNSName, "."), host) {
				continue
			}
		} else if !strings.EqualFold(p.HostName, id) {
			continue
		}
		candidates = append(candidates, k)
	}
	// Stable sort keeps key order within the online and offline groups.
	sort.SliceStable(candidates, func(i, j int) bool {
		return status.Peer[candidates[i]].Online && !status.Peer[candidates[j]].Online
	})
	for _, k := range candidates {
		for _, s := range status.Peer[k].TailscaleIPs {
			if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
				fleet.mu.Lock()
				for i := range fleet.reg.Machines {
					if fleet.reg.Machines[i].ID == id {
						fleet.reg.Machines[i].IP = s
					}
				}
				fleet.mu.Unlock()
				return
			}
		}
	}
}

// peerHostPort is the host:port of a peer's hub (addr overrides it in tests).
func peerHostPort(m machine) string {
	if m.addr != "" {
		return m.addr
	}
	return net.JoinHostPort(m.IP, peerPort)
}

// fetchPeerOnce makes one POST to the peer's /api/sessions.
func fetchPeerOnce(ctx context.Context, m machine) ([]sessionInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+peerHostPort(m)+"/api/sessions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(peerHeader, "1")
	resp, err := peerHTTP.Do(req)
	if err != nil {
		return nil, doError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s: status %d", m.ID, resp.StatusCode)
	}
	var body struct {
		Sessions []sessionInfo `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	for i := range body.Sessions {
		body.Sessions[i].Machine = m.ID
	}
	return body.Sessions, nil
}

// fetchPeerSessions fetches a peer's local sessions.  Any round-trip error,
// timeouts included (a peer whose tailnet IP changed usually just drops
// packets), refreshes the IP once and retries if it changed.  A peer that
// answered (non-200, bad body) has the right IP, so no refresh.
func fetchPeerSessions(ctx context.Context, m machine) ([]sessionInfo, error) {
	sessions, err := fetchPeerOnce(ctx, m)
	if err == nil || m.addr != "" {
		return sessions, err
	}
	var de doError
	if !errors.As(err, &de) {
		return nil, err
	}
	refreshPeerIP(m.ID)
	fleet.mu.RLock()
	fresh, ok := fleet.reg.byID(m.ID)
	fleet.mu.RUnlock()
	if !ok || fresh.IP == m.IP {
		return nil, err
	}
	return fetchPeerOnce(ctx, fresh)
}

// mergedSessions returns this machine's sessions followed by every peer's,
// in registry order, plus per-machine state.  Index 0 of the states is the
// hub itself.
func mergedSessions(ctx context.Context) ([]sessionInfo, []machineState) {
	fleet.mu.RLock()
	self := fleet.self
	peers := fleet.reg.peers(self)
	selfName := self
	if m, ok := fleet.reg.byID(self); ok && m.Name != "" {
		selfName = m.Name
	}
	fleet.mu.RUnlock()

	var local []sessionInfo
	remote := make([][]sessionInfo, len(peers))
	errs := make([]error, len(peers))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		local = localSessions(ctx)
	}()
	for i, m := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote[i], errs[i] = fetchPeerSessions(ctx, m)
		}()
	}
	wg.Wait()

	now := time.Now().Unix()
	sessions := local
	states := []machineState{{ID: self, Name: selfName, State: "awake", LastSeen: now}}
	fleet.mu.Lock()
	if fleet.lastSeen == nil {
		fleet.lastSeen = map[string]int64{}
	}
	for i, m := range peers {
		st := machineState{ID: m.ID, Name: m.Name, State: "asleep"}
		if errs[i] == nil {
			fleet.lastSeen[m.ID] = now
			st.State = "awake"
			sessions = append(sessions, remote[i]...)
		}
		st.LastSeen = fleet.lastSeen[m.ID]
		states = append(states, st)
	}
	fleet.mu.Unlock()
	return sessions, states
}
