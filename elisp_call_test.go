package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestElispExprQuotesEachArgumentKind(t *testing.T) {
	got := elispExpr("fn",
		elispStr(`a"b\c`),
		elispB64("héllo"),
		elispRaw("t"),
		elispRaw("42"))
	want := `(fn "a\"b\\c" "aMOpbGxv" t 42)`
	if got != want {
		t.Fatalf("elispExpr = %s, want %s", got, want)
	}
}

func TestCallElispMapsNilToNotFound(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	_, err := callElisp(context.Background(), "fn", elispStr("x"))
	if !errors.Is(err, errElispNotFound) {
		t.Fatalf("err = %v, want errElispNotFound", err)
	}
}

func TestCallElispReturnsTrimmedOutput(t *testing.T) {
	argsFile := installFakeEmacsclient(t, `"on"`)
	out, err := callElisp(context.Background(), "fn", elispStr("x"))
	if err != nil {
		t.Fatal(err)
	}
	if out != `"on"` {
		t.Fatalf("out = %q, want %q", out, `"on"`)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), `(fn "x")`) {
		t.Fatalf("emacsclient args = %q", args)
	}
}

func postJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestLabelHandlerSendsEscapedExpression(t *testing.T) {
	argsFile := installFakeEmacsclient(t, `"ok"`)
	rec := postJSON(t, handleLabel, `{"bufferName":"Claude Agent @ x<2>","label":"say \"hi\""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	args, _ := os.ReadFile(argsFile)
	want := `(mr-x/agent-label-set "Claude Agent @ x<2>" "say \"hi\"")`
	if !strings.Contains(string(args), want) {
		t.Fatalf("emacsclient args = %q, want %q", args, want)
	}
}

func TestLabelHandlerNilIsNotFound(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	rec := postJSON(t, handleLabel, `{"bufferName":"Claude Agent @ x","label":"l"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestPushHandlerReportsState(t *testing.T) {
	argsFile := installFakeEmacsclient(t, `"on"`)
	rec := postJSON(t, handlePush, `{"bufferName":"Claude Agent @ x","enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK   bool `json:"ok"`
		Push bool `json:"push"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.Push {
		t.Fatalf("resp = %+v", resp)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), `(agent-shell-push-set "Claude Agent @ x" t)`) {
		t.Fatalf("emacsclient args = %q", args)
	}
}

func TestKillHandlerNilIsNotFound(t *testing.T) {
	argsFile := installFakeEmacsclient(t, "nil")
	rec := postJSON(t, handleKill, `{"bufferName":"Claude Agent @ x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), `(meta-agent-shell-close-session "Claude Agent @ x")`) {
		t.Fatalf("emacsclient args = %q", args)
	}
}
