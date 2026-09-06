package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	mux.HandleFunc("/api/statuses", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"statuses": map[string]string{}, "version": buildID})
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
// when the banner is tapped: no load, no resume event, and on iOS the
// worker's clients.matchAll() may miss the window.  The worker also
// broadcasts the target; the page must act on it and clear the durable
// copy so the next resume does not replay it.
func TestPushTapBroadcastOpensChatWhileForeground(t *testing.T) {
	server := newPushTapServer(t)
	defer server.Close()
	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof openSessionByName === 'function' && lastSessions.length === 1`)
	page.eval(t, `(async () => {
		const c = await caches.open('acp-pending');
		await c.put('/pending-session', new Response('Claude Agent @ tap-test'));
		new BroadcastChannel('acp-push').postMessage({ type: 'open-session', bufferName: 'Claude Agent @ tap-test' });
		return true;
	})()`)
	page.waitFor(t, `document.getElementById('chat-view').classList.contains('visible') && currentBufferName === 'Claude Agent @ tap-test'`)
	page.waitFor(t, `(async () => !(await (await caches.open('acp-pending')).match('/pending-session')))()`)
}
