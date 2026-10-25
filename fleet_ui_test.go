package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A hub with one peer: the remote session's card carries the peer's
// mythology name, and opening it dials /ws with that machine so the hub
// proxies; the hub's own session dials without one.
func TestFleetBadgeAndRemoteConnect(t *testing.T) {
	queries := make(chan string, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"sessions":[`+
			`{"pid":1,"machine":"mrx","bufferName":"Claude Agent @ dotfiles"},`+
			`{"pid":40388,"machine":"vengeance","bufferName":"Claude Agent @ dotfiles"}],`+
			`"machine":"mrx",`+
			`"machines":[{"id":"mrx","name":"POLLUX","state":"awake"},{"id":"vengeance","name":"NEMESIS","state":"awake"}]}`)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery
		http.Error(w, "test", http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	nextQuery := func() string {
		t.Helper()
		select {
		case q := <-queries:
			return q
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a /ws dial")
			return ""
		}
	}

	page := openChromePage(t, srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)

	badges, _ := page.eval(t, `JSON.stringify([...document.querySelectorAll('.machine-badge')].map(b => b.textContent))`).(string)
	if badges != `["NEMESIS"]` {
		t.Fatalf("machine badges = %s, want exactly one NEMESIS", badges)
	}

	page.eval(t, `document.querySelectorAll('.session-card')[1].click()`)
	q := nextQuery()
	if !strings.Contains(q, "machine=vengeance") || !strings.Contains(q, "sock=40388") {
		t.Fatalf("remote card dialed /ws?%s, want sock=40388 and machine=vengeance", q)
	}

	// The failed remote socket may retry; skip its dials and take the
	// first dial for the hub's own session.
	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'mrx'))`)
	for q = nextQuery(); strings.Contains(q, "sock=40388"); q = nextQuery() {
	}
	if !strings.Contains(q, "sock=1") || strings.Contains(q, "machine=") {
		t.Fatalf("local card dialed /ws?%s, want sock=1 with no machine", q)
	}
}
