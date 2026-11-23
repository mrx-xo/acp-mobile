package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// historyHome gives the test a HOME with one eligible transcript file per
// name and returns the loader that indexes them with the given timestamps.
func historyHome(t *testing.T, entries ...transcriptInfo) transcriptIndexLoader {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	out := make([]transcriptInfo, len(entries))
	for i, e := range entries {
		e.File = filepath.Join(dir, e.SessionID+".md")
		if err := os.WriteFile(e.File, []byte("body\n"), 0600); err != nil {
			t.Fatal(err)
		}
		out[i] = e
	}
	return func(context.Context, int) ([]transcriptInfo, error) { return out, nil }
}

func postHistory(h http.Handler, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)))
	return rec
}

func transcriptKeys(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got []transcriptInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	var keys []string
	for _, e := range got {
		keys = append(keys, e.Machine+":"+e.SessionID)
	}
	return keys
}

func joinKeys(k []string) string { return strings.Join(k, " ") }

func TestMergedTranscriptsTagAndSortByTimestamp(t *testing.T) {
	load := historyHome(t,
		transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"},
		transcriptInfo{SessionID: "h2", Timestamp: "2026-11-03-10-00-00"})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(peerHeader) != "1" || r.Method != http.MethodPost {
			t.Errorf("peer got %s with header %q", r.Method, r.Header.Get(peerHeader))
		}
		w.Write([]byte(`[{"file":"/p/p1.md","sessionId":"p1","timestamp":"2026-11-02-10-00-00"},{"file":"/p/p2.md","sessionId":"p2","timestamp":"2026-11-04-10-00-00"}]`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	got := joinKeys(transcriptKeys(t, postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", `{}`)))
	if want := "vengeance:p2 mrx:h2 vengeance:p1 mrx:h1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMergedTranscriptsSkipAFailingPeer(t *testing.T) {
	logs := captureLog(t)
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	got := joinKeys(transcriptKeys(t, postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", `{}`)))
	if got != "mrx:h1" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(logs.String(), "fleet: /api/transcripts on vengeance") {
		t.Errorf("log %q does not record the failing peer", logs.String())
	}
}

func TestMergedTranscriptsLeaveOutAPeerThatAnswersNotJSON(t *testing.T) {
	logs := captureLog(t)
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>not json</html>`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	got := joinKeys(transcriptKeys(t, postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", `{}`)))
	if got != "mrx:h1" || !strings.Contains(logs.String(), "fleet: /api/transcripts on vengeance:") {
		t.Fatalf("got %q, log %q", got, logs.String())
	}
}

func TestMergedTranscriptsSkipMachinesWithHistoryGap(t *testing.T) {
	for _, path := range []string{"/api/transcripts?fleet=1", "/api/transcript-search?fleet=1"} {
		t.Run(path, func(t *testing.T) {
			load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Write([]byte(`[]`))
			}))
			defer peer.Close()
			m := peerAt(t, "vengeance", "NEMESIS", peer.URL)
			m.Gaps = []string{"history"}
			withFleetFixture(t, "mrx", m)
			if strings.Contains(path, "search") {
				postHistory(newTranscriptSearchHandler(load), path, `{"query":"body"}`)
			} else {
				postHistory(newTranscriptsHandler(load), path, `{}`)
			}
			if calls.Load() != 0 {
				t.Fatalf("peer with a history gap was called %d times", calls.Load())
			}
		})
	}
}

func TestMergedTranscriptsSurviveALocalFailure(t *testing.T) {
	logs := captureLog(t)
	load := func(context.Context, int) ([]transcriptInfo, error) { return nil, errors.New("emacs down") }
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"file":"/p/p1.md","sessionId":"p1","timestamp":"2026-11-02-10-00-00"}]`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	got := joinKeys(transcriptKeys(t, postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", `{}`)))
	if got != "vengeance:p1" || !strings.Contains(logs.String(), "transcripts: emacs down; using other machines") {
		t.Fatalf("got %q, log %q", got, logs.String())
	}
}

func TestMergedTranscriptsErrorWhenNoMachineHasAny(t *testing.T) {
	captureLog(t)
	load := func(context.Context, int) ([]transcriptInfo, error) { return nil, errors.New("emacs down") }
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestTranscriptsServeLocalOnlyToPeers(t *testing.T) {
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`[{"file":"/p/p1.md","sessionId":"p1"}]`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := httptest.NewRecorder()
	newTranscriptsHandler(load).ServeHTTP(rec, withPeerFlag(httptest.NewRequest(http.MethodPost, "/api/transcripts?fleet=1", strings.NewReader(`{}`))))
	if got := joinKeys(transcriptKeys(t, rec)); got != "mrx:h1" || calls.Load() != 0 {
		t.Fatalf("got %q, peer calls %d", got, calls.Load())
	}
}

func TestTranscriptsWithoutFleetFlagStayLocal(t *testing.T) {
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`[{"file":"/p/p1.md","sessionId":"p1"}]`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postHistory(newTranscriptsHandler(load), "/api/transcripts", `{}`)
	if got := joinKeys(transcriptKeys(t, rec)); got != "mrx:h1" || calls.Load() != 0 {
		t.Fatalf("list: got %q, peer calls %d", got, calls.Load())
	}
	rec = postHistory(newTranscriptSearchHandler(load), "/api/transcript-search", `{"query":"body"}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"vengeance"`) || calls.Load() != 0 {
		t.Fatalf("search: status %d body %s peer calls %d", rec.Code, rec.Body.String(), calls.Load())
	}
}

func TestTranscriptSearchFansOutTheSameQuery(t *testing.T) {
	// Every local entry matches the query "body" through its preview; the
	// peer's results carry their own counts.
	var local []transcriptInfo
	for i := 0; i < 30; i++ {
		local = append(local, transcriptInfo{SessionID: "h" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Timestamp: "2026-10-01-10-00-00", Preview: "body of chat"})
	}
	load := historyHome(t, local...)
	var gotBody, gotQuery atomic.Value
	var peerResults []string
	for i := 0; i < 20; i++ {
		peerResults = append(peerResults, `{"file":"/p/`+string(rune('a'+i))+`.md","sessionId":"p`+string(rune('a'+i))+`","timestamp":"2026-11-`+fmt.Sprintf("%02d", 1+i)+`-10-00-00","matchCount":5,"matchField":"body","snippet":"s"}`)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		gotQuery.Store(r.URL.RawQuery)
		w.Write([]byte(`{"query":"body","truncated":false,"results":[` + strings.Join(peerResults, ",") + `]}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	const sent = `{"query":"body",  "tags":[]}`
	rec := postHistory(newTranscriptSearchHandler(load), "/api/transcript-search?fleet=1&scope=all", sent)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if gotBody.Load() != sent {
		t.Fatalf("peer body = %q, want the hub's %q", gotBody.Load(), sent)
	}
	if gotQuery.Load() != "scope=all" {
		t.Fatalf("peer query = %q, want only scope=all", gotQuery.Load())
	}
	var reply struct {
		Results   []transcriptSearchResult `json:"results"`
		Truncated bool                     `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Results) != transcriptSearchLimit || !reply.Truncated {
		t.Fatalf("got %d results, truncated %v; want %d capped", len(reply.Results), reply.Truncated, transcriptSearchLimit)
	}
	// Peer entries (matchCount 5) outrank the hub's (1 each).
	for i, r := range reply.Results[:20] {
		if r.Machine != "vengeance" || r.MatchCount != 5 {
			t.Fatalf("result %d = %s %d, want the peer's high-count entries first", i, r.Machine, r.MatchCount)
		}
	}
	for i := 1; i < 20; i++ {
		if reply.Results[i-1].Timestamp < reply.Results[i].Timestamp {
			t.Fatalf("equal counts not newest first: %s before %s", reply.Results[i-1].Timestamp, reply.Results[i].Timestamp)
		}
	}
	for _, r := range reply.Results[20:] {
		if r.Machine != "mrx" {
			t.Fatalf("tail has %s, want mrx", r.Machine)
		}
	}
}

func TestTranscriptSearchNamingAnotherMachineIsForwarded(t *testing.T) {
	// The hub's own index would fail loudly if it were used.
	load := func(context.Context, int) ([]transcriptInfo, error) { return nil, errors.New("hub index used") }
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"query":"body","results":[],"truncated":false}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postHistory(fleetRoute(newTranscriptSearchHandler(load).ServeHTTP), "/api/transcript-search?machine=vengeance&fleet=1", `{"query":"body"}`)
	if rec.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status %d body %s peer calls %d", rec.Code, rec.Body.String(), calls.Load())
	}
}

func TestWantsFleet(t *testing.T) {
	peerA := machine{ID: "vengeance", Name: "NEMESIS"}
	cases := []struct {
		name   string
		target string
		peers  []machine
		isPeer bool
		want   bool
	}{
		{"no flag", "/?", []machine{peerA}, false, false},
		{"flag on a hub with peers", "/?fleet=1", []machine{peerA}, false, true},
		{"flag on a node (no registry peers)", "/?fleet=1", nil, false, false},
		{"flag on a hub with no peers", "/?fleet=1", []machine{}, false, false},
		{"flag on a peer request", "/?fleet=1", []machine{peerA}, true, false},
		{"flag with a machine named", "/?fleet=1&machine=mrx", []machine{peerA}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withFleetFixture(t, "mrx", c.peers...)
			r := httptest.NewRequest(http.MethodPost, c.target, nil)
			if c.isPeer {
				r = withPeerFlag(r)
			}
			if got := wantsFleet(r); got != c.want {
				t.Fatalf("wantsFleet = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMergedTranscriptSearchSurvivesALocalFailure(t *testing.T) {
	logs := captureLog(t)
	load := func(context.Context, int) ([]transcriptInfo, error) { return nil, errors.New("emacs down") }
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"file":"/p/p1.md","sessionId":"p1","timestamp":"2026-11-02-10-00-00","matchCount":1}],"truncated":false}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postHistory(newTranscriptSearchHandler(load), "/api/transcript-search?fleet=1", `{"query":"body"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"vengeance"`) {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "transcripts: emacs down; using other machines") {
		t.Fatalf("log %q lacks the underlying local error", logs.String())
	}
}

func TestMergedTranscriptsPassBodyAndQueryToPeers(t *testing.T) {
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
	var gotBody, gotQuery atomic.Value
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		gotQuery.Store(r.URL.RawQuery)
		w.Write([]byte(`[]`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1&limit=5", `{"x":1}`)
	if gotQuery.Load() != "limit=5" || gotBody.Load() != `{"x":1}` {
		t.Fatalf("peer saw query %q body %q", gotQuery.Load(), gotBody.Load())
	}
}

func TestMergedTranscriptsRejectAPeerReplyWithoutTheList(t *testing.T) {
	for name, reply := range map[string]string{"error object": `{"error":"unavailable"}`, "null": `null`} {
		t.Run(name, func(t *testing.T) {
			logs := captureLog(t)
			load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(reply)) }))
			defer peer.Close()
			withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
			got := joinKeys(transcriptKeys(t, postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", `{}`)))
			if got != "mrx:h1" || !strings.Contains(logs.String(), "fleet: /api/transcripts on vengeance: ") {
				t.Fatalf("got %q, log %q", got, logs.String())
			}
		})
	}
}

func TestMergedTranscriptsDropEntriesWithoutAFile(t *testing.T) {
	logs := captureLog(t)
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[null,{"file":"/p/p1.md","sessionId":"p1","timestamp":"2026-11-02-10-00-00"}]`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	got := joinKeys(transcriptKeys(t, postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", `{}`)))
	if got != "vengeance:p1 mrx:h1" || !strings.Contains(logs.String(), "fleet: /api/transcripts on vengeance: dropped 1 entries without a file") {
		t.Fatalf("got %q, log %q", got, logs.String())
	}
}

func TestMergedTranscriptSearchRejectsAReplyWithoutResults(t *testing.T) {
	logs := captureLog(t)
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00", Preview: "body of chat"})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postHistory(newTranscriptSearchHandler(load), "/api/transcript-search?fleet=1", `{"query":"body"}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"vengeance"`) || !strings.Contains(rec.Body.String(), `"mrx"`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "fleet: /api/transcript-search on vengeance: reply has no results list") {
		t.Fatalf("log %q", logs.String())
	}
}

func TestMergedTranscriptSearchDropsResultsWithoutAFile(t *testing.T) {
	logs := captureLog(t)
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00", Preview: "body of chat"})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[null,{"file":"/p/p1.md","sessionId":"p1","timestamp":"2026-11-02-10-00-00","matchCount":9}],"truncated":false}`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postHistory(newTranscriptSearchHandler(load), "/api/transcript-search?fleet=1", `{"query":"body"}`)
	var reply struct {
		Results []transcriptSearchResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Results) != 2 || reply.Results[0].SessionID != "p1" || reply.Results[0].Machine != "vengeance" ||
		!strings.Contains(logs.String(), "dropped 1 results without a file") {
		t.Fatalf("body %s log %q", rec.Body.String(), logs.String())
	}
}

func TestTranscriptsRefuseAnOversizedBody(t *testing.T) {
	var calls atomic.Int32
	loaded := false
	load := func(context.Context, int) ([]transcriptInfo, error) { loaded = true; return nil, nil }
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`[]`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", strings.Repeat("a", transcriptsBodyLimit+1))
	if rec.Code != http.StatusRequestEntityTooLarge || strings.TrimSpace(rec.Body.String()) != `{"error":"request body too large"}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 || loaded {
		t.Fatalf("peer calls %d, local lookup ran %v", calls.Load(), loaded)
	}
}

func TestTranscriptsFanOutABodyAtTheCapWhole(t *testing.T) {
	load := historyHome(t, transcriptInfo{SessionID: "h1", Timestamp: "2026-11-01-10-00-00"})
	sent := strings.Repeat("a", transcriptsBodyLimit)
	var got atomic.Value
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(string(b))
		w.Write([]byte(`[]`))
	}))
	defer peer.Close()
	withFleetFixture(t, "mrx", peerAt(t, "vengeance", "NEMESIS", peer.URL))
	rec := postHistory(newTranscriptsHandler(load), "/api/transcripts?fleet=1", sent)
	if rec.Code != http.StatusOK || got.Load() != sent {
		t.Fatalf("status %d, peer got %d bytes, want %d", rec.Code, len(got.Load().(string)), len(sent))
	}
}
