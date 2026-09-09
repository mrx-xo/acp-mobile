package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// An idle gap in a large replay must not expose a partial bottom. Once the
// explicit boundary arrives, readers keep their position and can jump/follow.
func TestHistoryLoadingWaitsForCompleteMarker(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	page.eval(t, `
		showChat();
		for (let i = 0; i < 40; i++) addAgentMsg('old ' + i);
		messagesEl.scrollTop = 0;
		messagesEl.dispatchEvent(new Event('scroll'));
	`)
	page.waitFor(t, `scrollBtn.classList.contains('visible')`)
	page.eval(t, `
		window.WebSocket = class {
			static OPEN = 1; static CLOSING = 2; static CLOSED = 3;
			constructor() { this.readyState = 1; } close() {} send() {}
		};
		window.deliver = msg => ws.onmessage({data: JSON.stringify(msg)});
		window.startReplay = () => {
			connect('fixture'); ws.onopen();
			deliver({jsonrpc:'2.0', method:'acp-multiplex/replay_start'});
		};
		window.endReplay = () => deliver({jsonrpc:'2.0', method:'acp-multiplex/replay_complete'});
		window.replayMessage = text => deliver({jsonrpc:'2.0', method:'session/update',
			params:{sessionId:'fixture', update:{sessionUpdate:'user_message_chunk', content:{type:'text',text}}}});
		startReplay();
		for (let i = 0; i < 40; i++) replayMessage('history ' + i);
	`)
	state := page.evalObject(t, `(async () => {
		await new Promise(r => setTimeout(r, 400));
		return {buffering: replayMode, button: scrollBtn.classList.contains('visible'),
			loading: !!document.querySelector('#history-loading:not([hidden])'),
			oldVisible: messagesEl.textContent.includes('old 0')};
	})()`)
	if state["buffering"] != true || state["button"] != false || state["loading"] != true || state["oldVisible"] != true {
		t.Fatalf("incomplete history must retain the transcript and show loading without a jump button: %v", state)
	}
	page.eval(t, `endReplay()`)
	page.waitFor(t, `!messagesEl.hasAttribute('aria-busy') && scrollBtn.classList.contains('visible')`)
	state = page.evalObject(t, `(() => {
		const preserved = messagesEl.scrollTop < 10;
		scrollBtn.click();
		replayMessage('new live message');
		return {preserved, gap: messagesEl.scrollHeight - messagesEl.clientHeight - messagesEl.scrollTop,
			button: scrollBtn.classList.contains('visible'), latest: messagesEl.textContent.includes('new live message')};
	})()`)
	if state["preserved"] != true || state["gap"].(float64) > 1 || state["button"] != false || state["latest"] != true {
		t.Fatalf("ready history must preserve reading position, then jump and follow live content: %v", state)
	}
	// Even an empty replay has an explicit completion; it must not hang.
	page.eval(t, `startReplay(); endReplay();`)
	page.waitFor(t, `!messagesEl.hasAttribute('aria-busy') && !replayMode`)
	if page.eval(t, `scrollBtn.classList.contains('visible') || !document.getElementById('history-loading').hidden`) != false {
		t.Fatal("empty completed history should have neither loading nor jump controls")
	}
}

// A completed replay can still have an image with unknown dimensions. Waiting
// for that actual load (including lazy images) must survive cancellation safely.
func TestHistoryLoadingWaitsForImagesAndCancelsStaleLayout(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" width="300" height="1200"><rect width="300" height="1200" fill="gray"/></svg>`)
	}))
	t.Cleanup(imageServer.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	page := openComposerTestPage(t, 844, 844)
	page.eval(t, fmt.Sprintf(`
		showChat();
		for (let i = 0; i < 40; i++) addAgentMsg('history ' + i);
		beginHistoryLoad();
		const img = document.createElement('img');
		img.loading = 'lazy'; img.src = %q;
		messagesEl.lastElementChild.appendChild(img);
		void finishHistoryLayout(historyLoad);
	`, imageServer.URL))
	state := page.evalObject(t, `(async () => {
		await new Promise(r => setTimeout(r, 400));
		return {busy: messagesEl.getAttribute('aria-busy'), loading: !historyLoadingEl.hidden,
			button: scrollBtn.classList.contains('visible'), complete: messagesEl.querySelector('img').complete};
	})()`)
	if state["busy"] != "true" || state["loading"] != true || state["button"] != false || state["complete"] != false {
		t.Fatalf("button must wait for pending image dimensions: %v", state)
	}
	state = page.evalObject(t, `(async () => {
		messagesEl.dispatchEvent(new WheelEvent('wheel', {deltaY: -100}));
		messagesEl.scrollTop = 0;
		messagesEl.dispatchEvent(new Event('scroll'));
		await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
		return {top: messagesEl.scrollTop, following: historyLoad.follow};
	})()`)
	if state["top"].(float64) > 1 || state["following"] != false {
		t.Fatalf("upward scrolling while assets load must interrupt automatic following: %v", state)
	}
	// Replace the connection while the old image is still loading. Completing
	// that old layout must not unlock the new connection's partial transcript.
	page.eval(t, `beginHistoryLoad()`)
	once.Do(func() { close(release) })
	page.waitFor(t, `messagesEl.querySelector('img').complete`)
	state = page.evalObject(t, `(async () => {
		await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
		return {ready: historyReady, button: scrollBtn.classList.contains('visible')};
	})()`)
	if state["ready"] != false || state["button"] != false {
		t.Fatalf("stale asset completion unlocked a newer history load: %v", state)
	}
	page.eval(t, `finishHistoryLayout(historyLoad)`)
	page.waitFor(t, `historyReady`)
	if gap := page.eval(t, `messagesEl.scrollHeight - messagesEl.clientHeight - messagesEl.scrollTop`).(float64); gap > 1 {
		t.Fatalf("completed image layout left the latest message below the viewport by %v px", gap)
	}
	page.eval(t, `beginHistoryLoad(); cancelHistoryLoad();`)
	state = page.evalObject(t, `(async () => {
		await new Promise(r => setTimeout(r, 400));
		return {busy: messagesEl.hasAttribute('aria-busy'), loading: !historyLoadingEl.hidden,
			button: scrollBtn.classList.contains('visible')};
	})()`)
	if state["busy"] != false || state["loading"] != false || state["button"] != false {
		t.Fatalf("cancelled loading must not revive its indicator or button: %v", state)
	}
}
