package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func elispB64Output(jsonText string) string {
	return `"` + base64.StdEncoding.EncodeToString([]byte(jsonText)) + `"`
}

func TestCallElispJSONDecodesBase64Armor(t *testing.T) {
	installFakeEmacsclient(t, elispB64Output(`{"label":"Fable · Bypass"}`))
	raw, err := callElispJSON(context.Background(), "fn")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"label":"Fable · Bypass"}` {
		t.Fatalf("raw = %s", raw)
	}
}

func TestCallElispJSONRejectsUnarmoredOutput(t *testing.T) {
	installFakeEmacsclient(t, `(error "boom")`)
	if _, err := callElispJSON(context.Background(), "fn"); err == nil {
		t.Fatal("expected an error for non-base64 output")
	}
}

func TestPresetsHandlerMirrorsRigOrder(t *testing.T) {
	argsFile := installFakeEmacsclient(t, elispB64Output(`[
	  {"key":"f","label":"Fable 5.1 · Bypass","model":"fable[1m]","mode":"bypassPermissions","agent":"claude","effort":""},
	  {"key":"F","label":"Fable 5 · Bypass","model":"claude-fable-5[1m]","mode":"bypassPermissions","agent":"claude","effort":""},
	  {"key":"a","label":"Astra · Full","model":"gpt-6-astra","mode":"agent-full-access","agent":"codex","effort":"high"}
	]`))
	rec := postJSON(t, handlePresets, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Presets []spawnPreset `json:"presets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Presets) != 3 {
		t.Fatalf("presets = %+v", resp.Presets)
	}
	if resp.Presets[0].Key != "f" || resp.Presets[1].Key != "F" || resp.Presets[2].Key != "a" {
		t.Fatalf("order = %+v", resp.Presets)
	}
	if resp.Presets[0].Label != "Fable 5.1 · Bypass" {
		t.Fatalf("label = %q, non-ASCII must survive the armor", resp.Presets[0].Label)
	}
	if resp.Presets[2].Agent != "codex" || resp.Presets[2].Effort != "high" {
		t.Fatalf("codex preset = %+v", resp.Presets[2])
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "(syzygy-presets-json)") {
		t.Fatalf("emacsclient args = %q", args)
	}
}

func TestPresetsHandlerEmptyListIsNotAnError(t *testing.T) {
	installFakeEmacsclient(t, elispB64Output(`[]`))
	rec := postJSON(t, handlePresets, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != `{"presets":[]}` {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestPresetsHandlerNilIsNotFound(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	rec := postJSON(t, handlePresets, `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestPresetsHandlerRejectsGet(t *testing.T) {
	installFakeEmacsclient(t, elispB64Output(`[]`))
	rec := postJSON(t, handlePresets, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d", rec.Code)
	}
	get := httptest.NewRecorder()
	handlePresets(get, httptest.NewRequest(http.MethodGet, "/", nil))
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", get.Code)
	}
}

func TestSpawnAcceptsUppercasePresetKeys(t *testing.T) {
	argsFile := installFakeSpawnScript(t)
	rec := postJSON(t, handleSpawn, `{"cwd":"/tmp/x","preset":"F"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	args, _ := os.ReadFile(argsFile)
	if string(args) != "\n/tmp/x\n\nF\n" {
		t.Fatalf("spawn args = %q", args)
	}
	for _, bad := range []string{"ff", "1", "-", "é"} {
		rec := postJSON(t, handleSpawn, `{"cwd":"/tmp/x","preset":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("preset %q: status = %d", bad, rec.Code)
		}
	}
}

// installFakeSpawnScript shadows the agent-shell-spawn bridge script and
// records its argv one per line.
func installFakeSpawnScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$FAKE_SPAWN_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "agent-shell-spawn"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SPAWN_ARGS", argsFile)
	previousConfig := appConfig
	appConfig = config{}
	t.Cleanup(func() { appConfig = previousConfig })
	return argsFile
}
