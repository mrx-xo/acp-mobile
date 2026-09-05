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
	mux.HandleFunc("/sw.js", handleServiceWorker)
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
		navigatorHidden: document.getElementById('navigator').classList.contains('hidden'),
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
