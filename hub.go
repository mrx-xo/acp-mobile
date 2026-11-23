package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	OS       string `json:"os,omitempty"`
	// Gaps are the features the machine declares it lacks; always an array.
	Gaps []string `json:"gaps"`
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
	return tailscaleOutput(ctx, "status", "--json")
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

// gapsOf is m's declared gaps, never nil so the JSON is always an array.
func gapsOf(m machine) []string {
	if m.Gaps == nil {
		return []string{}
	}
	return m.Gaps
}

// fanOutJSON POSTs body (may be nil) to path on every peer in parallel with
// the peer header, each call bounded by budget.  skip, when non-nil, leaves
// a machine out.  It does not refresh a peer's tailnet IP; the sessions poll
// owns that.  The result maps machine id to the raw reply body; a peer
// that errors, times out or answers non-200 is absent.
func fanOutJSON(ctx context.Context, path string, body []byte, budget time.Duration, skip func(machine) bool) map[string][]byte {
	fleet.mu.RLock()
	peers := fleet.reg.peers(fleet.self)
	fleet.mu.RUnlock()
	replies := make([][]byte, len(peers))
	var wg sync.WaitGroup
	for i, m := range peers {
		if skip != nil && skip(m) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			var rd io.Reader
			if body != nil {
				rd = bytes.NewReader(body)
			}
			b, err := fanOutOne(cctx, m, path, rd, body != nil)
			if err != nil {
				log.Printf("fleet: %s on %s: %v", path, m.ID, err)
				return
			}
			replies[i] = b
		}()
	}
	wg.Wait()
	out := map[string][]byte{}
	for i, m := range peers {
		if replies[i] != nil {
			out[m.ID] = replies[i]
		}
	}
	return out
}

// fanOutOne is one peer's call for fanOutJSON.
func fanOutOne(ctx context.Context, m machine, path string, body io.Reader, hasBody bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+peerHostPort(m)+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(peerHeader, "1")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
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
	fleet.mu.RLock()
	selfM, _ := fleet.reg.byID(self)
	fleet.mu.RUnlock()
	states := []machineState{{ID: self, Name: selfName, State: "awake", LastSeen: now, OS: selfM.OS, Gaps: gapsOf(selfM)}}
	fleet.mu.Lock()
	if fleet.lastSeen == nil {
		fleet.lastSeen = map[string]int64{}
	}
	for i, m := range peers {
		st := machineState{ID: m.ID, Name: m.Name, State: "asleep", OS: m.OS, Gaps: gapsOf(m)}
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

// remoteActionPeekLimit bounds how much of a JSON body the remote-machine
// guard reads; every guarded route's body is far smaller.
const remoteActionPeekLimit = 1 << 20

// remoteTarget returns the machine r names (query or JSON body field
// "machine") when it is not this one, else "".  The body is restored for
// the handler.  tooLarge is true when the machine could only be named in
// a body longer than remoteActionPeekLimit: the owner is then unknown and
// the caller must refuse, not run the request locally.
func remoteTarget(r *http.Request) (target string, tooLarge bool) {
	target = r.URL.Query().Get("machine")
	if target == "" && r.Body != nil && r.Body != http.NoBody {
		peek, err := io.ReadAll(io.LimitReader(r.Body, remoteActionPeekLimit+1))
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), r.Body), r.Body}
		if err == nil && len(peek) > remoteActionPeekLimit {
			return "", true
		}
		if err == nil {
			var body struct {
				Machine string `json:"machine"`
			}
			if json.Unmarshal(peek, &body) == nil {
				target = body.Machine
			}
		}
	}
	if target == "" {
		return "", false
	}
	fleet.mu.RLock()
	self := fleet.self
	fleet.mu.RUnlock()
	if target == self {
		return "", false
	}
	return target, false
}

// forwardTimeout is how long the hub waits for a peer to answer a
// forwarded action on path.  The node runs emacsclient or a spawn script
// before it answers, so these wait longer than the session poll does.
func forwardTimeout(path string) time.Duration {
	switch path {
	case "/api/spawn":
		return 180 * time.Second
	case "/api/resume-transcript":
		return 45 * time.Second
	case "/api/fork":
		return 60 * time.Second
	}
	return 15 * time.Second
}

// peerActionHTTP carries a forwarded action.  It has no client timeout:
// forwardToMachine bounds each call with a per-route context deadline.
var peerActionHTTP = &http.Client{}

// fleetRoute wraps a machine-scoped handler.  A request that names no
// machine, or this one, runs h unchanged.  A request that names another
// machine is handed to that machine (forwardToMachine), because this
// machine's Emacs and sockets do not own its chats.  A call that already
// came from a peer is never forwarded again, so two hubs cannot bounce an
// action between each other.
func fleetRoute(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target, tooLarge := remoteTarget(r)
		if tooLarge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			json.NewEncoder(w).Encode(map[string]string{"error": "request body too large"})
			return
		}
		if target == "" {
			h(w, r)
			return
		}
		if isPeerRequest(r) {
			http.Error(w, "a forwarded action is not forwarded again", http.StatusNotImplemented)
			return
		}
		forwardToMachine(w, r, target)
	}
}

// forwardToMachine replays r's method, path and body on the machine with
// id target, as a peer call, and relays that machine's status and body.
// The node sees its own id in ?machine= and runs the action locally.  A
// failure of the forward itself is answered as a JSON error object,
// because the phone parses every action reply as JSON.
func forwardToMachine(w http.ResponseWriter, r *http.Request, target string) {
	jsonError := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	fleet.mu.RLock()
	m, ok := fleet.reg.byID(target)
	fleet.mu.RUnlock()
	if !ok {
		jsonError(http.StatusNotFound, "unknown machine "+target)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, remoteActionPeekLimit+1))
	if err != nil {
		jsonError(http.StatusBadRequest, "unreadable request body")
		return
	}
	if len(body) > remoteActionPeekLimit {
		jsonError(http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	// Keep the caller's query (a GET probe names its chat there) and make
	// sure the machine rides in it, wherever the caller put it.
	query := r.URL.Query()
	query.Set("machine", target)
	u := "http://" + peerHostPort(m) + r.URL.Path + "?" + query.Encode()
	ctx, cancel := context.WithTimeout(r.Context(), forwardTimeout(r.URL.Path))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, u, bytes.NewReader(body))
	if err != nil {
		jsonError(http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set(peerHeader, "1")
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := peerActionHTTP.Do(req)
	if err != nil {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		log.Printf("forward %s to %s: %v", r.URL.Path, m.ID, err)
		jsonError(http.StatusBadGateway, name+" did not answer")
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// The status is already sent; drop the connection so the client
		// sees a failed transfer, not a clean short body.
		log.Printf("forward %s to %s: relay: %v", r.URL.Path, m.ID, err)
		panic(http.ErrAbortHandler)
	}
}

// The phone polls /api/statuses every 3s, so a peer gets one second to
// answer and a peer that failed is left alone for a while: an asleep
// machine must not add its timeout to every poll.
const (
	peerStatusTimeout = time.Second
	peerStatusBackoff = 10 * time.Second
)

var peerStatusRetry = struct {
	mu    sync.Mutex
	after map[string]time.Time
}{after: map[string]time.Time{}}

// resetPeerStatusBackoff forgets which peers recently failed a status poll.
func resetPeerStatusBackoff() {
	peerStatusRetry.mu.Lock()
	peerStatusRetry.after = map[string]time.Time{}
	peerStatusRetry.mu.Unlock()
}

// fetchPeerStatuses makes one POST to the peer's /api/statuses.
func fetchPeerStatuses(ctx context.Context, m machine) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, peerStatusTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+peerHostPort(m)+"/api/statuses", nil)
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
		Statuses map[string]string `json:"statuses"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Statuses, nil
}

// peerStatuses returns every reachable peer's session statuses in one map.
// The phone replaces each card's status with what /api/statuses returns, so
// a hub that answered with its own chats only blanked every remote card.
// A peer that fails is skipped (its cards go blank, like the machine) and
// not asked again until the backoff passes; the sessions poll owns IP
// refresh and the asleep state.
func peerStatuses(ctx context.Context) map[string]string {
	fleet.mu.RLock()
	peers := fleet.reg.peers(fleet.self)
	fleet.mu.RUnlock()

	now := time.Now()
	results := make([]map[string]string, len(peers))
	var wg sync.WaitGroup
	for i, m := range peers {
		peerStatusRetry.mu.Lock()
		wait := now.Before(peerStatusRetry.after[m.ID])
		peerStatusRetry.mu.Unlock()
		if wait {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses, err := fetchPeerStatuses(ctx, m)
			if err != nil {
				peerStatusRetry.mu.Lock()
				peerStatusRetry.after[m.ID] = time.Now().Add(peerStatusBackoff)
				peerStatusRetry.mu.Unlock()
				return
			}
			results[i] = statuses
		}()
	}
	wg.Wait()

	merged := map[string]string{}
	for _, statuses := range results {
		for id, status := range statuses {
			merged[id] = status
		}
	}
	return merged
}

// wantsFleet reports whether r asks for the merged, every-machine answer:
// only a request that sends fleet=1, names no machine, is not itself a peer's
// call, and reaches a machine that has peers.  Everything else is local.
func wantsFleet(r *http.Request) bool {
	q := r.URL.Query()
	if q.Get("fleet") != "1" || q.Get("machine") != "" || isPeerRequest(r) {
		return false
	}
	fleet.mu.RLock()
	defer fleet.mu.RUnlock()
	return len(fleet.reg.peers(fleet.self)) > 0
}

// hasHistoryGap reports whether m cannot answer history requests.
func hasHistoryGap(m machine) bool {
	for _, g := range m.Gaps {
		if g == "history" {
			return true
		}
	}
	return false
}

// thisMachineID is this machine's id in the registry.
func thisMachineID() string {
	fleet.mu.RLock()
	defer fleet.mu.RUnlock()
	return fleet.self
}

// fanOutPath is r's path with its query minus the routing parameters (fleet,
// machine), so a peer answers locally and still sees every other parameter.
func fanOutPath(r *http.Request) string {
	q := r.URL.Query()
	q.Del("fleet")
	q.Del("machine")
	if len(q) == 0 {
		return r.URL.Path
	}
	return r.URL.Path + "?" + q.Encode()
}
