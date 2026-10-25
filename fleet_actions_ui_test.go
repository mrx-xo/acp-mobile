package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Until fleet step 3, a remote session must not drive the hub's Emacs:
// its card has no delete button and the chat's Lisp-backed actions are
// disabled.  The hub's own session keeps every action.
func TestFleetRemoteSessionHidesHubActions(t *testing.T) {
	var lispHits atomic.Int32
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
	for _, p := range []string{"/api/kill", "/api/fork", "/api/label", "/api/push", "/api/spawn"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			lispHits.Add(1)
			http.Error(w, "test", http.StatusBadRequest)
		})
	}
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
	if cards != `[{"remote":false,"del":true},{"remote":true,"del":false}]` {
		t.Fatalf("cards = %s, want a delete button on the local card only", cards)
	}

	const controls = `JSON.stringify(['tb-kill','tb-fork','tb-clone','tb-pin','tb-model'].map(id => {
		const b = document.getElementById(id); return id + ':' + b.disabled + ':' + (b.title || '');
	}).concat(['bell:' + document.getElementById('bell-btn').classList.contains('visible'),
		'label:' + (document.getElementById('header-title').title || '')]))`
	const remoteTitle = `Not available for remote sessions yet`

	page.eval(t, `setToolbarOpen(true); selectSession(lastSessions.find(s => s.machine === 'vengeance'))`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ skate'`)
	got, _ := page.eval(t, controls).(string)
	want := `["tb-kill:true:` + remoteTitle + `","tb-fork:true:` + remoteTitle + `","tb-clone:true:` + remoteTitle +
		`","tb-pin:true:` + remoteTitle + `","tb-model:true:` + remoteTitle + `","bell:false","label:` + remoteTitle + `"]`
	if got != want {
		t.Fatalf("remote chat controls =\n%s\nwant\n%s", got, want)
	}
	// Calling the actions directly must not reach the hub either.
	page.eval(t, `window.confirm = () => true; window.prompt = () => 'x'; window.alert = () => {};
		killSession(); forkSession(); cloneSession(); document.getElementById('header-title').click();
		document.getElementById('bell-btn').click(); 0`)
	page.eval(t, `new Promise(r => setTimeout(() => r(0), 300))`)
	if n := lispHits.Load(); n != 0 {
		t.Fatalf("remote chat reached %d hub Lisp routes", n)
	}

	page.eval(t, `selectSession(lastSessions.find(s => s.machine === 'mrx'))`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ dotfiles'`)
	local, _ := page.eval(t, `JSON.stringify(['tb-kill','tb-clone','tb-pin','tb-model'].map(id => {
		const b = document.getElementById(id); return id + ':' + b.disabled + ':' + (b.title === '`+remoteTitle+`');
	}).concat(['bell:' + document.getElementById('bell-btn').classList.contains('visible'),
		'label:' + (document.getElementById('header-title').title === '`+remoteTitle+`')]))`).(string)
	if local != `["tb-kill:false:false","tb-clone:false:false","tb-pin:false:false","tb-model:false:false","bell:true","label:false"]` {
		t.Fatalf("local chat controls = %s, want every action back", local)
	}
}
