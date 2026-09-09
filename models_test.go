package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestModelHandlersArmoredReplies(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, reply, expression string
		status                              int
	}{
		{
			"list", "/api/models", `{"bufferName":"Claude Agent @ x<2>"}`,
			`{"current":"gpt-6-astra","models":[{"id":"fable[1m]","name":"Fable","description":"Modèle rapide"},{"id":"gpt-6-astra","name":"Astra","description":""}]}`,
			`(syzygy-models-json "Claude Agent @ x<2>")`, http.StatusOK,
		},
		{
			"empty", "/api/models", `{"bufferName":"Claude Agent @ x<2>"}`,
			`{"current":"","models":[]}`,
			`(syzygy-models-json "Claude Agent @ x<2>")`, http.StatusOK,
		},
		{
			"set", "/api/model", `{"bufferName":"Claude Agent @ x<2>","modelId":"fable[1m]"}`,
			`{"ok":true,"current":"fable[1m]"}`,
			`(syzygy-model-set-json "Claude Agent @ x<2>" "fable[1m]")`, http.StatusOK,
		},
		{
			"failure", "/api/model", `{"bufferName":"Claude Agent @ x<2>","modelId":"unknown"}`,
			`{"ok":false,"error":"unknown model"}`,
			`(syzygy-model-set-json "Claude Agent @ x<2>" "unknown")`, http.StatusConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := installFakeEmacsclient(t, elispB64Output(tc.reply))
			mux := http.NewServeMux()
			registerModelHandlers(mux)
			rec := postJSON(t, func(w http.ResponseWriter, r *http.Request) {
				r.URL.Path = tc.path
				mux.ServeHTTP(w, r)
			}, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			var got, want interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.reply), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("reply = %s, want %s", rec.Body.String(), tc.reply)
			}
			if rec.Header().Get("Content-Type") != "application/json" ||
				rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("headers = %v", rec.Header())
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(args) != "--eval\n"+tc.expression+"\n" {
				t.Fatalf("emacsclient args = %q", args)
			}
		})
	}
}

func TestModelHandlersErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		body    string
	}{
		{"list", handleModels, `{"bufferName":"Agent"}`},
		{"set", handleModel, `{"bufferName":"Agent","modelId":"gpt-6-astra"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, output := range []string{"nil", elispB64Output("{"), "unarmored"} {
				t.Run(output, func(t *testing.T) {
					installFakeEmacsclient(t, output)
					rec := postJSON(t, tc.handler, tc.body)
					want := http.StatusInternalServerError
					if output == "nil" {
						want = http.StatusNotFound
					}
					if rec.Code != want {
						t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
					}
					if output == "nil" && strings.TrimSpace(rec.Body.String()) != `{"error":"no such session"}` {
						t.Fatalf("body = %s", rec.Body.String())
					}
				})
			}
			argsFile := installFakeEmacsclient(t, "nil")
			for _, body := range []string{"{", `{}`, `{"bufferName":42}`, `{"bufferName":"bad\\"}`} {
				rec := postJSON(t, tc.handler, body)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("body %s: status = %d", body, rec.Code)
				}
			}
			rec := httptest.NewRecorder()
			tc.handler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("GET status = %d", rec.Code)
			}
			if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
				t.Fatalf("invalid requests invoked emacsclient: %v", err)
			}
		})
	}
}

func TestModelHandlerRejectsBadModelIDs(t *testing.T) {
	argsFile := installFakeEmacsclient(t, "nil")
	for _, id := range []string{"", strings.Repeat("a", 129), "a\x00b", "a\nb", "a\tb", "a\x7fb", "modèle", "a\"b", "a\\b"} {
		body, err := json.Marshal(map[string]string{"bufferName": "Agent", "modelId": id})
		if err != nil {
			t.Fatal(err)
		}
		rec := postJSON(t, handleModel, string(body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("modelId %q: status = %d body = %s", id, rec.Code, rec.Body.String())
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("invalid model id invoked emacsclient: %v", err)
	}
}

func TestModelHandlerAccepts128ByteModelID(t *testing.T) {
	id := strings.Repeat("a", 128)
	argsFile := installFakeEmacsclient(t, elispB64Output(`{"ok":true,"current":"`+id+`"}`))
	rec := postJSON(t, handleModel, `{"bufferName":"Agent","modelId":"`+id+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "--eval\n(syzygy-model-set-json \"Agent\" \""+id+"\")\n" {
		t.Fatalf("emacsclient args = %q", args)
	}
}
