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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFleetFixture installs a registry (self plus peers) and restores the
// previous fleet state on cleanup.
func withFleetFixture(t *testing.T, self string, peers ...machine) {
	t.Helper()
	fleet.mu.Lock()
	oldReg, oldSelf, oldSeen := fleet.reg, fleet.self, fleet.lastSeen
	fleet.reg = registry{Machines: append([]machine{{ID: self, Name: strings.ToUpper(self)}}, peers...)}
	fleet.self = self
	fleet.lastSeen = nil
	fleet.mu.Unlock()
	t.Cleanup(func() {
		fleet.mu.Lock()
		fleet.reg, fleet.self, fleet.lastSeen = oldReg, oldSelf, oldSeen
		fleet.mu.Unlock()
	})
}

// peerAt builds a machine whose hub address is the httptest server.
func peerAt(t *testing.T, id, name, serverURL string) machine {
	t.Helper()
	return machine{ID: id, Name: name, addr: strings.TrimPrefix(serverURL, "http://")}
}

func TestMergedSessionsTagsAndMergesPeers(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var gotHeader string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method %s", r.Method)
		}
		gotHeader = r.Header.Get(peerHeader)
		fmt.Fprint(w, `{"sessions":[{"pid":40388,"bufferName":"Claude Agent @ dotfiles","sessionId":"n1"}]}`)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	sessions, states := mergedSessions(context.Background())
	var remote []sessionInfo
	for _, s := range sessions {
		if s.Machine == "vengeance" {
			remote = append(remote, s)
		}
	}
	if len(remote) != 1 || remote[0].Pid != 40388 {
		t.Fatalf("remote = %+v", remote)
	}
	if gotHeader != "1" {
		t.Fatalf("peer header = %q", gotHeader)
	}
	if len(states) != 2 || states[0].ID != "mrx" || states[0].State != "awake" ||
		states[1].ID != "vengeance" || states[1].State != "awake" || states[1].LastSeen == 0 {
		t.Fatalf("states = %+v", states)
	}
}

func TestMergedSessionsReportsTimedOutPeerAsleep(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	release := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
	}))
	defer peer.Close()
	defer close(release)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	_, states := mergedSessions(context.Background())
	if states[1].State != "asleep" || states[1].LastSeen != 0 {
		t.Fatalf("want asleep, never seen, got %+v", states[1])
	}
}

func TestMergedSessionsKeepSameBufferNameAcrossMachines(t *testing.T) {
	// Unix socket paths are length-limited; t.TempDir() is too long on macOS.
	tmp, err := os.MkdirTemp("/tmp", "hub")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	sockDir := filepath.Join(tmp, "acp-multiplex")
	os.MkdirAll(sockDir, 0700)
	// The test process pid is alive, so discovery keeps its socket.
	ln, err := net.Listen("unix", filepath.Join(sockDir, fmt.Sprintf("%d.sock", os.Getpid())))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprint(conn, `{"method":"acp-multiplex/meta","params":{"name":"Claude Agent @ dotfiles"}}`+"\n")
			conn.Close()
		}
	}()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"sessions":[{"pid":7,"bufferName":"Claude Agent @ dotfiles"}]}`)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	sessions, _ := mergedSessions(context.Background())
	machines := map[string]bool{}
	for _, s := range sessions {
		if s.BufferName == "Claude Agent @ dotfiles" {
			machines[s.Machine] = true
		}
	}
	if len(machines) != 2 || !machines["mrx"] || !machines["vengeance"] {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestHandleSessionsServesLocalOnlyToPeers(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	called := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	req := withPeerFlag(httptest.NewRequest("POST", "/api/sessions", nil))
	rec := httptest.NewRecorder()
	handleSessions(rec, req)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["machines"]; ok {
		t.Fatalf("peer request got machines: %s", rec.Body.String())
	}
	if string(body["machine"]) != `"mrx"` {
		t.Fatalf("machine = %s", body["machine"])
	}
	if called {
		t.Fatal("hub fanned out on a peer request")
	}
}

func TestHandleSessionsNodeShapeAndHubShape(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	withFleetFixture(t, "mrx")
	rec := httptest.NewRecorder()
	handleSessions(rec, httptest.NewRequest("POST", "/api/sessions", nil))
	var body map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["machines"]; ok || string(body["machine"]) != `"mrx"` {
		t.Fatalf("node body = %s", rec.Body.String())
	}

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"sessions":[]}`)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec = httptest.NewRecorder()
	handleSessions(rec, httptest.NewRequest("POST", "/api/sessions", nil))
	body = nil
	json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["machines"]; !ok || string(body["machine"]) != `"mrx"` {
		t.Fatalf("hub body = %s", rec.Body.String())
	}
}

func TestRefreshPeerIPFromTailscaleStatus(t *testing.T) {
	old := tailscaleStatusJSON
	defer func() { tailscaleStatusJSON = old }()
	tailscaleStatusJSON = func() ([]byte, error) {
		return []byte(`{"Peer":{"k":{"HostName":"vengeance","DNSName":"Vengeance.example.ts.net.","Online":true,"TailscaleIPs":["fd7a::1","100.100.0.33"]}}}`), nil
	}
	withFleetFixture(t, "mrx", machine{ID: "vengeance", Host: "vengeance.example.ts.net", IP: "100.100.0.3"})
	refreshPeerIP("vengeance")
	m, _ := fleet.reg.byID("vengeance")
	if m.IP != "100.100.0.33" {
		t.Fatalf("ip = %s", m.IP)
	}
}

func TestRefreshPeerIPMatchesDNSNameNotHostName(t *testing.T) {
	old := tailscaleStatusJSON
	defer func() { tailscaleStatusJSON = old }()
	// Two devices report HostName "vengeance"; only one owns the
	// registry's DNS name.  Run several times: map order must not matter.
	tailscaleStatusJSON = func() ([]byte, error) {
		return []byte(`{"Peer":{
			"a":{"HostName":"vengeance","DNSName":"vengeance.example.ts.net.","Online":true,"TailscaleIPs":["100.100.0.10"]},
			"b":{"HostName":"vengeance","DNSName":"vengeance-1.example.ts.net.","Online":true,"TailscaleIPs":["100.100.0.11"]}}}`), nil
	}
	for i := 0; i < 20; i++ {
		withFleetFixture(t, "mrx", machine{ID: "vengeance", Host: "vengeance-1.example.ts.net", IP: "100.100.0.3"})
		refreshPeerIP("vengeance")
		if m, _ := fleet.reg.byID("vengeance"); m.IP != "100.100.0.11" {
			t.Fatalf("run %d: ip = %s, want the DNSName match", i, m.IP)
		}
	}
}

func TestRefreshPeerIPHostNameFallbackPrefersOnline(t *testing.T) {
	old := tailscaleStatusJSON
	defer func() { tailscaleStatusJSON = old }()
	tailscaleStatusJSON = func() ([]byte, error) {
		return []byte(`{"Peer":{
			"a":{"HostName":"vengeance","DNSName":"vengeance.example.ts.net.","Online":false,"TailscaleIPs":["100.100.0.10"]},
			"b":{"HostName":"vengeance","DNSName":"vengeance-1.example.ts.net.","Online":true,"TailscaleIPs":["100.100.0.11"]}}}`), nil
	}
	for i := 0; i < 20; i++ {
		withFleetFixture(t, "mrx", machine{ID: "vengeance", IP: "100.100.0.3"})
		refreshPeerIP("vengeance")
		if m, _ := fleet.reg.byID("vengeance"); m.IP != "100.100.0.11" {
			t.Fatalf("run %d: ip = %s, want the online peer", i, m.IP)
		}
	}
}

func TestRefreshPeerIPIgnoresHostNameWhenHostSet(t *testing.T) {
	old := tailscaleStatusJSON
	defer func() { tailscaleStatusJSON = old }()
	tailscaleStatusJSON = func() ([]byte, error) {
		return []byte(`{"Peer":{"a":{"HostName":"vengeance","DNSName":"other.example.ts.net.","Online":true,"TailscaleIPs":["100.100.0.10"]}}}`), nil
	}
	withFleetFixture(t, "mrx", machine{ID: "vengeance", Host: "vengeance.example.ts.net", IP: "100.100.0.3"})
	refreshPeerIP("vengeance")
	if m, _ := fleet.reg.byID("vengeance"); m.IP != "100.100.0.3" {
		t.Fatalf("ip = %s, want unchanged", m.IP)
	}
}

func TestFetchPeerSessionsRefreshesIPAndRetries(t *testing.T) {
	hits := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, `{"sessions":[{"pid":5,"bufferName":"x"}]}`)
	}))
	defer peer.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(peer.URL, "http://"))
	oldPort, oldTS := peerPort, tailscaleStatusJSON
	peerPort = port
	t.Cleanup(func() { peerPort, tailscaleStatusJSON = oldPort, oldTS })
	tailscaleStatusJSON = func() ([]byte, error) {
		return []byte(`{"Peer":{"k":{"HostName":"vengeance","TailscaleIPs":["127.0.0.1"]}}}`), nil
	}
	// 0.0.0.1 is not routable; the first dial must fail before the retry.
	withFleetFixture(t, "mrx", machine{ID: "vengeance", IP: "0.0.0.1"})
	sessions, err := fetchPeerSessions(context.Background(), machine{ID: "vengeance", IP: "0.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Machine != "vengeance" || hits != 1 {
		t.Fatalf("sessions = %+v hits = %d", sessions, hits)
	}
	if m, _ := fleet.reg.byID("vengeance"); m.IP != "127.0.0.1" {
		t.Fatalf("ip = %s", m.IP)
	}
}

func TestFetchPeerSessionsNon200DoesNotRefresh(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer peer.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(peer.URL, "http://"))
	oldPort, oldTS := peerPort, tailscaleStatusJSON
	peerPort = port
	t.Cleanup(func() { peerPort, tailscaleStatusJSON = oldPort, oldTS })
	tailscaleStatusJSON = func() ([]byte, error) {
		t.Fatal("tailscale status called for a peer that answered")
		return nil, nil
	}
	withFleetFixture(t, "mrx", machine{ID: "vengeance", IP: "127.0.0.1"})
	if _, err := fetchPeerSessions(context.Background(), machine{ID: "vengeance", IP: "127.0.0.1"}); err == nil {
		t.Fatal("want error on 500")
	}
}

// Fleet step 3, kill only: the hub's Emacs does not own a peer's chat, so
// the hub hands /api/kill to the machine that does and relays its answer.
func TestKillForwardsToOwningMachine(t *testing.T) {
	argsFile := installFakeEmacsclient(t, "t")
	var gotMethod, gotPath, gotHeader, gotMachine, gotBody string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotHeader = r.Method, r.URL.Path, r.Header.Get(peerHeader)
		gotMachine = r.URL.Query().Get("machine")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	rec := httptest.NewRecorder()
	fleetRoute(handleKill)(rec, httptest.NewRequest("POST", "/api/kill",
		strings.NewReader(`{"bufferName":"Claude Agent @ skate","machine":"vengeance"}`)))

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Fatalf("hub answered %d %q, want the peer's 200", rec.Code, rec.Body.String())
	}
	if gotMethod != "POST" || gotPath != "/api/kill" || gotHeader != "1" || gotMachine != "vengeance" {
		t.Fatalf("peer saw %s %s header=%q machine=%q", gotMethod, gotPath, gotHeader, gotMachine)
	}
	var body struct{ BufferName string }
	if json.Unmarshal([]byte(gotBody), &body) != nil || body.BufferName != "Claude Agent @ skate" {
		t.Fatalf("peer body = %q, want the buffer name", gotBody)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("a remote kill still ran the hub's emacsclient")
	}
}

func TestKillRelaysThePeersRefusal(t *testing.T) {
	installFakeEmacsclient(t, "t")
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"session not found or already closed"}`)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	rec := httptest.NewRecorder()
	fleetRoute(handleKill)(rec, httptest.NewRequest("POST", "/api/kill",
		strings.NewReader(`{"bufferName":"Claude Agent @ skate","machine":"vengeance"}`)))

	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "session not found or already closed") {
		t.Fatalf("hub answered %d %q, want the peer's 404 and message", rec.Code, rec.Body.String())
	}
}

// The phone reads every kill reply as JSON, so an unreachable or unknown
// machine must still answer with an error object.
func TestKillOnUnreachableOrUnknownMachineAnswersJSON(t *testing.T) {
	installFakeEmacsclient(t, "t")
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", deadURL))
	for _, tc := range []struct {
		machine string
		code    int
		want    string
	}{
		{"vengeance", http.StatusBadGateway, "NEMESIS"},
		{"nowhere", http.StatusNotFound, "nowhere"},
	} {
		rec := httptest.NewRecorder()
		fleetRoute(handleKill)(rec, httptest.NewRequest("POST", "/api/kill",
			strings.NewReader(`{"bufferName":"x","machine":"`+tc.machine+`"}`)))
		var reply struct{ Error string }
		if rec.Code != tc.code || json.Unmarshal(rec.Body.Bytes(), &reply) != nil || !strings.Contains(reply.Error, tc.want) {
			t.Errorf("machine %s: %d %q, want %d and a JSON error naming %s", tc.machine, rec.Code, rec.Body.String(), tc.code, tc.want)
		}
	}
}

// Windows has no lsof or /proc, so a NEMESIS card has no cwd to name its
// project from; the buffer name carries it.
func TestProjectFromBufferName(t *testing.T) {
	for name, want := range map[string]string{
		"Claude Agent @ studio-bridge<2>": "studio-bridge",
		"Codex B Agent @ .dotfiles":       ".dotfiles",
		"Claude Agent":                    "",
		"":                                "",
	} {
		if got := projectFromBufferName(name); got != want {
			t.Errorf("projectFromBufferName(%q) = %q, want %q", name, got, want)
		}
	}
}

// A kill that already arrived as a hub-to-node call is never passed on:
// two hubs must not bounce an action between each other (spec section 8).
func TestKillFromAPeerIsNotForwardedAgain(t *testing.T) {
	installFakeEmacsclient(t, "t")
	hits := 0
	peer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	rec := httptest.NewRecorder()
	fleetRoute(handleKill)(rec, withPeerFlag(httptest.NewRequest("POST", "/api/kill",
		strings.NewReader(`{"bufferName":"x","machine":"vengeance"}`))))

	if hits != 0 || rec.Code != http.StatusNotImplemented {
		t.Fatalf("peer-origin kill: forwarded %d times, code %d; want 0 and 501", hits, rec.Code)
	}
}

// statusesReply runs handleStatuses for r and returns the statuses map.
func statusesReply(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	handleStatuses(rec, r)
	var body struct {
		Statuses map[string]string `json:"statuses"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body.Statuses
}

// withLocalStatuses points the sidecar at a temp home holding status.json
// and clears the peer-status backoff left by other tests.
func withLocalStatuses(t *testing.T, statusJSON string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".acp-mobile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".acp-mobile", "status.json"), []byte(statusJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	resetPeerStatusBackoff()
	t.Cleanup(resetPeerStatusBackoff)
}

func TestHandleStatusesMergesPeerStatuses(t *testing.T) {
	withLocalStatuses(t, `{"l1":"idle"}`)
	var gotPath, gotHeader string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeader = r.URL.Path, r.Header.Get(peerHeader)
		fmt.Fprint(w, `{"statuses":{"n1":"busy"}}`)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	got := statusesReply(t, httptest.NewRequest("POST", "/api/statuses", nil))
	if got["l1"] != "idle" || got["n1"] != "busy" {
		t.Fatalf("statuses = %v, want local l1 and the peer's n1", got)
	}
	if gotPath != "/api/statuses" || gotHeader != "1" {
		t.Fatalf("peer call path %q header %q", gotPath, gotHeader)
	}
}

func TestHandleStatusesServesLocalOnlyToPeers(t *testing.T) {
	withLocalStatuses(t, `{"l1":"busy"}`)
	called := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	setPresence(false, "")

	got := statusesReply(t, withPeerFlag(httptest.NewRequest("POST", "/api/statuses", nil)))
	if got["l1"] != "busy" {
		t.Fatalf("statuses = %v", got)
	}
	if called {
		t.Fatal("fanned out on a peer request")
	}
	// A hub's poll is not the phone looking at this machine's page: it must
	// not park this machine's pushes as if the page were visible.
	if visible, _ := presenceFresh(); visible {
		t.Fatal("a peer's status poll marked the page visible")
	}
}

func TestHandleStatusesBacksOffAFailingPeer(t *testing.T) {
	withLocalStatuses(t, `{"l1":"idle"}`)
	calls := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	for i := 0; i < 2; i++ {
		if got := statusesReply(t, httptest.NewRequest("POST", "/api/statuses", nil)); got["l1"] != "idle" {
			t.Fatalf("poll %d: statuses = %v, want local statuses despite the peer", i, got)
		}
	}
	if calls != 1 {
		t.Fatalf("peer polled %d times, want 1 inside the backoff window", calls)
	}
}

// Fork routes like kill: the hub's Emacs does not own a peer's chat, so the
// probe and the fork both go to the machine that does.
func TestForkForwardsToOwningMachine(t *testing.T) {
	argsFile := installFakeEmacsclient(t, "t")
	var seen []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, fmt.Sprintf("%s %s peer=%s machine=%s buffer=%s body=%s", r.Method, r.URL.Path,
			r.Header.Get(peerHeader), r.URL.Query().Get("machine"), r.URL.Query().Get("bufferName"), b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"bufferName":"Claude Agent @ skate<2>","supported":true}`)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	for _, req := range []*http.Request{
		httptest.NewRequest("GET", "/api/fork?bufferName=Claude+Agent+%40+skate&machine=vengeance", nil),
		httptest.NewRequest("POST", "/api/fork", strings.NewReader(`{"bufferName":"Claude Agent @ skate","machine":"vengeance"}`)),
	} {
		rec := httptest.NewRecorder()
		fleetRoute(handleFork)(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"supported":true`) {
			t.Fatalf("%s: hub answered %d %q, want the peer's 200", req.Method, rec.Code, rec.Body.String())
		}
	}
	want := []string{
		"GET /api/fork peer=1 machine=vengeance buffer=Claude Agent @ skate body=",
		`POST /api/fork peer=1 machine=vengeance buffer= body={"bufferName":"Claude Agent @ skate","machine":"vengeance"}`,
	}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("peer saw\n%q\nwant\n%q", seen, want)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("a forwarded fork still ran the hub's emacsclient")
	}
}

func TestForkFromAPeerIsNotForwardedAgain(t *testing.T) {
	installFakeEmacsclient(t, "t")
	called := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := httptest.NewRecorder()
	fleetRoute(handleFork)(rec, withPeerFlag(httptest.NewRequest("POST", "/api/fork",
		strings.NewReader(`{"bufferName":"Claude Agent @ skate","machine":"vengeance"}`))))
	if rec.Code != http.StatusNotImplemented || called {
		t.Fatalf("code=%d forwarded=%v, want 501 and no second hop", rec.Code, called)
	}
}

// peerRecorder is a peer server that records what the hub forwarded.
type peerRecorder struct {
	hits                                 int
	method, path, header, rawQuery, body string
	query                                map[string][]string
}

func newPeerRecorder(t *testing.T, status int, reply string) (*peerRecorder, *httptest.Server) {
	t.Helper()
	rec := &peerRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits++
		rec.method, rec.path, rec.header = r.Method, r.URL.Path, r.Header.Get(peerHeader)
		rec.rawQuery, rec.query = r.URL.RawQuery, r.URL.Query()
		b, _ := io.ReadAll(r.Body)
		rec.body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func TestFleetRouteForwardsToOwningMachine(t *testing.T) {
	argsFile := installFakeEmacsclient(t, "t")
	rec, peer := newPeerRecorder(t, http.StatusTeapot, `{"ok":"peer"}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	body := `{"bufferName":"b","label":"x","machine":"vengeance"}`
	w := httptest.NewRecorder()
	fleetRoute(handleLabel)(w, httptest.NewRequest("POST", "/api/label", strings.NewReader(body)))

	if w.Code != http.StatusTeapot || strings.TrimSpace(w.Body.String()) != `{"ok":"peer"}` {
		t.Fatalf("hub answered %d %q, want the peer's status and body", w.Code, w.Body.String())
	}
	if rec.method != "POST" || rec.path != "/api/label" || rec.header != "1" || rec.body != body {
		t.Fatalf("peer saw %s %s header=%q body=%q", rec.method, rec.path, rec.header, rec.body)
	}
	if got := rec.query["machine"]; len(got) != 1 || got[0] != "vengeance" {
		t.Fatalf("peer query machine = %v, want [vengeance]", got)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("a forwarded request still ran the hub's emacsclient")
	}
}

func TestFleetRouteKeepsTheQueryString(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	w := httptest.NewRecorder()
	fleetRoute(handlePreview)(w, httptest.NewRequest("GET", "/api/preview?pid=40388&machine=vengeance", nil))

	if rec.hits != 1 || rec.method != "GET" || rec.path != "/api/preview" {
		t.Fatalf("peer hits=%d saw %s %s", rec.hits, rec.method, rec.path)
	}
	for k, want := range map[string]string{"pid": "40388", "machine": "vengeance"} {
		if got := rec.query[k]; len(got) != 1 || got[0] != want {
			t.Errorf("peer query %s = %v, want exactly [%s] (raw %q)", k, got, want, rec.rawQuery)
		}
	}
}

func TestFleetRouteRunsLocalWithoutMachine(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	for _, tc := range []struct{ url, body string }{
		{"/api/x", `{"a":1}`},
		{"/api/x?machine=mrx", `{"a":1}`},
		{"/api/x", `{"a":1,"machine":"mrx"}`},
	} {
		ran := ""
		h := fleetRoute(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			ran = string(b)
		})
		h(httptest.NewRecorder(), httptest.NewRequest("POST", tc.url, strings.NewReader(tc.body)))
		if ran != tc.body {
			t.Errorf("%s %s: handler saw body %q, want it to run locally with the full body", tc.url, tc.body, ran)
		}
	}
	if rec.hits != 0 {
		t.Fatalf("peer called %d times for local requests", rec.hits)
	}
}

func TestFleetRouteDoesNotForwardAPeersCall(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))

	ran := false
	w := httptest.NewRecorder()
	fleetRoute(func(http.ResponseWriter, *http.Request) { ran = true })(w, withPeerFlag(
		httptest.NewRequest("POST", "/api/kill", strings.NewReader(`{"bufferName":"x","machine":"vengeance"}`))))

	if rec.hits != 0 || ran || w.Code != http.StatusNotImplemented {
		t.Fatalf("peer-origin call: forwarded %d, ran %v, code %d; want 0, false, 501", rec.hits, ran, w.Code)
	}
}

func TestFleetRouteUnknownOrSilentMachineAnswersJSON(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", deadURL))
	for _, tc := range []struct {
		machine string
		code    int
		want    string
	}{
		{"ghost", http.StatusNotFound, "unknown machine ghost"},
		{"vengeance", http.StatusBadGateway, "NEMESIS did not answer"},
	} {
		w := httptest.NewRecorder()
		fleetRoute(handleLabel)(w, httptest.NewRequest("POST", "/api/label",
			strings.NewReader(`{"bufferName":"x","machine":"`+tc.machine+`"}`)))
		var reply struct{ Error string }
		if w.Code != tc.code || json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.Error != tc.want {
			t.Errorf("machine %s: %d %q, want %d and error %q", tc.machine, w.Code, w.Body.String(), tc.code, tc.want)
		}
	}
	if !strings.Contains(logs.String(), "forward /api/label to vengeance") {
		t.Errorf("log %q does not record the failed forward", logs.String())
	}
}

func TestFleetRouteForwardsABodyAtTheLimitWhole(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	body := strings.Repeat("a", remoteActionPeekLimit)
	w := httptest.NewRecorder()
	fleetRoute(handleLabel)(w, httptest.NewRequest("POST", "/api/label?machine=vengeance", strings.NewReader(body)))
	if w.Code != http.StatusOK || rec.hits != 1 || rec.body != body {
		t.Fatalf("code %d hits %d peer body %d bytes, want 200 and %d bytes byte-equal", w.Code, rec.hits, len(rec.body), len(body))
	}
}

func TestFleetRouteRefusesAnOversizedBody(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	w := httptest.NewRecorder()
	fleetRoute(handleLabel)(w, httptest.NewRequest("POST", "/api/label?machine=vengeance",
		strings.NewReader(strings.Repeat("a", remoteActionPeekLimit+1))))
	if w.Code != http.StatusRequestEntityTooLarge || strings.TrimSpace(w.Body.String()) != `{"error":"request body too large"}` || rec.hits != 0 {
		t.Fatalf("code %d body %q hits %d, want 413, the JSON error, no peer call", w.Code, w.Body.String(), rec.hits)
	}
}

// jsonBodyNaming builds a valid JSON body of exactly n bytes with machine
// as a field.
func jsonBodyNaming(n int, machine string) string {
	head := `{"machine":"` + machine + `","pad":"`
	tail := `"}`
	return head + strings.Repeat("a", n-len(head)-len(tail)) + tail
}

func TestFleetRouteRefusesAnOversizedBodyNamingAMachineInJSON(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	ran := false
	w := httptest.NewRecorder()
	fleetRoute(func(http.ResponseWriter, *http.Request) { ran = true })(w,
		httptest.NewRequest("POST", "/api/label", strings.NewReader(jsonBodyNaming(remoteActionPeekLimit+1, "vengeance"))))
	if w.Code != http.StatusRequestEntityTooLarge || strings.TrimSpace(w.Body.String()) != `{"error":"request body too large"}` || ran || rec.hits != 0 {
		t.Fatalf("code %d body %q ran %v hits %d, want 413, the JSON error, nothing run", w.Code, w.Body.String(), ran, rec.hits)
	}
}

func TestFleetRouteForwardsALimitSizedBodyNamingAMachineInJSON(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	body := jsonBodyNaming(remoteActionPeekLimit, "vengeance")
	ran := false
	w := httptest.NewRecorder()
	fleetRoute(func(http.ResponseWriter, *http.Request) { ran = true })(w,
		httptest.NewRequest("POST", "/api/label", strings.NewReader(body)))
	if w.Code != http.StatusOK || ran || rec.hits != 1 || rec.body != body {
		t.Fatalf("code %d ran %v hits %d peer body %d bytes, want 200 and %d bytes byte-equal", w.Code, ran, rec.hits, len(rec.body), len(body))
	}
}

// routeSpy is a local handler that records whether it ran.
type routeSpy struct {
	ran  bool
	body string
}

func (s *routeSpy) handler(w http.ResponseWriter, r *http.Request) {
	s.ran = true
	b, _ := io.ReadAll(r.Body)
	s.body = string(b)
}

func assertInvalidBody(t *testing.T, w *httptest.ResponseRecorder, spy *routeSpy, rec *peerRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != `{"error":"invalid request body"}` || spy.ran || rec.hits != 0 {
		t.Fatalf("code %d body %q local ran %v peer hits %d, want 400, the JSON error, nothing run", w.Code, w.Body.String(), spy.ran, rec.hits)
	}
}

func TestFleetRouteRejectsABodyWithTrailingData(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	spy := &routeSpy{}
	w := httptest.NewRecorder()
	fleetRoute(spy.handler)(w, httptest.NewRequest("POST", "/api/label",
		strings.NewReader(`{"machine":"vengeance","bufferName":"shared","label":"x"} {}`)))
	assertInvalidBody(t, w, spy, rec)
}

func TestFleetRouteRejectsANonJSONBody(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	for _, body := range []string{`not json`, `{"machine":"vengeance"`, `{"bufferName":"x"}garbage`} {
		spy := &routeSpy{}
		w := httptest.NewRecorder()
		fleetRoute(spy.handler)(w, httptest.NewRequest("POST", "/api/label", strings.NewReader(body)))
		assertInvalidBody(t, w, spy, rec)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestFleetRouteRejectsAnUnreadableBody(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	spy := &routeSpy{}
	w := httptest.NewRecorder()
	fleetRoute(spy.handler)(w, httptest.NewRequest("POST", "/api/label", failingReader{}))
	assertInvalidBody(t, w, spy, rec)
}

func TestFleetRoutePassesAnEmptyBodyThrough(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	for _, body := range []string{``, "  \n"} {
		spy := &routeSpy{}
		w := httptest.NewRecorder()
		fleetRoute(spy.handler)(w, httptest.NewRequest("POST", "/api/mode-words", strings.NewReader(body)))
		if !spy.ran || rec.hits != 0 || spy.body != body {
			t.Fatalf("body %q: local ran %v (saw %q) peer hits %d, want a local run", body, spy.ran, spy.body, rec.hits)
		}
	}
}

func TestFleetRoutePassesANonObjectJSONBodyAsLocal(t *testing.T) {
	rec, peer := newPeerRecorder(t, http.StatusOK, `{}`)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	for _, body := range []string{`[{"machine":"vengeance"}]`, `"vengeance"`, `42`, ` {"a":1}  `} {
		spy := &routeSpy{}
		w := httptest.NewRecorder()
		fleetRoute(spy.handler)(w, httptest.NewRequest("POST", "/api/label", strings.NewReader(body)))
		if !spy.ran || rec.hits != 0 || spy.body != body {
			t.Fatalf("body %q: local ran %v (saw %q) peer hits %d, want a local run with the body intact", body, spy.ran, spy.body, rec.hits)
		}
	}
}

// lockedBuffer is a log sink the handler goroutine writes and the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestFleetRouteAbortsWhenThePeerCutsTheBody(t *testing.T) {
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var reached atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n0123456789")
		buf.Flush()
		conn.Close()
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	hub := httptest.NewServer(fleetRoute(handleFileRead))
	defer hub.Close()

	resp, err := http.Get(hub.URL + "/files/read?machine=vengeance&path=x")
	if err == nil {
		defer resp.Body.Close()
		got, rerr := io.ReadAll(resp.Body)
		if rerr == nil {
			t.Fatalf("read %d bytes cleanly, want a failed transfer", len(got))
		}
	}
	// A client-side failure alone proves nothing: the peer must have been
	// reached and the hub must have logged the cut relay.
	if !reached.Load() {
		t.Error("the peer was never reached")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "relay:") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "relay:") {
		t.Errorf("log %q does not mention the relay failure", logs.String())
	}
}

func TestForwardTimeoutIsPerRoute(t *testing.T) {
	for path, want := range map[string]time.Duration{
		"/api/spawn":             180 * time.Second,
		"/api/resume-transcript": 45 * time.Second,
		"/api/fork":              60 * time.Second,
		"/api/label":             15 * time.Second,
	} {
		if got := forwardTimeout(path); got != want {
			t.Errorf("forwardTimeout(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestMergedSessionsReportGapsAndOS(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"sessions":[]}`)
	}))
	defer peer.Close()
	m := peerAt(t, "vengeance", "NEMESIS", peer.URL)
	m.OS = "windows"
	m.Gaps = []string{"handoff"}
	withFleetFixture(t, "mrx", m)
	_, states := mergedSessions(context.Background())
	raw, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		OS   string   `json:"os"`
		Gaps []string `json:"gaps"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != 2 {
		t.Fatalf("states json %s (%v)", raw, err)
	}
	if got[1].OS != "windows" || len(got[1].Gaps) != 1 || got[1].Gaps[0] != "handoff" {
		t.Errorf("peer = %+v", got[1])
	}
	if got[0].Gaps == nil || len(got[0].Gaps) != 0 {
		t.Errorf("self gaps = %#v, want an empty array", got[0].Gaps)
	}
	if !strings.Contains(string(raw), `"gaps":[]`) {
		t.Errorf("json %s: gaps must serialise as []", raw)
	}
}

func TestFanOutJSONCollectsEveryAnsweringPeer(t *testing.T) {
	var gotHeader, gotType, gotBody atomic.Value
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader.Store(r.Header.Get(peerHeader))
		gotType.Store(r.Header.Get("Content-Type"))
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		fmt.Fprint(w, `{"from":"a"}`)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"from":"b"}`)
	}))
	defer b.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", a.URL), peerAt(t, "castor", "CASTOR", b.URL))
	got := fanOutJSON(context.Background(), "/api/x", []byte(`{"q":1}`), time.Second, nil)
	if len(got) != 2 || string(got["vengeance"]) != `{"from":"a"}` || string(got["castor"]) != `{"from":"b"}` {
		t.Fatalf("got %v", got)
	}
	if gotHeader.Load() != "1" || gotType.Load() != "application/json" || gotBody.Load() != `{"q":1}` {
		t.Errorf("header %v type %v body %v", gotHeader.Load(), gotType.Load(), gotBody.Load())
	}
}

func TestFanOutJSONOmitsFailingAndSlowPeers(t *testing.T) {
	captureLog(t)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	withFleetFixture(t, "mrx",
		peerAt(t, "good", "GOOD", ok.URL), peerAt(t, "bad", "BAD", bad.URL), peerAt(t, "slow", "SLOW", slow.URL))
	start := time.Now()
	got := fanOutJSON(context.Background(), "/api/x", nil, 200*time.Millisecond, nil)
	if len(got) != 1 || got["good"] == nil {
		t.Fatalf("got %v, want only good", got)
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %v, the budget did not bound the slow peer", time.Since(start))
	}
}

func TestFanOutJSONHonoursSkip(t *testing.T) {
	var asked atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		fmt.Fprint(w, `{}`)
	})
	a, b := httptest.NewServer(h), httptest.NewServer(h)
	defer a.Close()
	defer b.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", a.URL), peerAt(t, "castor", "CASTOR", b.URL))
	got := fanOutJSON(context.Background(), "/api/x", nil, time.Second, func(m machine) bool { return m.ID == "castor" })
	if len(got) != 1 || got["vengeance"] == nil || asked.Load() != 1 {
		t.Fatalf("got %v asked %d", got, asked.Load())
	}
}
