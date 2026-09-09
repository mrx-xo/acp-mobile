package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPinHandlerGetMirrorsJSON(t *testing.T) {
	reply := `{"pins":["Claude Agent @ x<2>"]}`
	argsFile := installFakeEmacsclient(t,
		`"`+base64.StdEncoding.EncodeToString([]byte(reply))+`"`)
	rec := httptest.NewRecorder()
	handlePin(rec, httptest.NewRequest(http.MethodGet, "/api/pin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != reply {
		t.Fatalf("body = %s, want %s", got, reply)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := `(syzygy-orrery-pin-json)`
	if !strings.Contains("\n"+string(args), "\n"+want+"\n") {
		t.Fatalf("emacsclient args = %q, want expression %q", args, want)
	}
}

func TestPinHandlerPostSendsExpressionsAndMirrorsJSON(t *testing.T) {
	name := base64.StdEncoding.EncodeToString([]byte("Claude Agent @ x<2>"))
	for _, tc := range []struct {
		name  string
		body  string
		want  string
		reply string
	}{
		{
			name:  "toggle",
			body:  `{"bufferName":"Claude Agent @ x<2>"}`,
			want:  `(syzygy-orrery-pin-json "` + name + `" nil)`,
			reply: `{"bufferName":"Claude Agent @ x<2>","pinned":true,"pins":["Claude Agent @ x<2>"]}`,
		},
		{
			name:  "pin",
			body:  `{"bufferName":"Claude Agent @ x<2>","action":"pin"}`,
			want:  `(syzygy-orrery-pin-json "` + name + `" "pin")`,
			reply: `{"bufferName":"Claude Agent @ x<2>","pinned":true,"pins":["Claude Agent @ x<2>"]}`,
		},
		{
			name:  "unpin",
			body:  `{"bufferName":"Claude Agent @ x<2>","action":"unpin"}`,
			want:  `(syzygy-orrery-pin-json "` + name + `" "unpin")`,
			reply: `{"bufferName":"Claude Agent @ x<2>","pinned":false,"pins":[]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := installFakeEmacsclient(t,
				`"`+base64.StdEncoding.EncodeToString([]byte(tc.reply))+`"`)
			rec := postJSON(t, handlePin, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.reply {
				t.Fatalf("body = %s, want %s", got, tc.reply)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains("\n"+string(args), "\n"+tc.want+"\n") {
				t.Fatalf("emacsclient args = %q, want expression %q", args, tc.want)
			}
		})
	}
}

func TestPinHandlerRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "invalid action",
			body: `{"bufferName":"Claude Agent @ x","action":"other"}`,
		},
		{
			name: "empty buffer name",
			body: `{"bufferName":""}`,
		},
		{
			name: "quote in buffer name",
			body: `{"bufferName":"Claude Agent @ \"x\""}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := installFakeEmacsclient(t, "nil")
			rec := postJSON(t, handlePin, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
				t.Fatalf("emacsclient should not be called; args file stat error = %v", err)
			}
		})
	}
}

func TestPinHandlerNilIsNotFound(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	rec := postJSON(t, handlePin, `{"bufferName":"Claude Agent @ missing"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestPinHandlerRejectsDelete(t *testing.T) {
	rec := httptest.NewRecorder()
	handlePin(rec, httptest.NewRequest(http.MethodDelete, "/api/pin", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d", rec.Code)
	}
}
