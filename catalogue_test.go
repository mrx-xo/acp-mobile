package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogueHandlerSendsExpressionsAndMirrorsJSON(t *testing.T) {
	sessionID := "session-1"
	note := `Résumé: say "hi"`
	b64 := func(s string) string {
		return base64.StdEncoding.EncodeToString([]byte(s))
	}
	saveBody, err := json.Marshal(catalogueRequest{
		SessionID: sessionID,
		Note:      note,
		Tags:      []string{" #Syzygy ", "resume", "RESUME", "", "#"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		method string
		target string
		body   string
		want   string
		reply  string
	}{
		{
			name:   "save",
			method: http.MethodPost,
			target: "/api/catalogue",
			body:   string(saveBody),
			want: `(syzygy-recall-catalogue-json "` + b64(sessionID) +
				`" "` + b64(note) +
				`" "` + b64(`["syzygy","resume"]`) + `")`,
			reply: `{"sessionId":"session-1","catalogued":"2026-09-08T12:00:00Z","note":"Résumé: say \"hi\"","tags":["syzygy","resume"],"allTags":["syzygy","resume"]}`,
		},
		{
			name:   "get",
			method: http.MethodGet,
			target: "/api/catalogue?sessionId=session-1",
			want:   `(syzygy-recall-catalogue-get-json "` + b64(sessionID) + `")`,
			reply:  `{"sessionId":"session-1","catalogued":"2026-09-08T12:00:00Z","note":"","tags":[],"allTags":[]}`,
		},
		{
			name:   "uncatalogue",
			method: http.MethodPost,
			target: "/api/catalogue",
			body:   `{"sessionId":"session-1","uncatalogue":true}`,
			want:   `(syzygy-recall-uncatalogue-json "` + b64(sessionID) + `")`,
			reply:  `{"sessionId":"session-1","catalogued":"","note":"","tags":[],"allTags":[]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := installFakeEmacsclient(t,
				`"`+base64.StdEncoding.EncodeToString([]byte(tc.reply))+`"`)
			rec := httptest.NewRecorder()
			handleCatalogue(rec, httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body)))
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

func TestCatalogueHandlerNilIsNotFound(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	rec := postJSON(t, handleCatalogue, `{"sessionId":"missing"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestCatalogueHandlerRejectsInvalidRequests(t *testing.T) {
	tags := make([]string, 21)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag-%02d", i)
	}
	for _, tc := range []struct {
		name string
		req  catalogueRequest
	}{
		{
			name: "note over 500 runes",
			req:  catalogueRequest{SessionID: "session-1", Note: strings.Repeat("界", 501)},
		},
		{
			name: "control character in note",
			req:  catalogueRequest{SessionID: "session-1", Note: "two\nlines"},
		},
		{
			name: "more than 20 tags",
			req:  catalogueRequest{SessionID: "session-1", Tags: tags},
		},
		{
			name: "tag over 40 runes",
			req:  catalogueRequest{SessionID: "session-1", Tags: []string{strings.Repeat("界", 41)}},
		},
		{
			name: "space in tag",
			req:  catalogueRequest{SessionID: "session-1", Tags: []string{"two words"}},
		},
		{
			name: "empty session id",
			req:  catalogueRequest{},
		},
		{
			name: "quote in session id",
			req:  catalogueRequest{SessionID: `session-"1`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := installFakeEmacsclient(t, "nil")
			body, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			rec := postJSON(t, handleCatalogue, string(body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
				t.Fatalf("emacsclient should not be called; args file stat error = %v", err)
			}
		})
	}
}

func TestCatalogueHandlerRejectsPut(t *testing.T) {
	rec := httptest.NewRecorder()
	handleCatalogue(rec, httptest.NewRequest(http.MethodPut, "/api/catalogue", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d", rec.Code)
	}
}

func TestNormalizeCatalogueTags(t *testing.T) {
	got := normalizeCatalogueTags([]string{" #Syzygy ", "resume", "RESUME", "", "#"})
	want := []string{"syzygy", "resume"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %#v, want %#v", got, want)
	}
}

func TestTranscriptSearchHandlerCatalogueNarrowing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	transcripts := []transcriptInfo{
		{
			SessionID:  "older-save",
			Timestamp:  "2026-09-08-12-00-00",
			Catalogued: "2026-09-06T12:00:00Z",
			Tags:       []string{"syzygy"},
		},
		{
			SessionID: "unsaved",
			Timestamp: "2026-09-09-12-00-00",
		},
		{
			SessionID:  "newer-save",
			Timestamp:  "2026-09-01-12-00-00",
			Catalogued: "2026-09-07T12:00:00Z",
			Tags:       []string{"resume"},
		},
	}
	for i := range transcripts {
		file := filepath.Join(dir, transcripts[i].SessionID+".md")
		if err := os.WriteFile(file, []byte("ordinary transcript\n"), 0600); err != nil {
			t.Fatal(err)
		}
		transcripts[i].File = file
	}

	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
		wantIDs    []string
	}{
		{
			name:       "catalogued newest save first",
			body:       `{"catalogued":true}`,
			wantStatus: http.StatusOK,
			wantIDs:    []string{"newer-save", "older-save"},
		},
		{
			name:       "tag only",
			body:       `{"tags":["syzygy"]}`,
			wantStatus: http.StatusOK,
			wantIDs:    []string{"older-save"},
		},
		{
			name:       "empty query without narrowing",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			loader := func(_ context.Context, limit int) ([]transcriptInfo, error) {
				calls++
				if limit != 0 {
					t.Fatalf("loader limit = %d, want 0", limit)
				}
				return append([]transcriptInfo(nil), transcripts...), nil
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/transcript-search", strings.NewReader(tc.body))
			newTranscriptSearchHandler(loader).ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusBadRequest {
				if calls != 0 {
					t.Fatalf("loader calls = %d, want 0", calls)
				}
				return
			}
			if calls != 1 {
				t.Fatalf("loader calls = %d, want 1", calls)
			}
			var payload struct {
				Query     string                   `json:"query"`
				Results   []transcriptSearchResult `json:"results"`
				Truncated bool                     `json:"truncated"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Query != "" || payload.Truncated {
				t.Fatalf("unexpected response: %#v", payload)
			}
			gotIDs := make([]string, len(payload.Results))
			for i, result := range payload.Results {
				gotIDs[i] = result.SessionID
				if result.MatchField != "catalogue" {
					t.Fatalf("match field = %q, want catalogue", result.MatchField)
				}
			}
			if !reflect.DeepEqual(gotIDs, tc.wantIDs) {
				t.Fatalf("sessions = %#v, want %#v", gotIDs, tc.wantIDs)
			}
		})
	}
}
