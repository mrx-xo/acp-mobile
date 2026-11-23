package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// historyFleet is a hub with one peer for the History tests.  It records
// every History request the page makes; the hub and the peer each own one
// chat named "Claude Agent @ skate", so a lookup by buffer name alone opens
// the wrong one.
type historyFleet struct {
	mu          sync.Mutex
	listQueries []string
	searchQuery []string
	transcripts []string // raw query of each /api/transcript
	resumes     []string // body of each /api/resume-transcript
	catalogues  []string // body of each POST /api/catalogue
	wsQueries   []string
	gaps        string        // JSON array of the peer's gaps
	failOpen    bool          // the peer's transcript answers 502
	failResume  bool          // the peer's resume answers 502
	extraEntry  bool          // a third, unresumable peer entry with its own reason
	holdResume  chan struct{} // when set, the peer's resume waits for it
	held        chan struct{} // signalled when a peer resume is being held
	srv         *httptest.Server
}

func newHistoryFleet(t *testing.T, gaps string) *historyFleet {
	f := &historyFleet{gaps: gaps}
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
			`{"pid":1,"machine":"mrx","bufferName":"Claude Agent @ skate"},`+
			`{"pid":40388,"machine":"vengeance","bufferName":"Claude Agent @ skate"}],`+
			`"machine":"mrx",`+
			`"machines":[{"id":"mrx","name":"POLLUX","state":"awake","gaps":[]},`+
			`{"id":"vengeance","name":"NEMESIS","state":"awake","gaps":`+f.gaps+`}]}`)
	})
	entry := func(machine, file, session string) string {
		return fmt.Sprintf(`{"machine":%q,"file":%q,"project":"skate","timestamp":"2026-09-01-12-00-00",`+
			`"agent":"Claude","preview":"Preview of %s","sessionId":%q,"label":"","resumable":true,"resumeReason":""}`,
			machine, file, machine, session)
	}
	mux.HandleFunc("/api/transcripts", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.listQueries = append(f.listQueries, r.URL.RawQuery)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		extra := ""
		if f.extraEntry {
			extra = `,{"machine":"vengeance","file":"/t/two.md","project":"skate","timestamp":"2026-09-01-11-00-00",` +
				`"agent":"Claude","preview":"Preview two","sessionId":"s2","label":"","resumable":false,"resumeReason":"no session id recorded"}`
		}
		// The same file path on both machines: only machine plus file is unique.
		_, _ = fmt.Fprint(w, `[`+entry("mrx", "/t/one.md", "s1")+`,`+entry("vengeance", "/t/one.md", "s1")+extra+`]`)
	})
	mux.HandleFunc("/api/transcript-search", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.searchQuery = append(f.searchQuery, r.URL.RawQuery)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"results":[`+entry("mrx", "/t/one.md", "s1")+`,`+entry("vengeance", "/t/one.md", "s1")+`],"truncated":false}`)
	})
	mux.HandleFunc("/api/transcript", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.transcripts = append(f.transcripts, r.URL.RawQuery)
		fail := f.failOpen && r.URL.Query().Get("machine") == "vengeance"
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprint(w, `{"error":"NEMESIS did not answer"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"content": "**Agent:** Claude\n\n---\n\n## User (2026-09-01 12:00)\nHello there\n\n## Agent (2026-09-01 12:01)\nHi\n"})
	})
	mux.HandleFunc("/api/resume-transcript", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.resumes = append(f.resumes, string(b))
		hold, failResume := f.holdResume, f.failResume
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), `"machine":"vengeance"`) {
			if hold != nil {
				f.held <- struct{}{}
				select {
				case <-hold:
				case <-r.Context().Done():
					return
				}
			}
			if failResume {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = fmt.Fprint(w, `{"error":"NEMESIS did not answer"}`)
				return
			}
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"bufferName":"Claude Agent @ skate"}`)
	})
	mux.HandleFunc("/api/catalogue", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.catalogues = append(f.catalogues, string(b))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"sessionId":"s1","catalogued":"2026-09-02T10:00:00Z","note":"n","tags":[]}`)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.wsQueries = append(f.wsQueries, r.URL.RawQuery)
		f.mu.Unlock()
		http.Error(w, "test", http.StatusBadRequest)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *historyFleet) snapshot(list *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), (*list)...)
}

// openHistory opens the History screen and waits for both entries.
func openHistory(t *testing.T, f *historyFleet) *chromePage {
	t.Helper()
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)
	page.eval(t, `document.getElementById('nav-history-btn').click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .hist-card').length >= 2`)
	return page
}

func historyBadges(t *testing.T, page *chromePage) string {
	t.Helper()
	got, _ := page.eval(t, `JSON.stringify([...document.querySelectorAll('#history-body .hist-card')].map(c =>
		[...c.querySelectorAll('.machine-badge')].map(b => b.textContent + ':' + b.style.color)))`).(string)
	return got
}

// History lists every awake machine (fleet=1); another machine's entry
// carries its machine's badge, the hub's own entry none.
func TestHistoryBadgesRemoteEntries(t *testing.T) {
	f := newHistoryFleet(t, `[]`)
	page := openHistory(t, f)
	if got := f.snapshot(&f.listQueries); len(got) != 1 || got[0] != "fleet=1" {
		t.Fatalf("list queries = %q, want [fleet=1]", got)
	}
	// The hub entry is first; rgb(251, 73, 52) is the vengeance colour.
	if got := historyBadges(t, page); got != `[[],["NEMESIS:rgb(251, 73, 52)"]]` {
		t.Fatalf("recent badges = %s, want none on the hub row and NEMESIS on the peer row", got)
	}
	page.eval(t, `searchHistory('orbit')`)
	page.waitFor(t, `document.querySelectorAll('#history-body .hist-card').length === 2`)
	if got := f.snapshot(&f.searchQuery); len(got) != 1 || got[0] != "fleet=1" {
		t.Fatalf("search queries = %q, want [fleet=1]", got)
	}
	if got := historyBadges(t, page); got != `[[],["NEMESIS:rgb(251, 73, 52)"]]` {
		t.Fatalf("search badges = %s, want none on the hub row and NEMESIS on the peer row", got)
	}
}

// An entry opens and resumes on its own machine, and the chat that opens is
// that machine's, though the hub has a chat with the same buffer name.
func TestHistoryOpensAndResumesOnTheEntrysMachine(t *testing.T) {
	f := newHistoryFleet(t, `[]`)
	page := openHistory(t, f)

	// The hub's entry names no machine at all.
	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[0].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
	if got := f.snapshot(&f.transcripts); len(got) != 1 || got[0] != "file=%2Ft%2Fone.md" {
		t.Fatalf("hub open queries = %q, want file only", got)
	}
	page.eval(t, `document.getElementById('history-back').click()`)

	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[1].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
	if got := f.snapshot(&f.transcripts); len(got) != 2 || got[1] != "file=%2Ft%2Fone.md&machine=vengeance" {
		t.Fatalf("peer open queries = %q, want machine=vengeance on the second", got)
	}
	if got, _ := page.eval(t, `document.getElementById('history-title').querySelector('.machine-badge')?.textContent`).(string); got != "NEMESIS" {
		t.Fatalf("transcript header badge = %q, want NEMESIS", got)
	}

	page.eval(t, `document.getElementById('history-resume').click()`)
	deadline := time.Now().Add(5 * time.Second)
	var ws []string
	for time.Now().Before(deadline) {
		if ws = f.snapshot(&f.wsQueries); len(ws) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := f.snapshot(&f.resumes); len(got) != 1 || got[0] != `{"file":"/t/one.md","machine":"vengeance"}` {
		t.Fatalf("resume bodies = %q, want the file and machine=vengeance", got)
	}
	if len(ws) == 0 {
		t.Fatal("no chat was opened after the resume")
	}
	for _, q := range ws {
		if !strings.Contains(q, "sock=40388") || !strings.Contains(q, "machine=vengeance") {
			t.Fatalf("opened chat dialed /ws?%s, want the vengeance session (sock=40388, machine=vengeance)", q)
		}
	}
}

// A peer that cannot resume still shows its transcript; Resume is present,
// disabled, and says why.  The hub's own Resume stays live.
func TestHistoryResumeGapIsDisabledWithReason(t *testing.T) {
	f := newHistoryFleet(t, `["resume"]`)
	f.extraEntry = true
	page := openHistory(t, f)
	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[1].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
	got, _ := page.eval(t, `JSON.stringify({shown: document.getElementById('history-resume').style.display !== 'none',
		disabled: document.getElementById('history-resume').disabled,
		title: document.getElementById('history-resume').title})`).(string)
	if got != `{"shown":true,"disabled":true,"title":"NEMESIS cannot resume chats"}` {
		t.Fatalf("peer resume button = %s", got)
	}
	page.eval(t, `document.getElementById('history-back').click()`)
	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[0].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
	got, _ = page.eval(t, `JSON.stringify({disabled: document.getElementById('history-resume').disabled,
		title: document.getElementById('history-resume').title})`).(string)
	if got != `{"disabled":false,"title":"Resume this conversation"}` {
		t.Fatalf("hub resume button = %s, want live", got)
	}
	// The gap wins over the entry's own reason.
	page.eval(t, `document.getElementById('history-back').click()`)
	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[2].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
	got, _ = page.eval(t, `JSON.stringify({disabled: document.getElementById('history-resume').disabled,
		title: document.getElementById('history-resume').title})`).(string)
	if got != `{"disabled":true,"title":"NEMESIS cannot resume chats"}` {
		t.Fatalf("unresumable peer entry button = %s, want the gap reason, not the entry's", got)
	}
}

// A catalogue change for a chat on one machine patches only that machine's
// rows, though another machine's row has the same session id.
func TestHistoryCatalogueChipIsPerMachine(t *testing.T) {
	f := newHistoryFleet(t, `[]`)
	page := openHistory(t, f)
	page.eval(t, `saveCatalogue('s1', 'n', [], 'vengeance').then(renderHistoryRoot)`)
	page.waitFor(t, `document.querySelectorAll('#history-body .hc-kept').length > 0`)
	got, _ := page.eval(t, `JSON.stringify([...document.querySelectorAll('#history-body .hist-card')].map(c => !!c.querySelector('.hc-kept')))`).(string)
	if got != `[false,true]` {
		t.Fatalf("kept chips = %s, want only the NEMESIS row", got)
	}
	if b := f.snapshot(&f.catalogues); len(b) != 1 || !strings.Contains(b[0], `"machine":"vengeance"`) {
		t.Fatalf("catalogue bodies = %q, want machine=vengeance", b)
	}
}

// An unreachable machine shows the server's own message and leaves the
// list usable.
func TestHistoryRemoteOpenFailureShowsServerError(t *testing.T) {
	f := newHistoryFleet(t, `[]`)
	f.failOpen = true
	page := openHistory(t, f)
	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[1].click()`)
	page.waitFor(t, `document.querySelector('#history-body .hist-empty')?.textContent.includes('NEMESIS did not answer')`)
	if got, _ := page.eval(t, `document.querySelector('#history-body .hist-empty').textContent`).(string); strings.Contains(got, "{") {
		t.Fatalf("error text = %q, want the message without raw JSON", got)
	}
	page.eval(t, `document.getElementById('history-back').click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .hist-card').length === 2`)
}

// A failed resume on another machine shows the server's message, re-enables
// Resume, and leaves the list usable.
func TestHistoryRemoteResumeFailureLeavesHistoryUsable(t *testing.T) {
	f := newHistoryFleet(t, `[]`)
	f.failResume = true
	page := openHistory(t, f)
	page.eval(t, `window.__alerts = []; window.alert = m => window.__alerts.push(m)`)
	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[1].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
	page.eval(t, `document.getElementById('history-resume').click()`)
	page.waitFor(t, `window.__alerts.length === 1`)
	if got, _ := page.eval(t, `window.__alerts[0]`).(string); got != "Resume failed: NEMESIS did not answer" {
		t.Fatalf("alert = %q, want the server's message", got)
	}
	page.waitFor(t, `!document.getElementById('history-resume').disabled && document.getElementById('history-resume').textContent === 'resume'`)
	page.eval(t, `document.getElementById('history-back').click()`)
	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[0].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
}

// Two resumes of entries that share a file path but live on different
// machines do not cross: the held NEMESIS reply, arriving after the user
// moved on and resumed the hub entry, opens nothing and changes nothing.
func TestHistoryTwoResumesDoNotCrossEntries(t *testing.T) {
	f := newHistoryFleet(t, `[]`)
	release := make(chan struct{})
	f.holdResume = release
	f.held = make(chan struct{}, 4)
	var once sync.Once
	releaseHeld := func() { once.Do(func() { close(release) }) }
	// Registered after the server's cleanup, so it runs before the server closes.
	t.Cleanup(releaseHeld)
	page := openHistory(t, f)

	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[1].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
	page.eval(t, `document.getElementById('history-resume').click()`)
	select {
	case <-f.held:
	case <-time.After(5 * time.Second):
		t.Fatal("the NEMESIS resume never reached the server")
	}
	page.eval(t, `document.getElementById('history-back').click()`)
	page.eval(t, `document.querySelectorAll('#history-body .hist-card')[0].click()`)
	page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
	page.eval(t, `document.getElementById('history-resume').click()`)
	waitWS := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, q := range f.snapshot(&f.wsQueries) {
				if strings.Contains(q, want) {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("no /ws dial with %s; saw %q", want, f.snapshot(&f.wsQueries))
	}
	waitWS("sock=1")
	releaseHeld()
	time.Sleep(1500 * time.Millisecond) // time for a wrongly applied late reply
	for _, q := range f.snapshot(&f.wsQueries) {
		if strings.Contains(q, "sock=40388") || strings.Contains(q, "machine=vengeance") {
			t.Fatalf("the late NEMESIS reply opened a chat: /ws?%s", q)
		}
	}
	if got := f.snapshot(&f.resumes); len(got) != 2 || got[0] != `{"file":"/t/one.md","machine":"vengeance"}` || got[1] != `{"file":"/t/one.md"}` {
		t.Fatalf("resume bodies = %q, want the peer's then the hub's, each on its own machine", got)
	}
	if n, _ := page.eval(t, `historyResuming.size`).(float64); n != 0 {
		t.Fatalf("resumes still tracked = %v, want 0", n)
	}
}

// A resume left behind by navigation does not block resuming the same entry
// again: the new one supersedes it, and only the new one opens the chat.
func TestHistoryResumeSupersedesLeftBehindResume(t *testing.T) {
	f := newHistoryFleet(t, `[]`)
	release := make(chan struct{})
	f.holdResume = release
	f.held = make(chan struct{}, 4)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	page := openHistory(t, f)
	for i := 0; i < 2; i++ {
		page.eval(t, `document.querySelectorAll('#history-body .hist-card')[1].click()`)
		page.waitFor(t, `document.querySelectorAll('#history-body .pv-msg').length === 2`)
		page.eval(t, `document.getElementById('history-resume').click()`)
		select {
		case <-f.held:
		case <-time.After(5 * time.Second):
			t.Fatalf("resume %d never reached the server", i+1)
		}
		if i == 0 {
			page.eval(t, `document.getElementById('history-back').click()`)
		}
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(5 * time.Second)
	for len(f.snapshot(&f.wsQueries)) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got := f.snapshot(&f.resumes); len(got) != 2 {
		t.Fatalf("resume bodies = %q, want both attempts sent", got)
	}
	if len(f.snapshot(&f.wsQueries)) == 0 {
		t.Fatal("the second resume did not open the chat")
	}
}
