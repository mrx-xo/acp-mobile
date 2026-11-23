package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
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
	rec := postJSON(t, handleProjects, `{}`)
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
	stubHubProjects(t, `[{"name":"dotfiles","path":"/h/dotfiles"}]`)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postJSON(t, handleProjects, `{}`)
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
	req := withPeerFlag(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)))
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
	installFakeEmacsclient(t, "nil")
	previous := projectLiveCwds
	projectLiveCwds = func() []string { return nil }
	t.Cleanup(func() { projectLiveCwds = previous })
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"projects":[{"name":"dotfiles","path":"C:/d/dotfiles"}]}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postJSON(t, handleProjects, `{}`)
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
}

func TestMergedProjectsErrorWhenNoMachineHasAny(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"empty list": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"projects":[]}`)) },
		"500":        func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
	} {
		t.Run(name, func(t *testing.T) {
			installFakeEmacsclient(t, "nil")
			previous := projectLiveCwds
			projectLiveCwds = func() []string { return nil }
			t.Cleanup(func() { projectLiveCwds = previous })
			peer := httptest.NewServer(handler)
			defer peer.Close()
			withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
			rec := postJSON(t, handleProjects, `{}`)
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no projects on the rig") {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}
