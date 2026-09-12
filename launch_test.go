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

func TestLaunchSettingsReachBridgeAndReturnExactBuffer(t *testing.T) {
	argsFile := installFakeEmacsclient(t, elispB64Output(`{"ok":true,"bufferName":"Codex Agent @ project"}`))
	body := `{"cwd":"/tmp/project","task":"quote \" and newline\nλ","settings":{"agent":"codex","model":"sol","mode":"agent","effort":"high"}}`
	rec := postJSON(t, handleSpawn, body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"bufferName":"Codex Agent @ project"`) {
		t.Fatalf("response %d %s", rec.Code, rec.Body.String())
	}
	args, _ := os.ReadFile(argsFile)
	expr := string(args)
	start := strings.Index(expr, `(syzygy-launch-json "`)
	if start < 0 {
		t.Fatalf("explicit launch did not use launch bridge: %s", expr)
	}
	encoded := strings.Split(expr[start+len(`(syzygy-launch-json "`):], `"`)[0]
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["task"] != "quote \" and newline\nλ" || got["settings"].(map[string]interface{})["effort"] != "high" {
		t.Fatalf("payload = %#v", got)
	}
}

func TestLaunchRejectsInvalidSettingsBeforeBridge(t *testing.T) {
	for _, body := range []string{
		`{"cwd":"/p","settings":{}}`,
		`{"cwd":"/p","settings":{"agent":"codex","model":"sol","mode":"agent","effort":"bad\nvalue"}}`,
		`{"cwd":"/p","cloneOf":"existing","settings":{"agent":"codex","model":"sol","mode":"agent"}}`,
		`{"settings":{"agent":"codex","model":"sol","mode":"agent"}}`,
	} {
		rec := postJSON(t, handleSpawn, body)
		if rec.Code != 400 {
			t.Fatalf("body %s => %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestLaunchPartialFailureKeepsBufferForRecovery(t *testing.T) {
	installFakeEmacsclient(t, elispB64Output(`{"ok":false,"error":"Effort rejected","bufferName":"new buffer"}`))
	rec := postJSON(t, handleSpawn, `{"cwd":"/p","settings":{"agent":"codex","model":"sol","mode":"agent"}}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "new buffer") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestLaunchMalformedSuccessIsNotAccepted(t *testing.T) {
	installFakeEmacsclient(t, elispB64Output(`{"ok":true}`))
	rec := postJSON(t, handleSpawn, `{"cwd":"/p","settings":{"agent":"codex","model":"sol","mode":"agent"}}`)
	if rec.Code != 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestLaunchOptionsMethodsAndShape(t *testing.T) {
	installFakeEmacsclient(t, elispB64Output(`{"agents":[],"defaultAgent":""}`))
	rec := postJSON(t, handleLaunchOptions, `{}`)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleLaunchOptions(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 405 {
		t.Fatalf("GET: %d", rec.Code)
	}
}
