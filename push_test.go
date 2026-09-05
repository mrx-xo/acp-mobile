package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePushSidecar(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".acp-mobile"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".acp-mobile", "push.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPushReadsArmedSessionIDs(t *testing.T) {
	writePushSidecar(t, `{"sess-a":true,"sess-b":false}`)
	push := loadPush()
	if !push["sess-a"] {
		t.Fatal("sess-a should be armed")
	}
	if push["sess-b"] {
		t.Fatal("sess-b is explicitly false and must not be armed")
	}
}

func TestLoadPushMissingSidecarIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if push := loadPush(); len(push) != 0 {
		t.Fatalf("missing sidecar should yield no armed sessions, got %v", push)
	}
}

func TestMergePushMarksOnlyArmedSessions(t *testing.T) {
	sessions := []sessionInfo{
		{SessionID: "sess-a"},
		{SessionID: "sess-b"},
		{SessionID: ""},
	}
	mergePush(sessions, map[string]bool{"sess-a": true})
	if !sessions[0].Push {
		t.Fatal("sess-a should be marked push")
	}
	if sessions[1].Push || sessions[2].Push {
		t.Fatal("unarmed sessions must stay unmarked")
	}
}

func TestSessionInfoPushSerializesAsBool(t *testing.T) {
	data, err := json.Marshal(sessionInfo{SessionID: "s", Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"push":true`) {
		t.Fatalf("push field missing from %s", data)
	}
}

func TestHandlePushRejectsNonPost(t *testing.T) {
	w := httptest.NewRecorder()
	handlePush(w, httptest.NewRequest(http.MethodGet, "/api/push", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", w.Code)
	}
}

func TestHandlePushRejectsInvalidBufferName(t *testing.T) {
	body := strings.NewReader(`{"bufferName":"evil\"(kill-emacs)","enabled":true}`)
	w := httptest.NewRecorder()
	handlePush(w, httptest.NewRequest(http.MethodPost, "/api/push", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandlePushRejectsEmptyBufferName(t *testing.T) {
	body := strings.NewReader(`{"bufferName":"","enabled":true}`)
	w := httptest.NewRecorder()
	handlePush(w, httptest.NewRequest(http.MethodPost, "/api/push", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPushExprIsQuotedLispCall(t *testing.T) {
	got := pushExpr(`Claude Agent @ "x"`, true)
	want := `(agent-shell-push-set "Claude Agent @ \"x\"" t)`
	if got != want {
		t.Fatalf("expr = %s, want %s", got, want)
	}
	if off := pushExpr("b", false); off != `(agent-shell-push-set "b" nil)` {
		t.Fatalf("off expr = %s", off)
	}
}
