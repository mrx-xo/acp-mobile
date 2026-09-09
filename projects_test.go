package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
