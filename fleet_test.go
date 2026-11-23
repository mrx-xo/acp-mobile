package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRegistryParsesFixture(t *testing.T) {
	reg, err := loadRegistry("testdata/syzygy.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Machines) != 3 {
		t.Fatalf("machines = %d, want 3", len(reg.Machines))
	}
	m, ok := reg.byID("vengeance")
	if !ok || m.Name != "NEMESIS" || m.OS != "windows" || m.Role != "node" {
		t.Fatalf("vengeance = %+v", m)
	}
	if m.Wake == nil || m.Wake.Relay != "homelab" {
		t.Fatalf("wake = %+v", m.Wake)
	}
	if got := reg.peers("mrx"); len(got) != 2 {
		t.Fatalf("peers = %d, want 2", len(got))
	}
}

func TestLoadRegistryMissingFileIsNode(t *testing.T) {
	reg, err := loadRegistry(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.peers("mrx")) != 0 {
		t.Fatal("missing registry must yield no peers")
	}
}

func TestLoadRegistryRejectsMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(p, []byte("{nope"), 0600)
	if _, err := loadRegistry(p); err == nil {
		t.Fatal("want parse error")
	}
}

// Every machine-scoped route, wrapped as main() registers it, hands a
// request that names another machine to that machine and never runs the
// hub's own emacsclient or spawn script.
func TestRemoteMachineActionsAreForwarded(t *testing.T) {
	argsFile := installFakeEmacsclient(t, "nil")
	spawnArgs := installFakeSpawnScript(t)
	var paths []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		req  *http.Request
	}{
		{"spawn", handleSpawn, httptest.NewRequest("POST", "/api/spawn", strings.NewReader(`{"cwd":"/tmp","machine":"vengeance"}`))},
		{"label", handleLabel, httptest.NewRequest("POST", "/api/label", strings.NewReader(`{"bufferName":"x","machine":"vengeance"}`))},
		{"push", handlePush, httptest.NewRequest("POST", "/api/push", strings.NewReader(`{"bufferName":"x","machine":"vengeance"}`))},
		{"pin", handlePin, httptest.NewRequest("POST", "/api/pin", strings.NewReader(`{"bufferName":"x","machine":"vengeance"}`))},
		{"preview", handlePreview, httptest.NewRequest("GET", "/api/preview?pid=1&machine=vengeance", nil)},
	} {
		before := len(paths)
		rec := httptest.NewRecorder()
		fleetRoute(tc.h)(rec, tc.req)
		if rec.Code != http.StatusOK || len(paths) != before+1 || paths[before] != tc.req.URL.Path {
			t.Errorf("%s: code=%d peer paths=%v, want one forward to %s", tc.name, rec.Code, paths[before:], tc.req.URL.Path)
		}
	}
	for _, f := range []string{argsFile, spawnArgs} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("a forwarded action still ran %s", f)
		}
	}
}

// The guard never changes a request aimed at this machine or at none: the
// handler still sees the full body.
func TestRemoteMachineGuardPassesLocalRequests(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	withFleetFixture(t, "mrx")
	for _, body := range []string{`{"bufferName":"bad;name"}`, `{"bufferName":"bad;name","machine":"mrx"}`} {
		rec := httptest.NewRecorder()
		fleetRoute(handleKill)(rec, httptest.NewRequest("POST", "/api/kill", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid buffer name") {
			t.Errorf("body %s: code=%d body=%q, want the handler's own 400", body, rec.Code, rec.Body.String())
		}
	}
}
