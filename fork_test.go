package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
)

func TestForkPostSendsExactExpression(t *testing.T) {
	const bufferName = "Claude Agent @ x"
	const reply = `{"ok":true,"bufferName":"Claude Agent @ x<3>","forkedFrom":"Claude Agent @ x","supported":true}`
	output := `"` + base64.StdEncoding.EncodeToString([]byte(reply)) + `"`
	argsFile := installFakeEmacsclient(t, output)

	rec := postJSON(t, handleFork, `{"bufferName":"Claude Agent @ x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK         bool   `json:"ok"`
		BufferName string `json:"bufferName"`
		ForkedFrom string `json:"forkedFrom"`
		Supported  bool   `json:"supported"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.BufferName != "Claude Agent @ x<3>" ||
		resp.ForkedFrom != bufferName || !resp.Supported {
		t.Fatalf("resp = %+v", resp)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	encodedName := base64.StdEncoding.EncodeToString([]byte(bufferName))
	want := "--eval\n" + `(syzygy-fork-json "` + encodedName + `" nil)` + "\n"
	if string(args) != want {
		t.Fatalf("emacsclient args = %q, want %q", args, want)
	}
}

func TestForkGetProbeSendsExactExpression(t *testing.T) {
	const bufferName = "Claude Agent @ x"
	const reply = `{"ok":true,"bufferName":"Claude Agent @ x","supported":true}`
	output := `"` + base64.StdEncoding.EncodeToString([]byte(reply)) + `"`
	argsFile := installFakeEmacsclient(t, output)

	req := httptest.NewRequest(http.MethodGet,
		"/api/fork?bufferName="+url.QueryEscape(bufferName), nil)
	rec := httptest.NewRecorder()
	handleFork(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK        bool `json:"ok"`
		Supported bool `json:"supported"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.Supported {
		t.Fatalf("resp = %+v", resp)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	encodedName := base64.StdEncoding.EncodeToString([]byte(bufferName))
	want := "--eval\n" + `(syzygy-fork-json "` + encodedName + `" t)` + "\n"
	if string(args) != want {
		t.Fatalf("emacsclient args = %q, want %q", args, want)
	}
}

func TestForkNilIsNotFound(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			installFakeEmacsclient(t, "nil")

			var rec *httptest.ResponseRecorder
			if method == http.MethodPost {
				rec = postJSON(t, handleFork, `{"bufferName":"Claude Agent @ x"}`)
			} else {
				req := httptest.NewRequest(http.MethodGet,
					"/api/fork?bufferName="+url.QueryEscape("Claude Agent @ x"), nil)
				rec = httptest.NewRecorder()
				handleFork(rec, req)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestForkUnsupportedIsConflict(t *testing.T) {
	const reply = `{"ok":false,"bufferName":"Claude Agent @ x","supported":false,"error":"Agent does not support session forking"}`
	output := `"` + base64.StdEncoding.EncodeToString([]byte(reply)) + `"`
	installFakeEmacsclient(t, output)

	rec := postJSON(t, handleFork, `{"bufferName":"Claude Agent @ x"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK         bool   `json:"ok"`
		BufferName string `json:"bufferName"`
		Supported  bool   `json:"supported"`
		Error      string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Supported || resp.BufferName != "Claude Agent @ x" ||
		resp.Error != "Agent does not support session forking" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestForkRejectsInvalidBufferName(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bufferName string
		body       string
	}{
		{
			name:       "invalid",
			bufferName: `evil"(kill-emacs)`,
			body:       `{"bufferName":"evil\"(kill-emacs)"}`,
		},
		{
			name:       "empty",
			bufferName: "",
			body:       `{"bufferName":""}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{http.MethodPost, http.MethodGet} {
				t.Run(method, func(t *testing.T) {
					argsFile := installFakeEmacsclient(t, "nil")

					var rec *httptest.ResponseRecorder
					if method == http.MethodPost {
						rec = postJSON(t, handleFork, tc.body)
					} else {
						req := httptest.NewRequest(http.MethodGet,
							"/api/fork?bufferName="+url.QueryEscape(tc.bufferName), nil)
						rec = httptest.NewRecorder()
						handleFork(rec, req)
					}
					if rec.Code != http.StatusBadRequest {
						t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
					}
					if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
						t.Fatalf("emacsclient args file should not exist, stat err = %v", err)
					}
				})
			}
		})
	}
}

func TestForkRejectsPut(t *testing.T) {
	argsFile := installFakeEmacsclient(t, "nil")
	req := httptest.NewRequest(http.MethodPut,
		"/api/fork?bufferName="+url.QueryEscape("Claude Agent @ x"), nil)
	rec := httptest.NewRecorder()
	handleFork(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("emacsclient args file should not exist, stat err = %v", err)
	}
}
