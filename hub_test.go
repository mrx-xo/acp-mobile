package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		return []byte(`{"Peer":{"k":{"HostName":"vengeance","DNSName":"vengeance.example.ts.net.","TailscaleIPs":["fd7a::1","100.100.0.33"]}}}`), nil
	}
	withFleetFixture(t, "mrx", machine{ID: "vengeance", IP: "100.100.0.3"})
	refreshPeerIP("vengeance")
	m, _ := fleet.reg.byID("vengeance")
	if m.IP != "100.100.0.33" {
		t.Fatalf("ip = %s", m.IP)
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
