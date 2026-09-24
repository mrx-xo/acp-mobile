package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// newHeaderTestPage serves the app with one session whose socket replays a
// short conversation, plus stubbed models and git status, and opens it.
func newHeaderTestPage(t *testing.T, label string) *chromePage {
	t.Helper()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "1.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprint(conn,
				`{"jsonrpc":"2.0","method":"acp-multiplex/replay_start"}`+"\n"+
					`{"jsonrpc":"2.0","method":"acp-multiplex/meta","params":{"name":"Claude Agent @ acp-mobile<4>"}}`+"\n"+
					`{"jsonrpc":"2.0","id":0,"result":{"sessionId":"s1","cwd":"/src/acp-mobile","modes":{"currentModeId":"bypassPermissions","availableModes":[{"id":"bypassPermissions","name":"Bypass Permissions"},{"id":"default","name":"Default"}]}}}`+"\n"+
					`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hello"}}}}`+"\n"+
					`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"reply"}}}}`+"\n"+
					`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_complete","stopReason":"end_turn"}}}`+"\n"+
					`{"jsonrpc":"2.0","method":"acp-multiplex/replay_complete"}`+"\n")
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	session := fmt.Sprintf(`{"pid":1,"sessionId":"s1","bufferName":"Claude Agent @ acp-mobile<4>","project":"acp-mobile","cwd":"/src/acp-mobile","label":%q}`, label)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/assets/", handleAsset)
	mux.Handle("/fonts/", http.FileServerFS(fontsFS))
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"sessions":[`+session+`]}`)
	})
	mux.HandleFunc("/api/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"current":"fable[1m]","models":[{"id":"fable[1m]","name":"Fable"},{"id":"opus[1m]","name":"Opus (1M context)"}]}`)
	})
	mux.HandleFunc("/api/fork", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"supported":true}`)
	})
	pinned := false
	mux.HandleFunc("/api/pin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			pinned = !pinned
			if pinned {
				fmt.Fprint(w, `{"bufferName":"Claude Agent @ acp-mobile<4>","pinned":true,"pins":["Claude Agent @ acp-mobile<4>"]}`)
			} else {
				fmt.Fprint(w, `{"bufferName":"Claude Agent @ acp-mobile<4>","pinned":false,"pins":[]}`)
			}
			return
		}
		fmt.Fprint(w, `{"pins":[]}`)
	})
	mux.HandleFunc("/api/git-status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"branch":"syzygy","staged":3,"unstaged":2,"untracked":1}`)
	})
	mux.HandleFunc("/api/diff-review", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"scope":"repository","available":true,"branch":"syzygy","files":[]}`)
	})
	mux.Handle("/ws", websocket.Handler(func(ws *websocket.Conn) { bridgeWebSocket(ws, sockPath) }))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	page := openChromePage(t, srv.URL)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": 393, "height": 852, "deviceScaleFactor": 1, "mobile": true})
	page.waitFor(t, `typeof connect === 'function' && Array.isArray(lastSessions)`)
	page.eval(t, `lastSessions = [`+session+`]; selectSession(lastSessions[0]);`)
	page.waitFor(t, `document.querySelectorAll('#messages > .msg').length >= 2 && statusText.className === 'header-status connected'`)
	return page
}

func TestHeaderTitleAndRow(t *testing.T) {
	page := newHeaderTestPage(t, "")
	page.waitFor(t, `document.getElementById('hs-model').textContent === 'Fable 5.1' && document.getElementById('hs-branch').textContent === 'syzygy'`)
	saveUIShot(t, page, "header-row")
	state := page.evalObject(t, `(()=>{
		const t = document.getElementById('header-title');
		const modeBtn = document.getElementById('mode-btn');
		const git = document.getElementById('hs-git');
		return {
			title: t.querySelector('.ht-main').textContent,
			ordinal: t.querySelector('.ht-ord').textContent,
			labeled: t.classList.contains('labeled'),
			provider: !!document.querySelector('#hs-agent .provider-icon.anthropic'),
			mode: modeBtn.textContent,
			modeColor: getComputedStyle(modeBtn).color,
			modeBg: getComputedStyle(modeBtn).backgroundColor,
			dirty: git.classList.contains('dirty'),
			gitHidden: git.hidden,
			dotGone: getComputedStyle(statusText).position === 'absolute',
			bufGone: !document.getElementById('header-buf'),
			menuGone: !document.getElementById('chat-menu-btn'),
		};
	})()`)
	if state["title"] != "acp-mobile" || state["ordinal"] != "4" || state["labeled"] != false {
		t.Fatalf("title = %v", state)
	}
	if state["provider"] != true || state["mode"] != "full" || state["modeColor"] != "rgb(251, 73, 52)" || state["modeBg"] != "rgba(0, 0, 0, 0)" {
		t.Fatalf("row 2 = %v", state)
	}
	if state["dirty"] != true || state["gitHidden"] != false || state["dotGone"] != true || state["bufGone"] != true || state["menuGone"] != true {
		t.Fatalf("row 2 git/status = %v", state)
	}

	labeled := newHeaderTestPage(t, "fix keyboard dismiss")
	got := labeled.evalObject(t, `(()=>{const t=document.getElementById('header-title');return {main:t.querySelector('.ht-main').textContent, ord:t.querySelector('.ht-ord').textContent, labeled:t.classList.contains('labeled')};})()`)
	if got["main"] != "fix keyboard dismiss" || got["ord"] != "" || got["labeled"] != true {
		t.Fatalf("labeled title = %v", got)
	}
}

func TestHeaderCanonicalVocabulary(t *testing.T) {
	page := newHeaderTestPage(t, "")
	got := page.evalObject(t, `(()=>{
		if (typeof canonicalModelName !== 'function') return {missing:true};
		const models = [
			['fable[1m]', 'Claude Agent @ x'],
			['opus[1m]', 'Claude Agent @ x'],
			['sonnet', 'Claude Agent @ x'],
			['default', 'Claude Agent @ x'],
			['default', 'DeepSeek Agent @ x'],
			['gpt-6-astra', 'Codex Agent @ x'],
			['gpt-5.6-sol', 'Codex Agent @ x'],
			['openai/gpt-5.6-luna', 'OpenCode Agent @ x'],
			['openrouter/z-ai/glm-5.3-flash', 'OpenCode Agent @ x'],
		].map(([id, buffer]) => canonicalModelName(id, [], buffer));
		const modes = ['bypassPermissions','agent-full-access','bypass','acceptEdits','agent','auto','read-only','default','build','plan'].map(modeShortName);
		return {models:models.join('|'), modes:modes.join('|')};
	})()`)
	if got["models"] != "Fable 5.1|Opus 5.5|Sonnet 5|Opus 5.5|DeepSeek Chat|Astra 6|Sol 5.6|Luna 5.6|GLM 5.3 Flash" {
		t.Fatalf("canonical models = %v", got)
	}
	if got["modes"] != "full|full|full|accept edits|auto|auto|ask|manual|build|plan" {
		t.Fatalf("canonical modes = %v", got)
	}
}

func TestHeaderConnectionVisuals(t *testing.T) {
	page := newHeaderTestPage(t, "")
	read := func() map[string]interface{} {
		return page.evalObject(t, `(()=>{const icon=document.getElementById('hs-agent');const line=document.getElementById('conn-line');
			return {iconOpacity:getComputedStyle(icon).opacity, lineHidden:line.hidden, dead:line.classList.contains('dead'), label:statusText.textContent};})()`)
	}
	page.waitFor(t, `getComputedStyle(document.getElementById('hs-agent')).opacity === '1'`)
	connected := read()
	if connected["iconOpacity"] != "1" || connected["lineHidden"] != true {
		t.Fatalf("connected visuals = %v", connected)
	}
	page.eval(t, `statusText.textContent='Reconnecting...'; statusText.className='header-status reconnecting'`)
	page.waitFor(t, `document.getElementById('conn-line').hidden === false`)
	busy := read()
	if busy["dead"] != false || busy["label"] != "Reconnecting..." {
		t.Fatalf("reconnecting visuals = %v", busy)
	}
	page.eval(t, `statusText.textContent='Disconnected'; statusText.className='header-status error'`)
	page.waitFor(t, `document.getElementById('conn-line').classList.contains('dead') && getComputedStyle(document.getElementById('hs-agent')).opacity === '0.4'`)
	dead := read()
	if dead["iconOpacity"] != "0.4" || dead["lineHidden"] != false || dead["label"] != "Disconnected" {
		t.Fatalf("disconnected visuals = %v", dead)
	}
	page.eval(t, `statusText.textContent='Connected'; statusText.className='header-status connected'`)
	page.waitFor(t, `document.getElementById('conn-line').hidden === true && !document.getElementById('conn-line').classList.contains('dead')`)
}

func TestHeaderToolbar(t *testing.T) {
	page := newHeaderTestPage(t, "")
	page.waitFor(t, `document.getElementById('hs-branch').textContent === 'syzygy'`)
	closed := page.evalObject(t, `(()=>{const g=document.getElementById('header-grabber');const r=g.getBoundingClientRect();const b=g.querySelector('span').getBoundingClientRect();
		return {height:r.height + 22, pillW:b.width, hidden:document.getElementById('chat-toolbar').hidden, messages:document.getElementById('messages').getBoundingClientRect().height,
			reviewBar:!!document.getElementById('review-bar'), menu:!!document.getElementById('chat-menu')};})()`)
	if closed["hidden"] != true || closed["reviewBar"] != false || closed["pillW"] != float64(36) {
		t.Fatalf("closed state = %v", closed)
	}
	if h, _ := closed["height"].(float64); h < 44 {
		t.Fatalf("grabber tap target %v px, want at least 44", h)
	}
	if closed["menu"] != false {
		t.Fatalf("header menu should be removed: %v", closed)
	}
	page.eval(t, `document.getElementById('header-grabber').click()`)
	page.waitFor(t, `document.getElementById('chat-toolbar').hidden === false && document.getElementById('tb-git').classList.contains('dirty')`)
	saveUIShot(t, page, "header-toolbar-open")
	open := page.evalObject(t, `(()=>{const tb=document.getElementById('chat-toolbar');
		return {items:[...tb.querySelectorAll('button')].map(b=>b.id).join(','), labels:[...tb.querySelectorAll('.tb-label')].map(e=>e.textContent).join(','),
			height:tb.getBoundingClientRect().height, messages:document.getElementById('messages').getBoundingClientRect().height,
			stored:localStorage.getItem('acp-toolbar-open'), expanded:document.getElementById('header-grabber').getAttribute('aria-expanded'),
			overflow:tb.scrollWidth > tb.clientWidth, overflowX:getComputedStyle(tb).overflowX};})()`)
	if open["items"] != "tb-git,tb-model,tb-fork,tb-clone,tb-catalogue,tb-pin,tb-turn-nav,tb-kill" {
		t.Fatalf("toolbar items = %v", open["items"])
	}
	if open["labels"] != "syzygy,Fable 5.1,Fork,Clone,Catalogue,Pin chat,Turn nav,Kill" || open["stored"] != "true" || open["expanded"] != "true" || open["overflow"] != true || open["overflowX"] != "auto" {
		t.Fatalf("toolbar labels = %v", open)
	}
	page.eval(t, `document.getElementById('tb-pin').click()`)
	page.waitFor(t, `document.querySelector('#tb-pin .tb-label').textContent === 'Unpin' && document.getElementById('tb-pin').classList.contains('pinned')`)
	page.eval(t, `document.getElementById('tb-turn-nav').click()`)
	page.waitFor(t, `document.querySelector('#tb-turn-nav .tb-label').textContent === 'Hide nav' && document.getElementById('tb-turn-nav').classList.contains('enabled')`)
	page.eval(t, `catalogueStates.set('s1',{sessionId:'s1',catalogued:'2026-09-24T00:00:00Z',note:'keep'}); paintCatalogueEntry(catalogueStates.get('s1')); document.getElementById('tb-catalogue').click()`)
	page.waitFor(t, `document.getElementById('catalogue-menu').classList.contains('visible')`)
	secondary := page.evalObject(t, `(()=>({items:[...document.querySelectorAll('#catalogue-menu button')].map(b=>b.textContent).join(','), headerMenu:!!document.getElementById('chat-menu')}))()`)
	if secondary["items"] != "Edit catalogue entry,Uncatalogue" || secondary["headerMenu"] != false {
		t.Fatalf("catalogue secondary actions = %v", secondary)
	}
	page.eval(t, `closeCatalogueMenu()`)
	before, _ := closed["messages"].(float64)
	after, _ := open["messages"].(float64)
	barH, _ := open["height"].(float64)
	if before-after < barH-1 || before-after > barH+1 {
		t.Fatalf("message list shrank by %v, toolbar is %v", before-after, barH)
	}
	page.eval(t, `document.getElementById('tb-git').click()`)
	page.waitFor(t, `document.getElementById('diff-review').classList.contains('visible') && diffReview.scope === 'repository'`)
	page.eval(t, `closeDiffReview(); location.reload()`)
	page.waitFor(t, `typeof connect === 'function' && Array.isArray(lastSessions)`)
	page.eval(t, `selectSession({pid:1, sessionId:'s1', bufferName:'Claude Agent @ acp-mobile<4>', project:'acp-mobile', cwd:'/src/acp-mobile'})`)
	page.waitFor(t, `document.getElementById('chat-toolbar').hidden === false`)
	page.eval(t, `document.getElementById('header-grabber').click()`)
	page.waitFor(t, `document.getElementById('chat-toolbar').hidden === true && localStorage.getItem('acp-toolbar-open') === 'false'`)
}
