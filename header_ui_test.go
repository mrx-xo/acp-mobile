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
		fmt.Fprint(w, `{"current":"fable","models":[{"id":"fable","name":"fable"},{"id":"opus","name":"opus"}]}`)
	})
	mux.HandleFunc("/api/fork", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"supported":true}`)
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
	page.waitFor(t, `document.getElementById('hs-model').textContent === 'fable' && document.getElementById('hs-branch').textContent === 'syzygy'`)
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
			kebabBorder: getComputedStyle(document.getElementById('chat-menu-btn')).borderStyle,
		};
	})()`)
	if state["title"] != "acp-mobile" || state["ordinal"] != "4" || state["labeled"] != false {
		t.Fatalf("title = %v", state)
	}
	if state["provider"] != true || state["mode"] != "bypass" || state["modeColor"] != "rgb(251, 73, 52)" || state["modeBg"] != "rgba(0, 0, 0, 0)" {
		t.Fatalf("row 2 = %v", state)
	}
	if state["dirty"] != true || state["gitHidden"] != false || state["dotGone"] != true || state["bufGone"] != true || state["kebabBorder"] != "none" {
		t.Fatalf("row 2 git/status = %v", state)
	}

	labeled := newHeaderTestPage(t, "fix keyboard dismiss")
	got := labeled.evalObject(t, `(()=>{const t=document.getElementById('header-title');return {main:t.querySelector('.ht-main').textContent, ord:t.querySelector('.ht-ord').textContent, labeled:t.classList.contains('labeled')};})()`)
	if got["main"] != "fix keyboard dismiss" || got["ord"] != "" || got["labeled"] != true {
		t.Fatalf("labeled title = %v", got)
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
			reviewBar:!!document.getElementById('review-bar'), kebab:[...document.querySelectorAll('#chat-menu button')].filter(b=>b.style.display!=='none').map(b=>b.id).join(',')};})()`)
	if closed["hidden"] != true || closed["reviewBar"] != false || closed["pillW"] != float64(36) {
		t.Fatalf("closed state = %v", closed)
	}
	if h, _ := closed["height"].(float64); h < 44 {
		t.Fatalf("grabber tap target %v px, want at least 44", h)
	}
	if closed["kebab"] != "cm-turn-nav,cm-pin,cm-kill" {
		t.Fatalf("kebab items = %v", closed["kebab"])
	}
	page.eval(t, `document.getElementById('header-grabber').click()`)
	page.waitFor(t, `document.getElementById('chat-toolbar').hidden === false && document.getElementById('tb-git').classList.contains('dirty')`)
	saveUIShot(t, page, "header-toolbar-open")
	open := page.evalObject(t, `(()=>{const tb=document.getElementById('chat-toolbar');
		return {items:[...tb.querySelectorAll('button')].map(b=>b.id).join(','), labels:[...tb.querySelectorAll('.tb-label')].map(e=>e.textContent).join(','),
			height:tb.getBoundingClientRect().height, messages:document.getElementById('messages').getBoundingClientRect().height,
			stored:localStorage.getItem('acp-toolbar-open'), expanded:document.getElementById('header-grabber').getAttribute('aria-expanded')};})()`)
	if open["items"] != "tb-git,tb-model,tb-pinned,tb-fork,tb-clone,tb-catalogue" {
		t.Fatalf("toolbar items = %v", open["items"])
	}
	if open["labels"] != "syzygy,fable,Pinned,Fork,Clone,Catalogue" || open["stored"] != "true" || open["expanded"] != "true" {
		t.Fatalf("toolbar labels = %v", open)
	}
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
