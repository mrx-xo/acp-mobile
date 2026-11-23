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

// A remote session keeps every chat action: the card keeps its delete
// button, Fork, kill, clone, pin, model and label stay live, and the
// requests name the owning machine (the full list is in
// TestFleetRemoteActionsNameTheirMachine).  The hub's own session is
// unchanged.
func TestFleetRemoteSessionKeepsItsActions(t *testing.T) {
	var killMu sync.Mutex
	var kills, forks []string
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
			`{"pid":40388,"machine":"vengeance","bufferName":"Claude Agent @ skate"}],`+
			`"machine":"mrx",`+
			`"machines":[{"id":"mrx","name":"POLLUX","state":"awake"},{"id":"vengeance","name":"NEMESIS","state":"awake"}]}`)
	})
	mux.HandleFunc("/api/kill", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		killMu.Lock()
		kills = append(kills, string(b))
		killMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/api/fork", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			killMu.Lock()
			forks = append(forks, "probe "+r.URL.Query().Get("machine")+" "+r.URL.Query().Get("bufferName"))
			killMu.Unlock()
			_, _ = fmt.Fprint(w, `{"ok":true,"supported":true}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		killMu.Lock()
		forks = append(forks, string(b))
		killMu.Unlock()
		_, _ = fmt.Fprint(w, `{"ok":true,"bufferName":"Claude Agent @ skate<2>"}`)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "test", http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	page := openChromePage(t, srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)

	cards, _ := page.eval(t, `JSON.stringify([...document.querySelectorAll('.session-card')].map(c => ({
		remote: !!c.querySelector('.machine-badge'),
		del: !!c.querySelector('.sc-delete')})).sort((a, b) => a.remote - b.remote))`).(string)
	if cards != `[{"remote":false,"del":true},{"remote":true,"del":true}]` {
		t.Fatalf("cards = %s, want a delete button on both cards", cards)
	}
	killed := func() []string {
		killMu.Lock()
		defer killMu.Unlock()
		return append([]string(nil), kills...)
	}
	const remoteKill = `{"bufferName":"Claude Agent @ skate","machine":"vengeance"}`
	page.eval(t, `window.confirm = () => true; window.alert = () => {};
		[...document.querySelectorAll('.session-card')].find(c => c.querySelector('.machine-badge')).querySelector('.sc-delete').click(); 0`)
	page.eval(t, `new Promise(r => setTimeout(() => r(0), 300))`)
	if got := killed(); len(got) != 1 || got[0] != remoteKill {
		t.Fatalf("remote card delete sent %q, want one %s", got, remoteKill)
	}

	const controls = `JSON.stringify(['tb-kill','tb-fork','tb-clone','tb-pin','tb-model'].map(id => {
		const b = document.getElementById(id); return id + ':' + b.disabled + ':' + (b.title || '');
	}).concat(['bell:' + document.getElementById('bell-btn').classList.contains('visible'),
		'label:' + (document.getElementById('header-title').title || '')]))`
	page.eval(t, `setToolbarOpen(true); selectSession(lastSessions.find(s => s.machine === 'vengeance'))`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ skate'`)
	page.waitFor(t, `!document.getElementById('tb-fork').disabled`)
	got, _ := page.eval(t, controls).(string)
	want := `["tb-kill:false:","tb-fork:false:","tb-clone:false:","tb-pin:false:Pin this chat","tb-model:false:","bell:true","label:"]`
	if got != want {
		t.Fatalf("remote chat controls =\n%s\nwant\n%s", got, want)
	}
	page.eval(t, `window.confirm = () => true; window.alert = () => {}; forkSession(); 0`)
	page.eval(t, `new Promise(r => setTimeout(r, 300))`)
	// Fork is forwarded: the probe and the fork both name the owning machine.
	killMu.Lock()
	gotForks := append([]string(nil), forks...)
	killMu.Unlock()
	// The toolbar may probe more than once; exactly one fork follows.
	const wantProbe, wantFork = "probe vengeance Claude Agent @ skate", `{"bufferName":"Claude Agent @ skate","machine":"vengeance"}`
	if n := len(gotForks); n < 2 || gotForks[n-1] != wantFork {
		t.Fatalf("remote fork sent %q, want probes then %s", gotForks, wantFork)
	}
	for _, f := range gotForks[:len(gotForks)-1] {
		if f != wantProbe {
			t.Fatalf("remote fork sent %q, want only %q before the fork", gotForks, wantProbe)
		}
	}
	// The in-chat kill names the owning machine too.
	page.eval(t, `killSession(); 0`)
	page.eval(t, `new Promise(r => setTimeout(() => r(0), 300))`)
	if got := killed(); len(got) != 2 || got[1] != remoteKill {
		t.Fatalf("in-chat remote kill sent %q, want a second %s", got, remoteKill)
	}

	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'mrx'))`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ dotfiles'`)
	local, _ := page.eval(t, `JSON.stringify(['tb-kill','tb-clone','tb-pin','tb-model'].map(id => {
		const b = document.getElementById(id); return id + ':' + b.disabled;
	}).concat(['bell:' + document.getElementById('bell-btn').classList.contains('visible')]))`).(string)
	if local != `["tb-kill:false","tb-clone:false","tb-pin:false","tb-model:false","bell:true"]` {
		t.Fatalf("local chat controls = %s, want every action", local)
	}
}

// fleetRequest is one request the fleet-actions fixture server saw.
type fleetRequest struct {
	Method, Path, Query, Body string
}

// names reports whether the request carries machine=<id>, in the query
// (GET) or as a "machine" field of the JSON body (POST).
func (r fleetRequest) names(machine string) bool {
	if r.Query != "" {
		for _, kv := range strings.Split(r.Query, "&") {
			if kv == "machine="+machine {
				return true
			}
		}
	}
	var body map[string]any
	if json.Unmarshal([]byte(r.Body), &body) == nil {
		if m, ok := body["machine"].(string); ok && m == machine {
			return true
		}
	}
	return false
}

// carriesMachine reports any machine at all, in either place.
func (r fleetRequest) carriesMachine() bool {
	if strings.Contains(r.Query, "machine=") {
		return true
	}
	var body map[string]any
	if json.Unmarshal([]byte(r.Body), &body) == nil {
		_, ok := body["machine"]
		return ok
	}
	return false
}

// fleetFixture serves index.html and stubs every chat-action route,
// recording what the page sends.  Both machines own a chat with the same
// buffer name.  pins maps a machine query ("" is the hub) to its reply.
type fleetFixture struct {
	srv      *httptest.Server
	mu       sync.Mutex
	reqs     []fleetRequest
	pinReply map[string]string
	pinFail  map[string]bool
	sessions string            // overrides the default session list
	spawn    string            // overrides the /api/spawn reply
	status   int               // overrides the /api/spawn status
	catalog  map[string]string // /api/catalogue reply by machine query
}

func newFleetFixture(t *testing.T, machinesJSON string) *fleetFixture {
	t.Helper()
	f := &fleetFixture{pinReply: map[string]string{}, pinFail: map[string]bool{}}
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
		f.mu.Lock()
		custom := f.sessions
		f.mu.Unlock()
		if custom != "" {
			_, _ = fmt.Fprint(w, `{"sessions":[`+custom+`],"machine":"mrx","machines":`+machinesJSON+`}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"sessions":[`+
			`{"pid":1,"machine":"mrx","sessionId":"sess-hub","bufferName":"Claude Agent @ shared"},`+
			`{"pid":40388,"machine":"vengeance","sessionId":"sess-remote","bufferName":"Claude Agent @ shared"}],`+
			`"machine":"mrx","machines":`+machinesJSON+`}`)
	})
	record := func(w http.ResponseWriter, r *http.Request, reply string) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, fleetRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, reply)
	}
	replies := map[string]string{
		"/api/label":       `{"ok":true}`,
		"/api/fork":        `{"ok":true,"supported":true,"bufferName":"Claude Agent @ shared<2>"}`,
		"/api/spawn":       `{"ok":true,"bufferName":"Claude Agent @ shared<3>"}`,
		"/api/models":      `{"current":"m0","models":[{"id":"m1","name":"M1"}]}`,
		"/api/model":       `{"ok":true,"current":"m1"}`,
		"/api/catalogue":   `{"sessionId":"sess-same","catalogued":"yes","note":"saved","tags":[],"allTags":[]}`,
		"/api/preview":     `{"messages":[]}`,
		"/api/git-status":  `{"branch":"main","staged":0,"unstaged":0,"untracked":0}`,
		"/api/diff-review": `{"available":true,"files":[]}`,
		"/files/list":      `{"path":"/work","files":[]}`,
		"/files/read":      `{"content":"x"}`,
		"/api/push":        `{"ok":true,"push":true}`,
	}
	for p, reply := range replies {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			reply := reply // request-local: handlers run concurrently
			f.mu.Lock()
			spawn, status, cat := f.spawn, f.status, f.catalog[r.URL.Query().Get("machine")]
			f.mu.Unlock()
			if p == "/api/spawn" && spawn != "" {
				reply = spawn
				if status != 0 {
					b, _ := io.ReadAll(r.Body)
					f.mu.Lock()
					f.reqs = append(f.reqs, fleetRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
					f.mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, reply)
					return
				}
			}
			if p == "/api/catalogue" && r.Method == http.MethodGet && cat != "" {
				reply = cat
			}
			record(w, r, reply)
		})
	}
	mux.HandleFunc("/api/pin", func(w http.ResponseWriter, r *http.Request) {
		m := r.URL.Query().Get("machine")
		f.mu.Lock()
		fail, reply := f.pinFail[m], f.pinReply[m]
		f.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if reply == "" {
			reply = `{"pins":[]}`
		}
		if r.Method == http.MethodPost {
			reply = `{"pinned":true,"pins":["Claude Agent @ shared"]}`
		}
		record(w, r, reply)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "test", http.StatusBadRequest)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fleetFixture) requests() []fleetRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fleetRequest(nil), f.reqs...)
}

const fleetTwoMachines = `[{"id":"mrx","name":"POLLUX","state":"awake","gaps":[]},` +
	`{"id":"vengeance","name":"NEMESIS","state":"awake","gaps":[]}]`

// driveFleetActions opens the chat owned by machine and triggers every
// chat action once, returning what the hub was asked.
func driveFleetActions(t *testing.T, machine string) []fleetRequest {
	f := newFleetFixture(t, fleetTwoMachines)
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)
	page.eval(t, `SPAWN_POLL_MS = 5; window.confirm = () => true; window.prompt = () => 'x'; window.alert = () => {};
		setToolbarOpen(true); selectSession(lastSessions.find(s => s.machine === '`+machine+`')); 0`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ shared'`)
	page.eval(t, `(async () => {
		const sleep = ms => new Promise(r => setTimeout(r, ms));
		await sleep(300);
		const s = lastSessions.find(x => x.machine === '`+machine+`');
		await fetchHeaderModel();
		document.getElementById('header-title').click(); await sleep(100);
		await openModelPicker(); await sleep(100);
		await chooseModel('m1'); await sleep(100);
		document.getElementById('tb-pin').click(); await sleep(100);
		document.getElementById('tb-catalogue').click(); await sleep(200);
		await showPreview(s, 'x');
		openDiffReview('repository'); await sleep(100);
		projectCwd = '/work'; await openFileBrowser(); await fbOpenFile('/work/a'); await sleep(100);
		forkSession(); await sleep(300);
		cloneSession(); await sleep(300);
		return 0;
	})()`)
	return f.requests()
}

// Every action about a chat names the machine that owns it; the hub's
// own chats send no machine at all.
func TestFleetRemoteActionsNameTheirMachine(t *testing.T) {
	wantPaths := []string{"/api/label", "/api/fork", "/api/spawn", "/api/models", "/api/model", "/api/pin",
		"/api/catalogue", "/api/preview", "/api/git-status", "/api/diff-review", "/files/list", "/files/read"}
	for _, owner := range []string{"vengeance", "mrx"} {
		t.Run(owner, func(t *testing.T) {
			seen := map[string]int{}
			for _, r := range driveFleetActions(t, owner) {
				if r.Path == "/api/pin" && r.Method == http.MethodGet {
					continue // the list load asks each machine on its own
				}
				seen[r.Path]++
				if owner == "vengeance" && !r.names("vengeance") {
					t.Errorf("remote %s %s ?%s body=%s does not name vengeance", r.Method, r.Path, r.Query, r.Body)
				}
				if owner == "mrx" && r.carriesMachine() {
					t.Errorf("hub %s %s ?%s body=%s carries a machine", r.Method, r.Path, r.Query, r.Body)
				}
			}
			for _, p := range wantPaths {
				if seen[p] == 0 {
					t.Errorf("%s was never requested; the action did not run", p)
				}
			}
		})
	}
}

// The bell stays off for a remote chat until fleet step 4, and says so.
func TestFleetRemoteBellStaysOffUntilStep4(t *testing.T) {
	f := newFleetFixture(t, fleetTwoMachines)
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)
	page.eval(t, `window.alert = () => {}; selectSession(lastSessions.find(s => s.machine === 'vengeance')); 0`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ shared'`)
	got, _ := page.eval(t, `(async () => {
		const b = document.getElementById('bell-btn');
		b.dispatchEvent(new Event('click')); b.click();
		await new Promise(r => setTimeout(r, 200));
		return b.disabled + '|' + b.title + '|' + b.classList.contains('visible');
	})()`).(string)
	if want := `true|Push for remote chats arrives with fleet step 4|true`; got != want {
		t.Fatalf("remote bell = %s, want %s", got, want)
	}
	for _, r := range f.requests() {
		if r.Path == "/api/push" {
			t.Fatalf("remote chat called /api/push: %+v", r)
		}
	}
	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'mrx')); 0`)
	got, _ = page.eval(t, `(async () => {
		await new Promise(r => setTimeout(r, 100));
		const b = document.getElementById('bell-btn');
		return b.disabled + '|' + b.title;
	})()`).(string)
	if want := `false|Phone push for this chat`; got != want {
		t.Fatalf("hub bell = %s, want %s", got, want)
	}
}

// A gap a machine declares disables its control, with the reason, and
// the control comes back untouched on a chat without the gap.
func TestFleetDeclaredGapDisablesItsControl(t *testing.T) {
	f := newFleetFixture(t, `[{"id":"mrx","name":"POLLUX","state":"awake","gaps":["push"]},`+
		`{"id":"vengeance","name":"NEMESIS","state":"awake","gaps":["diff-review","files"]}]`)
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)
	helpers, _ := page.eval(t, `[machineGap('vengeance', 'files'), machineGap('vengeance', 'push') === '',
		machineGap('nope', 'files') === '', machineGap('', 'push'), machineGap('mrx', 'push')].join('|')`).(string)
	if want := `NEMESIS cannot browse files|true|true|POLLUX cannot send push notifications|POLLUX cannot send push notifications`; helpers != want {
		t.Fatalf("machineGap = %s, want %s", helpers, want)
	}
	const state = `(async () => {
		await new Promise(r => setTimeout(r, 150));
		const out = [];
		for (const id of ['tb-git', 'hs-git', 'files-btn', 'bell-btn']) {
			const b = document.getElementById(id);
			out.push(id + ':' + (b ? b.disabled + ':' + b.title + ':' + (b.getAttribute('aria-label') || '') : 'missing'));
		}
		return out.join('\n');
	})()`
	page.eval(t, `setToolbarOpen(true); selectSession(lastSessions.find(s => s.machine === 'vengeance')); 0`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ shared'`)
	got, _ := page.eval(t, state).(string)
	const remote = "tb-git:true:NEMESIS cannot review diffs:\n" +
		"hs-git:true:NEMESIS cannot review diffs:Review repository changes: NEMESIS cannot review diffs\n" +
		"files-btn:true:NEMESIS cannot browse files:\n" +
		"bell-btn:true:Push for remote chats arrives with fleet step 4:Phone push"
	if got != remote {
		t.Fatalf("remote chat controls =\n%s\nwant\n%s", got, remote)
	}
	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'mrx')); 0`)
	got, _ = page.eval(t, state).(string)
	const hub = "tb-git:false::\n" +
		"hs-git:false:Clean tree:Review repository changes\n" +
		"files-btn:false::\n" +
		"bell-btn:true:POLLUX cannot send push notifications:Phone push: POLLUX cannot send push notifications"
	if got != hub {
		t.Fatalf("hub chat controls =\n%s\nwant\n%s", got, hub)
	}
}

// Pins are asked of the hub and of every awake peer; a card is pinned by
// its own machine's list, and one machine failing clears nobody else's.
func TestFleetPinsAreFetchedPerMachine(t *testing.T) {
	f := newFleetFixture(t, `[{"id":"mrx","name":"POLLUX","state":"awake","gaps":[]},`+
		`{"id":"vengeance","name":"NEMESIS","state":"awake","gaps":[]},`+
		`{"id":"mrx2","name":"CASTOR","state":"asleep","gaps":[]}]`)
	f.pinReply["vengeance"] = `{"pins":["Claude Agent @ shared"]}`
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)
	const pinned = `JSON.stringify([...document.querySelectorAll('.session-card')].map(c => ({
		remote: !!c.querySelector('.machine-badge'), pin: !!c.querySelector('.sc-pin')})).sort((a, b) => a.remote - b.remote))`
	page.waitFor(t, `document.querySelectorAll('.sc-pin').length === 1`)
	if got, _ := page.eval(t, pinned).(string); got != `[{"remote":false,"pin":false},{"remote":true,"pin":true}]` {
		t.Fatalf("cards = %s, want only the vengeance card pinned", got)
	}
	var hub, peer, asleep int
	for _, r := range f.requests() {
		if r.Path != "/api/pin" || r.Method != http.MethodGet {
			continue
		}
		switch r.Query {
		case "":
			hub++
		case "machine=vengeance":
			peer++
		default:
			asleep++
		}
	}
	if hub == 0 || peer == 0 || asleep != 0 {
		t.Fatalf("pin requests hub=%d vengeance=%d other=%d, want hub and vengeance only", hub, peer, asleep)
	}
	// vengeance goes away; the hub pins its own chat.  The peer's pin stays.
	f.mu.Lock()
	f.pinFail["vengeance"] = true
	f.pinReply[""] = `{"pins":["Claude Agent @ shared"]}`
	f.mu.Unlock()
	page.eval(t, `loadOrreryPins()`)
	page.waitFor(t, `document.querySelectorAll('.sc-pin').length === 2`)
}

const fleetSharedSessions = `{"pid":1,"machine":"mrx","sessionId":"sess-same","bufferName":"Claude Agent @ shared","label":"hub label"},` +
	`{"pid":40388,"machine":"vengeance","sessionId":"sess-same","bufferName":"Claude Agent @ shared","label":"remote label"}`

// The header shows the label of the chat that is open, not of a
// same-named chat on another machine.
func TestFleetHeaderShowsTheRemoteChatsOwnLabel(t *testing.T) {
	f := newFleetFixture(t, fleetTwoMachines)
	f.sessions = fleetSharedSessions
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)
	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'vengeance')); updateChatHeader(); 0`)
	if got, _ := page.eval(t, `document.querySelector('#header-title .ht-main').textContent`).(string); got != "remote label" {
		t.Fatalf("remote chat header = %q, want %q", got, "remote label")
	}
	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'mrx')); updateChatHeader(); 0`)
	if got, _ := page.eval(t, `document.querySelector('#header-title .ht-main').textContent`).(string); got != "hub label" {
		t.Fatalf("hub chat header = %q, want %q", got, "hub label")
	}
}

// A hub clone is found on the hub, even when a remote chat already has the
// name the clone came back with.
func TestFleetCloneOpensTheHubCloneNotASameNamedRemoteChat(t *testing.T) {
	f := newFleetFixture(t, fleetTwoMachines)
	f.spawn = `{"ok":true,"bufferName":"Claude Agent @ shared<2>"}`
	f.sessions = `{"pid":1,"machine":"mrx","bufferName":"Claude Agent @ shared"},` +
		`{"pid":40388,"machine":"vengeance","bufferName":"Claude Agent @ shared"},` +
		`{"pid":40389,"machine":"vengeance","bufferName":"Claude Agent @ shared<2>"}`
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 3`)
	page.eval(t, `SPAWN_POLL_MS = 5; window.alert = () => {};
		selectSession(lastSessions.find(s => s.machine === 'mrx')); cloneSession(); 0`)
	time.Sleep(400 * time.Millisecond)
	got, _ := page.eval(t, `currentSockMachine + '|' + currentBufferName`).(string)
	if got != "mrx|Claude Agent @ shared" {
		t.Fatalf("after a hub clone the open chat is %q, want to stay on the hub chat (the clone is not listed yet)", got)
	}
}

// A clone that was created but reported an error is looked for on the
// machine the clone was asked of.
func TestFleetCloneRecoveryKeepsItsMachine(t *testing.T) {
	f := newFleetFixture(t, fleetTwoMachines)
	f.spawn = `{"error":"settings failed","bufferName":"Claude Agent @ shared<2>"}`
	f.status = http.StatusBadGateway
	f.sessions = `{"pid":1,"machine":"mrx","bufferName":"Claude Agent @ shared"},` +
		`{"pid":40388,"machine":"vengeance","bufferName":"Claude Agent @ shared"},` +
		`{"pid":2,"machine":"mrx","bufferName":"Claude Agent @ shared<2>"},` +
		`{"pid":40389,"machine":"vengeance","bufferName":"Claude Agent @ shared<2>"}`
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 4`)
	page.eval(t, `SPAWN_POLL_MS = 5; window.alert = () => {};
		selectSession(lastSessions.find(s => s.machine === 'vengeance' && s.pid === 40388)); cloneSession(); 0`)
	time.Sleep(500 * time.Millisecond)
	got, _ := page.eval(t, `currentSockMachine + '|' + currentBufferName + '|' + cloneRecoveryMachine`).(string)
	if got != "vengeance|Claude Agent @ shared<2>|vengeance" {
		t.Fatalf("recovery opened %q, want the vengeance clone with its machine kept", got)
	}
}

// A gap reason never becomes a control's original title when the open chat
// moves between two machines that both declare it.
func TestFleetGapTitleDoesNotLeakBetweenMachines(t *testing.T) {
	f := newFleetFixture(t, `[{"id":"mrx","name":"POLLUX","state":"awake","gaps":[]},`+
		`{"id":"vengeance","name":"NEMESIS","state":"awake","gaps":["files"]},`+
		`{"id":"mrx2","name":"CASTOR","state":"awake","gaps":["files"]}]`)
	f.sessions = `{"pid":1,"machine":"mrx","bufferName":"Claude Agent @ a"},` +
		`{"pid":2,"machine":"vengeance","bufferName":"Claude Agent @ b"},` +
		`{"pid":3,"machine":"mrx2","bufferName":"Claude Agent @ c"}`
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 3`)
	page.eval(t, `setControlState(document.getElementById('files-btn'), {title: 'Browse files'}); 0`)
	read := `(async () => { await new Promise(r => setTimeout(r, 100));
		const b = document.getElementById('files-btn'); return b.disabled + '|' + b.title; })()`
	for _, step := range []struct{ machine, want string }{
		{"vengeance", "true|NEMESIS cannot browse files"},
		{"mrx2", "true|CASTOR cannot browse files"},
		{"mrx", "false|Browse files"},
	} {
		page.eval(t, `selectSession(lastSessions.find(s => s.machine === '`+step.machine+`')); 0`)
		if got, _ := page.eval(t, read).(string); got != step.want {
			t.Fatalf("on %s the files control = %q, want %q", step.machine, got, step.want)
		}
	}
}

// The same session id on two machines keeps two catalogue records.
func TestFleetCatalogueStateIsPerMachine(t *testing.T) {
	f := newFleetFixture(t, fleetTwoMachines)
	f.sessions = fleetSharedSessions
	f.catalog = map[string]string{
		"":          `{"sessionId":"sess-same","catalogued":"yes","note":"hub note","tags":["h"],"allTags":[]}`,
		"vengeance": `{"sessionId":"sess-same","catalogued":"yes","note":"remote note","tags":["r"],"allTags":[]}`,
	}
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)
	got, _ := page.eval(t, `(async () => {
		await fetchCatalogueState('sess-same', '');
		await fetchCatalogueState('sess-same', 'vengeance');
		await saveCatalogue('sess-same', 'remote edit', ['r'], 'vengeance');
		const hub = catalogueStates.get(catalogueKey('', 'sess-same'));
		const remote = catalogueStates.get(catalogueKey('vengeance', 'sess-same'));
		return hub.note + '|' + remote.note + '|' + catalogueStates.size;
	})()`).(string)
	if got != "hub note|saved|2" {
		t.Fatalf("catalogue states = %q, want hub and remote kept apart", got)
	}
}

// A gap hands a control back exactly as its own writers left it: still
// disabled if it was, and showing a title written while the gap held it.
func TestFleetGapRestoresTheControlsOwnState(t *testing.T) {
	f := newFleetFixture(t, `[{"id":"mrx","name":"POLLUX","state":"awake","gaps":[]},`+
		`{"id":"vengeance","name":"NEMESIS","state":"awake","gaps":["files","diff-review"]}]`)
	page := openChromePage(t, f.srv.URL)
	page.waitFor(t, `document.querySelectorAll('.session-card').length === 2`)
	page.eval(t, `fetchGitStatus = async () => {};
		setControlState(document.getElementById('files-btn'), {disabled: true});
		setControlState(document.getElementById('hs-git'), {title: 'Clean tree'}); 0`)
	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'vengeance')); 0`)
	page.eval(t, `new Promise(r => setTimeout(r, 100))`)
	if got, _ := page.eval(t, `document.getElementById('hs-git').title`).(string); got != "NEMESIS cannot review diffs" {
		t.Fatalf("gapped git title = %q, want the reason", got)
	}
	// A writer changes the git title while the gap holds the control.
	page.eval(t, `setControlState(document.getElementById('hs-git'), {title: '2 unstaged'}); 0`)
	if got, _ := page.eval(t, `document.getElementById('hs-git').title`).(string); got != "NEMESIS cannot review diffs" {
		t.Fatalf("a write during the gap leaked to the DOM: %q", got)
	}
	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'mrx')); 0`)
	got, _ := page.eval(t, `(async () => { await new Promise(r => setTimeout(r, 100));
		const f = document.getElementById('files-btn'), g = document.getElementById('hs-git');
		return f.disabled + '|' + f.title + '|' + g.title; })()`).(string)
	if got != "true||2 unstaged" {
		t.Fatalf("after the gap: %q, want a still-disabled files control and the git title written during the gap", got)
	}
}
