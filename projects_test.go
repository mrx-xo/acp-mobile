package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMergeProjectsRigFirstAndDedupes(t *testing.T) {
	got := mergeProjects([]project{{Name: "one", Path: "/one"}, {Name: "two", Path: "/two"}, {Name: "duplicate", Path: "/one"}}, []string{"/two", "/three", "/three"})
	want := []project{{Name: "one", Path: "/one"}, {Name: "two", Path: "/two", Live: true}, {Name: "three", Path: "/three", Live: true}}
	if !equalProjects(got, want) {
		t.Fatalf("projects = %+v, want %+v", got, want)
	}
}

func equalProjects(a, b []project) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func TestProjectsHandlerMirrorsRigList(t *testing.T) {
	argsFile := installFakeEmacsclient(t, `"`+base64.StdEncoding.EncodeToString([]byte(`[{"name":"dotfiles","path":"/Users/x/.dotfiles"}]`))+`"`)
	rec := postJSON(t, handleProjects, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"dotfiles"`) || !strings.Contains(rec.Body.String(), `"live":false`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "(syzygy-projects-json)") {
		t.Fatalf("emacsclient args = %q", args)
	}
}

func TestProjectsHandlerNilIsNotFound(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	previous := projectLiveCwds
	projectLiveCwds = func() []string { return nil }
	t.Cleanup(func() { projectLiveCwds = previous })
	rec := postJSON(t, handleProjects, `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestProjectsHandlerRejectsGet(t *testing.T) {
	get := httptest.NewRecorder()
	handleProjects(get, httptest.NewRequest(http.MethodGet, "/", nil))
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", get.Code)
	}
}

// stubHubProjects fakes the hub's Lisp call and live cwds.
func stubHubProjects(t *testing.T, listJSON string) {
	t.Helper()
	installFakeEmacsclient(t, `"`+base64.StdEncoding.EncodeToString([]byte(listJSON))+`"`)
	previous := projectLiveCwds
	projectLiveCwds = func() []string { return nil }
	t.Cleanup(func() { projectLiveCwds = previous })
}

func TestMergedProjectsTagEachMachine(t *testing.T) {
	stubHubProjects(t, `[{"name":"dotfiles","path":"/h/dotfiles"}]`)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(peerHeader) != "1" || r.Method != http.MethodPost {
			t.Errorf("peer got %s with header %q", r.Method, r.Header.Get(peerHeader))
		}
		w.Write([]byte(`{"projects":[{"name":"dotfiles","path":"C:/d/dotfiles"}]}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postProjects(t, handleProjects, "/?fleet=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Projects []project `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Projects) != 2 ||
		body.Projects[0].Machine != "mrx" || body.Projects[0].Path != "/h/dotfiles" ||
		body.Projects[1].Machine != "vengeance" || body.Projects[1].Path != "C:/d/dotfiles" {
		t.Fatalf("projects = %+v", body.Projects)
	}
}

func TestMergedProjectsSkipAFailingPeer(t *testing.T) {
	captureLog(t)
	stubHubProjects(t, `[{"name":"dotfiles","path":"/h/dotfiles"}]`)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postProjects(t, handleProjects, "/?fleet=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Projects []project `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Projects) != 1 || body.Projects[0].Machine != "mrx" {
		t.Fatalf("projects = %+v", body.Projects)
	}
}

func TestProjectsServeLocalOnlyToPeers(t *testing.T) {
	stubHubProjects(t, `[{"name":"dotfiles","path":"/h/dotfiles"}]`)
	var called atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.Write([]byte(`{"projects":[{"name":"x","path":"/x"}]}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	req := withPeerFlag(httptest.NewRequest(http.MethodPost, "/?fleet=1", strings.NewReader(`{}`)))
	rec := httptest.NewRecorder()
	handleProjects(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"/x"`) || !strings.Contains(rec.Body.String(), "/h/dotfiles") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if called.Load() {
		t.Error("a peer's request fanned out")
	}
}

func TestMergedProjectsSurviveALocalFailure(t *testing.T) {
	logs := captureLog(t)
	installFakeEmacsclient(t, "nil")
	previous := projectLiveCwds
	projectLiveCwds = func() []string { return nil }
	t.Cleanup(func() { projectLiveCwds = previous })
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"projects":[{"name":"dotfiles","path":"C:/d/dotfiles"}]}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postProjects(t, handleProjects, "/?fleet=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Projects []project `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Projects) != 1 || body.Projects[0].Machine != "vengeance" || body.Projects[0].Path != "C:/d/dotfiles" {
		t.Fatalf("projects = %+v", body.Projects)
	}
	if !strings.Contains(logs.String(), "using other machines") {
		t.Errorf("log %q does not record the local failure", logs.String())
	}
}

func TestMergedProjectsErrorWhenNoMachineHasAny(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"empty list": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"projects":[]}`)) },
		"500":        func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
	} {
		t.Run(name, func(t *testing.T) {
			captureLog(t)
			installFakeEmacsclient(t, "nil")
			previous := projectLiveCwds
			projectLiveCwds = func() []string { return nil }
			t.Cleanup(func() { projectLiveCwds = previous })
			peer := httptest.NewServer(handler)
			defer peer.Close()
			withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
			rec := postProjects(t, handleProjects, "/?fleet=1")
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no projects on the rig") {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// captureLog routes the standard logger into a buffer for the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

func postProjects(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{}`)))
	return rec
}

func projectPaths(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Projects []project `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	var out []string
	for _, p := range body.Projects {
		out = append(out, p.Machine+":"+p.Path)
	}
	return out
}

func peerCountingProjects(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"projects":[{"name":"x","path":"/x"}]}`))
	}))
	t.Cleanup(peer.Close)
	return peer, &calls
}

func TestProjectsWithoutFleetFlagStayLocal(t *testing.T) {
	stubHubProjects(t, `[{"name":"dotfiles","path":"/h/dotfiles"}]`)
	peer, calls := peerCountingProjects(t)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postProjects(t, handleProjects, "/")
	if got := projectPaths(t, rec); len(got) != 1 || got[0] != "mrx:/h/dotfiles" || calls.Load() != 0 {
		t.Fatalf("got %v, peer calls %d", got, calls.Load())
	}
}

func TestProjectsNamingThisMachineStayLocal(t *testing.T) {
	stubHubProjects(t, `[{"name":"dotfiles","path":"/h/dotfiles"}]`)
	peer, calls := peerCountingProjects(t)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postProjects(t, fleetRoute(handleProjects), "/?machine=mrx&fleet=1")
	if got := projectPaths(t, rec); len(got) != 1 || got[0] != "mrx:/h/dotfiles" || calls.Load() != 0 {
		t.Fatalf("got %v, peer calls %d", got, calls.Load())
	}
}

func TestProjectsNamingAnotherMachineAreForwarded(t *testing.T) {
	// The hub's Emacs stub would fail loudly if it were used.
	installFakeEmacsclient(t, "garbage")
	peer, calls := peerCountingProjects(t)
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postProjects(t, fleetRoute(handleProjects), "/api/projects?machine=vengeance&fleet=1")
	if rec.Code != http.StatusOK || calls.Load() != 1 || !strings.Contains(rec.Body.String(), `"/x"`) {
		t.Fatalf("status %d body %s peer calls %d", rec.Code, rec.Body.String(), calls.Load())
	}
}

func TestFanOutLogsEachFailureWithMachineAndReason(t *testing.T) {
	logs := captureLog(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", bad.URL), peerAt(t, "castor", "CASTOR", dead.URL))
	fanOutJSON(context.Background(), "/api/x", nil, time.Second, nil)
	for _, want := range []string{"fleet: /api/x on vengeance: status 500", "fleet: /api/x on castor: "} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q lacks %q", logs.String(), want)
		}
	}
}

func TestMergedProjectsLogAMalformedPeerReply(t *testing.T) {
	logs := captureLog(t)
	stubHubProjects(t, `[{"name":"dotfiles","path":"/h/dotfiles"}]`)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postProjects(t, handleProjects, "/?fleet=1")
	if got := projectPaths(t, rec); len(got) != 1 || got[0] != "mrx:/h/dotfiles" {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(logs.String(), "fleet: /api/projects on vengeance: ") {
		t.Errorf("log %q lacks the decode failure", logs.String())
	}
}

// installBlockingEmacsclient is an emacsclient that never answers until the
// test ends: the cleanup releases it, so no process outlives the test.
func installBlockingEmacsclient(t *testing.T) (release func()) {
	t.Helper()
	dir := t.TempDir()
	releasePath := filepath.Join(dir, "release")
	script := "#!/bin/sh\nwhile [ ! -e \"" + releasePath + "\" ]; do sleep 0.05; done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "emacsclient"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	previousConfig := appConfig
	appConfig = config{}
	t.Cleanup(func() { appConfig = previousConfig })
	return func() { os.WriteFile(releasePath, nil, 0600) }
}

// finishLocalLookups registers, last, the cleanup that releases the blocked
// lookup and waits for it to end before earlier cleanups restore globals.
func finishLocalLookups(t *testing.T, release func()) {
	t.Helper()
	t.Cleanup(func() {
		release()
		localLookups.Wait()
	})
}

// shortenLocalBudget lets a test see the local deadline fire quickly.
func shortenLocalBudget(t *testing.T) {
	t.Helper()
	previous := mergedLocalBudget
	mergedLocalBudget = 300 * time.Millisecond
	t.Cleanup(func() { mergedLocalBudget = previous })
}

// answersWithin runs serve and fails the test when it has not returned in d.
// A request that is still running then keeps its own recorder; the test's
// cleanups release whatever it waits on.
func answersWithin(t *testing.T, d time.Duration, serve func(*httptest.ResponseRecorder)) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(rec)
	}()
	select {
	case <-done:
		return rec
	case <-time.After(d):
		t.Fatalf("no answer within %v: a hung local lookup blocks the merged list", d)
		return nil
	}
}

func TestMergedProjectsDoNotWaitForAHungLocalLookup(t *testing.T) {
	logs := captureLog(t)
	release := installBlockingEmacsclient(t)
	shortenLocalBudget(t)
	previous := projectLiveCwds
	projectLiveCwds = func() []string { return nil }
	t.Cleanup(func() { projectLiveCwds = previous })
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"projects":[{"name":"dotfiles","path":"C:/d/dotfiles"}]}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	finishLocalLookups(t, release)
	started := time.Now()
	rec := answersWithin(t, 3*time.Second, func(rec *httptest.ResponseRecorder) {
		handleProjects(rec, httptest.NewRequest(http.MethodPost, "/?fleet=1", strings.NewReader(`{}`)))
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "C:/d/dotfiles") {
		t.Fatalf("status = %d body = %s, want the peer's project", rec.Code, rec.Body.String())
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("answered after %v, want about the local budget", took)
	}
	if !strings.Contains(logs.String(), "projects: ") || !strings.Contains(logs.String(), "; using other machines") {
		t.Errorf("log %q does not record the local timeout", logs.String())
	}
}
