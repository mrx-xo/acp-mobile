package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var presenceMu sync.Mutex
var presenceNotes []string

// A push tap must land on the tapped chat even when iOS reloaded the
// home-screen app on focus (postMessage lost) or launched it fresh without
// honoring the openWindow URL.  sw.js records the target in the Cache API;
// the page consumes it on load and on resume.
func newPushTapServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("echo") == "1" {
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte(`self.addEventListener('install', () => self.skipWaiting());
			self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));
			self.addEventListener('message', (e) => {
				if (e.data && e.data.type === 'echo-open-session') e.source.postMessage({ type: 'open-session', bufferName: e.data.bufferName });
			});`))
			return
		}
		handleServiceWorker(w, r)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{
			map[string]interface{}{"pid": 4242, "sessionId": "s-tap", "bufferName": "Claude Agent @ tap-test", "cwd": "/tmp/tap"},
		}})
	})
	mux.HandleFunc("/api/statuses", handleStatuses)
	mux.HandleFunc("/api/presence", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		presenceMu.Lock()
		presenceNotes = append(presenceNotes, string(b))
		presenceMu.Unlock()
		w.WriteHeader(204)
	})
	return httptest.NewServer(mux)
}

func TestPushTapPendingSessionOpensChatOnLoad(t *testing.T) {
	server := newPushTapServer(t)
	defer server.Close()
	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof openSessionByName === 'function'`)
	page.eval(t, `(async () => { const c = await caches.open('acp-pending'); await c.put('/pending-session', new Response('Claude Agent @ tap-test')); return true; })()`)
	page.call(t, "Page.reload", map[string]interface{}{})
	page.waitFor(t, `typeof openSessionByName === 'function' && document.getElementById('chat-view').classList.contains('visible')`)
	state := page.evalObject(t, `(async () => ({
		buffer: currentBufferName,
		navigatorHidden: document.getElementById('orrery').classList.contains('hidden'),
		pendingCleared: !(await (await caches.open('acp-pending')).match('/pending-session'))
	}))()`)
	if state["buffer"] != "Claude Agent @ tap-test" || state["navigatorHidden"] != true || state["pendingCleared"] != true {
		t.Fatalf("pending session on load: %#v", state)
	}
}

func TestPushTapPendingSessionOpensChatOnResume(t *testing.T) {
	server := newPushTapServer(t)
	defer server.Close()
	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof openSessionByName === 'function' && lastSessions.length === 1`)
	page.eval(t, `(async () => { const c = await caches.open('acp-pending'); await c.put('/pending-session', new Response('Claude Agent @ tap-test')); window.dispatchEvent(new Event('pageshow')); return true; })()`)
	page.waitFor(t, `document.getElementById('chat-view').classList.contains('visible') && currentBufferName === 'Claude Agent @ tap-test'`)
}

// The app-already-open path: sw.js posts open-session to the live window.
// Per the Service Worker spec (WebKit and Blink both implement it) those
// messages are queued until the page calls startMessages() or assigns
// onmessage; addEventListener alone never receives them.  Serve an echo
// worker so the test exercises real delivery, not a synthetic event.
func TestPushTapServiceWorkerMessageOpensChatWhileOpen(t *testing.T) {
	server := newPushTapServer(t)
	defer server.Close()
	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof openSessionByName === 'function' && lastSessions.length === 1`)
	// Swap in the echo worker at the same scope; the page's listener and
	// startMessages() call are what is under test.
	page.eval(t, `navigator.serviceWorker.register('/sw.js?echo=1', { scope: '/' }).then(() => true)`)
	page.waitFor(t, `!!navigator.serviceWorker.controller && navigator.serviceWorker.controller.scriptURL.indexOf('echo=1') >= 0`)
	page.eval(t, `(navigator.serviceWorker.controller.postMessage({ type: 'echo-open-session', bufferName: 'Claude Agent @ tap-test' }), true)`)
	page.waitFor(t, `document.getElementById('chat-view').classList.contains('visible') && currentBufferName === 'Claude Agent @ tap-test'`)
}

// App already in the foreground (user on the Orrery or in another chat)
// when a push about another chat goes out: iOS gives the page no way to
// hear its worker and no notificationclick, so the page polls the
// server's inbox while visible and shows an in-app banner whose tap
// opens that chat.
func TestForegroundPushViaStatusesShowsBannerAndTapOpensChat(t *testing.T) {
	resetPushInbox()
	resetPresence()
	server := newPushTapServer(t)
	defer server.Close()
	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof loadStatuses === 'function' && lastSessions.length === 1 && pushSince > 0`)
	recordPush("Claude Agent @ tap-test", "tap-test", "Finished", time.Now().UnixMilli())
	page.eval(t, `loadStatuses().then(() => true)`)
	page.waitFor(t, `document.getElementById('push-toast').classList.contains('visible') && document.getElementById('pt-title').textContent === 'tap-test'`)
	page.eval(t, `(document.getElementById('push-toast').click(), true)`)
	page.waitFor(t, `document.getElementById('chat-view').classList.contains('visible') && currentBufferName === 'Claude Agent @ tap-test'`)
	page.waitFor(t, `!document.getElementById('push-toast').classList.contains('shown')`)
}

func TestForegroundPushViaSocketFrameShowsBanner(t *testing.T) {
	resetPushInbox()
	resetPresence()
	server := newPushTapServer(t)
	defer server.Close()
	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof handleSocketFrame === 'function' && lastSessions.length === 1`)
	page.eval(t, `(handleSocketFrame({jsonrpc:'2.0', method:'acp-mobile/push', params:{bufferName:'Claude Agent @ tap-test', title:'tap-test', message:'Finished', at: Date.now()}}), true)`)
	page.waitFor(t, `document.getElementById('push-toast').classList.contains('visible') && document.getElementById('pt-msg').textContent === 'Finished'`)
	// Same chat on screen: no banner.
	page.eval(t, `(hidePushToast(), openSessionByName('Claude Agent @ tap-test'), true)`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ tap-test'`)
	// Let the fade-out finish (hidePushToast keeps 'visible' for 200ms) so
	// the next check reflects the second frame, not residue from the first.
	page.waitFor(t, `!document.getElementById('push-toast').classList.contains('visible')`)
	page.eval(t, `(handleSocketFrame({jsonrpc:'2.0', method:'acp-mobile/push', params:{bufferName:'Claude Agent @ tap-test', title:'tap-test', message:'Again', at: Date.now()}}), true)`)
	if shown, _ := page.eval(t, `document.getElementById('push-toast').classList.contains('visible') || pushToastTarget !== null`).(bool); shown {
		t.Fatal("banner must not show for the chat already on screen")
	}
}

func TestPagePostsPresenceOnLoadChatAndDismiss(t *testing.T) {
	presenceMu.Lock()
	presenceNotes = nil
	presenceMu.Unlock()
	server := newPushTapServer(t)
	defer server.Close()
	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof sendPresence === 'function' && lastSessions.length === 1`)
	page.eval(t, `(openSessionByName('Claude Agent @ tap-test'), true)`)
	page.waitFor(t, `currentBufferName === 'Claude Agent @ tap-test'`)
	page.eval(t, `(showPushToast({bufferName:'Codex Agent @ z', title:'z', message:'m'}), document.getElementById('pt-close').click(), true)`)
	page.eval(t, `(showOrrery(), true)`)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		presenceMu.Lock()
		joined := strings.Join(presenceNotes, "\n")
		presenceMu.Unlock()
		if strings.Contains(joined, `"visible":true,"bufferName":""`) &&
			strings.Contains(joined, `"bufferName":"Claude Agent @ tap-test"`) &&
			strings.Contains(joined, `"read":"Codex Agent @ z"`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	presenceMu.Lock()
	defer presenceMu.Unlock()
	t.Fatalf("presence notes = %v", presenceNotes)
}
