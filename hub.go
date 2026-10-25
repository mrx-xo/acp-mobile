package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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

var peerHTTP = &http.Client{Timeout: 2 * time.Second}

// tailscaleStatusJSON returns `tailscale status --json`.  A var for tests.
var tailscaleStatusJSON = func() ([]byte, error) {
	return command("tailscale", "status", "--json").Output()
}

// refreshPeerIP re-resolves a peer's tailnet IPv4 from Tailscale and
// updates the registry in place.
func refreshPeerIP(id string) {
	out, err := tailscaleStatusJSON()
	if err != nil {
		return
	}
	var status struct {
		Peer map[string]struct {
			HostName     string
			DNSName      string
			TailscaleIPs []string
		}
	}
	if json.Unmarshal(out, &status) != nil {
		return
	}
	for _, p := range status.Peer {
		label, _, _ := strings.Cut(p.DNSName, ".")
		if !strings.EqualFold(p.HostName, id) && !strings.EqualFold(label, id) {
			continue
		}
		for _, s := range p.TailscaleIPs {
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

// fetchPeerOnce makes one POST to the peer's /api/sessions.
func fetchPeerOnce(ctx context.Context, m machine) ([]sessionInfo, error) {
	addr := m.addr
	if addr == "" {
		addr = m.IP + ":8090"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/api/sessions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(peerHeader, "1")
	resp, err := peerHTTP.Do(req)
	if err != nil {
		return nil, err
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

// fetchPeerSessions fetches a peer's local sessions.  A dial error (the
// peer's tailnet IP may have changed) refreshes the IP once and retries.
func fetchPeerSessions(ctx context.Context, m machine) ([]sessionInfo, error) {
	sessions, err := fetchPeerOnce(ctx, m)
	if err == nil || m.addr != "" {
		return sessions, err
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
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
