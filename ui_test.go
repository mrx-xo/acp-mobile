package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestThoughtProgressRenderingLiveAndReplay(t *testing.T) {
	messages := loadThoughtReplayFixture(t)
	const replayPrefix = 4
	const anonymousReplay = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"thought-session-1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"cached anonymous "}}}}`
	const anonymousLive = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"thought-session-1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"continuation"}}}}`

	var connectionMu sync.Mutex
	connectionCount := 0
	liveRelease := make(chan struct{})
	anonymousRelease := make(chan struct{})
	var liveReleaseOnce sync.Once
	var anonymousReleaseOnce sync.Once
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sessions": []map[string]interface{}{{
				"pid": 4242, "sessionId": "thought-session-1",
				"cwd": "/tmp/syzygy", "project": "syzygy",
				"title": "Fixture Agent", "bufferName": "TEST: Thought Rendering",
			}},
		})
	})
	mux.HandleFunc("/api/statuses", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"statuses": map[string]string{}})
	})
	mux.Handle("/ws", &websocket.Server{
		Handler: func(ws *websocket.Conn) {
			connectionMu.Lock()
			connectionCount++
			connection := connectionCount
			connectionMu.Unlock()

			switch connection {
			case 1:
				for _, message := range messages[:replayPrefix] {
					if err := websocket.Message.Send(ws, message); err != nil {
						return
					}
				}
				// The test releases this only after observing replayMode=false,
				// so every remaining frame is guaranteed to use handleMessage.
				<-liveRelease
				for _, message := range messages[replayPrefix:] {
					if err := websocket.Message.Send(ws, message); err != nil {
						return
					}
				}
			case 2:
				// Reload connections receive the completed sequence as one replay.
				for _, message := range messages {
					if err := websocket.Message.Send(ws, message); err != nil {
						return
					}
				}
			case 3:
				// This reload ends its replay with an anonymous thought, then
				// continues that same logical thought through the live path.
				for _, message := range append(messages, anonymousReplay) {
					if err := websocket.Message.Send(ws, message); err != nil {
						return
					}
				}
				<-anonymousRelease
				if err := websocket.Message.Send(ws, anonymousLive); err != nil {
					return
				}
			default:
				for _, message := range messages {
					if err := websocket.Message.Send(ws, message); err != nil {
						return
					}
				}
			}

			var ignored string
			for websocket.Message.Receive(ws, &ignored) == nil {
			}
		},
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	defer liveReleaseOnce.Do(func() { close(liveRelease) })
	defer anonymousReleaseOnce.Do(func() { close(anonymousRelease) })

	page := openChromePage(t, server.URL)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{
		"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": true,
	})
	page.call(t, "Page.reload", map[string]interface{}{})
	openThoughtFixtureSession(t, page)
	page.waitFor(t, `replayMode === false`)
	liveReleaseOnce.Do(func() { close(liveRelease) })
	page.waitFor(t, `document.querySelector('.msg.agent') && document.querySelector('.msg.agent').textContent.includes('Done.')`)
	live := thoughtProgressState(t, page)
	assertThoughtProgressState(t, live)

	page.call(t, "Page.reload", map[string]interface{}{})
	openThoughtFixtureSession(t, page)
	page.waitFor(t, `document.querySelector('.msg.agent') && document.querySelector('.msg.agent').textContent.includes('Done.')`)
	replayed := thoughtProgressState(t, page)
	assertThoughtProgressState(t, replayed)
	if replayed["signature"] != live["signature"] {
		t.Fatalf("replay signature = %v, want live signature %v", replayed["signature"], live["signature"])
	}
	if replayed["order"] != live["order"] {
		t.Fatalf("replay order = %v, want live order %v", replayed["order"], live["order"])
	}

	handoff := page.evalObject(t, `(() => {
		const before = document.querySelector('.msg.thought[data-message-id="t1"]');
		const count = document.querySelectorAll('.msg.thought').length;
		handleMessage({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'thought-session-1', update: {
				sessionUpdate: 'agent_thought_chunk', messageId: 't1',
				content: {type: 'text', text: '!'}
			}
		}});
		const after = document.querySelector('.msg.thought[data-message-id="t1"]');
		return {same: before === after, countStable: count === document.querySelectorAll('.msg.thought').length,
			raw: after && after._text, text: after && after.textContent};
	})()`)
	if handoff["same"] != true || handoff["countStable"] != true ||
		handoff["raw"] != "**Inspecting the renderer**!" || handoff["text"] != "Inspecting the renderer!" {
		t.Fatalf("replay-to-live handoff state = %#v", handoff)
	}

	localUserBoundary := page.evalObject(t, `(() => {
		handleMessage({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'thought-session-1', update: {
				sessionUpdate: 'agent_thought_chunk', content: {type: 'text', text: 'before user'}
			}
		}});
		const before = [...document.querySelectorAll('.msg.thought:not([data-message-id])')].at(-1);
		addUserMsg('Local user boundary', 'sending');
		handleMessage({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'thought-session-1', update: {
				sessionUpdate: 'agent_thought_chunk', content: {type: 'text', text: 'after user'}
			}
		}});
		const anonymous = [...document.querySelectorAll('.msg.thought:not([data-message-id])')];
		const after = anonymous.at(-1);
		return {distinct: before !== after, beforeText: before.textContent, afterText: after.textContent,
			userBetween: before.nextElementSibling && before.nextElementSibling.classList.contains('user') &&
				before.nextElementSibling.nextElementSibling === after};
	})()`)
	if localUserBoundary["distinct"] != true || localUserBoundary["beforeText"] != "before user" ||
		localUserBoundary["afterText"] != "after user" || localUserBoundary["userBetween"] != true {
		t.Fatalf("local user thought boundary = %#v", localUserBoundary)
	}

	localSystemBoundary := page.evalObject(t, `(() => {
		endAnonymousThoughtRun();
		handleMessage({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'thought-session-1', update: {
				sessionUpdate: 'agent_thought_chunk', content: {type: 'text', text: 'before system'}
			}
		}});
		const before = [...document.querySelectorAll('.msg.thought:not([data-message-id])')].at(-1);
		addSystemMsg('Local system boundary');
		handleMessage({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'thought-session-1', update: {
				sessionUpdate: 'agent_thought_chunk', content: {type: 'text', text: 'after system'}
			}
		}});
		const anonymous = [...document.querySelectorAll('.msg.thought:not([data-message-id])')];
		const after = anonymous.at(-1);
		return {distinct: before !== after, beforeText: before.textContent, afterText: after.textContent,
			systemBetween: before.nextElementSibling && before.nextElementSibling.classList.contains('system') &&
				before.nextElementSibling.nextElementSibling === after};
	})()`)
	if localSystemBoundary["distinct"] != true || localSystemBoundary["beforeText"] != "before system" ||
		localSystemBoundary["afterText"] != "after system" || localSystemBoundary["systemBetween"] != true {
		t.Fatalf("local system thought boundary = %#v", localSystemBoundary)
	}

	reset := page.evalObject(t, `(() => {
		showNavigator();
		return {identified: thoughtMsgsById.size, anonymousCleared: currentAnonymousThought === null};
	})()`)
	if reset["identified"] != float64(0) || reset["anonymousCleared"] != true {
		t.Fatalf("navigator thought reset = %#v", reset)
	}

	page.call(t, "Page.reload", map[string]interface{}{})
	openThoughtFixtureSession(t, page)
	page.waitFor(t, `replayMode === false && [...document.querySelectorAll('.msg.thought:not([data-message-id])')]
		.some(el => el._text === 'cached anonymous ')`)
	seed := page.evalObject(t, `(() => {
		const thought = [...document.querySelectorAll('.msg.thought:not([data-message-id])')]
			.find(el => el._text === 'cached anonymous ');
		thought.dataset.replayAnonymous = 'true';
		return {found: !!thought, raw: thought && thought._text};
	})()`)
	if seed["found"] != true || seed["raw"] != "cached anonymous " {
		t.Fatalf("anonymous replay seed = %#v", seed)
	}
	anonymousReleaseOnce.Do(func() { close(anonymousRelease) })
	page.waitFor(t, `[...document.querySelectorAll('.msg.thought:not([data-message-id])')]
		.some(el => (el._text || '').includes('continuation'))`)
	anonymousHandoff := page.evalObject(t, `(() => {
		const thoughts = [...document.querySelectorAll('.msg.thought:not([data-message-id])')];
		const replayed = thoughts.find(el => el.dataset.replayAnonymous === 'true');
		const continuationRows = thoughts.filter(el =>
			el.dataset.replayAnonymous === 'true' || el._text === 'continuation');
		return {
			count: continuationRows.length,
			raw: replayed && replayed._text,
			text: replayed && replayed.textContent
		};
	})()`)
	if anonymousHandoff["count"] != float64(1) ||
		anonymousHandoff["raw"] != "cached anonymous continuation" ||
		anonymousHandoff["text"] != "cached anonymous continuation" {
		t.Fatalf("anonymous replay-to-live handoff = %#v", anonymousHandoff)
	}
}

func loadThoughtReplayFixture(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("testdata/thought-replay.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for number, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("thought replay fixture line %d is invalid JSON", number+1)
		}
		messages = append(messages, line)
	}
	if len(messages) < 5 {
		t.Fatalf("thought replay fixture has %d messages, want at least 5", len(messages))
	}
	return messages
}

func openThoughtFixtureSession(t *testing.T, page *chromePage) {
	t.Helper()
	page.waitFor(t, `document.querySelector('.session-card') !== null`)
	page.eval(t, `document.querySelector('.session-card').click()`)
	page.waitFor(t, `document.getElementById('chat-view').classList.contains('visible')`)
}

func thoughtProgressState(t *testing.T, page *chromePage) map[string]interface{} {
	t.Helper()
	return page.evalObject(t, `(() => {
		const thoughts = [...document.querySelectorAll('#messages > .msg.thought')];
		const byId = id => thoughts.find(el => el.dataset.messageId === id);
		const first = byId('t1');
		const rich = byId('t3');
		const safe = byId('safe');
		const toolCard = document.querySelector('#messages > .msg.tool');
		const messages = document.getElementById('messages');
		const overflow = thoughts.some(el => {
			const rect = el.getBoundingClientRect();
			const parent = messages.getBoundingClientRect();
			return rect.left < parent.left - 1 || rect.right > parent.right + 1 ||
				el.scrollWidth > el.clientWidth + 1;
		});
		const order = [...messages.children].filter(el => el.classList.contains('msg')).map(el => {
			if (el.classList.contains('thought')) return 'thought:' + (el.dataset.messageId || 'anon');
			if (el.classList.contains('tool')) return 'tool:' + ((el._toolState && el._toolState.id) || '');
			if (el.classList.contains('user')) return 'user';
			if (el.classList.contains('agent')) return 'agent';
			return 'other';
		}).join('|');
		return {
			count: thoughts.length,
			ids: thoughts.map(el => el.dataset.messageId || '').join('|'),
			signature: JSON.stringify(thoughts.map(el => ({id: el.dataset.messageId || '', text: el.textContent, html: el.querySelector('.md') && el.querySelector('.md').innerHTML}))),
			order,
			firstRaw: first && first._text,
			firstText: first && first.textContent,
			firstStrong: first && first.querySelector('strong') && first.querySelector('strong').textContent,
			firstHasLiteralDelimiter: first && first.textContent.includes('**'),
			containerFontStyle: first && getComputedStyle(first).fontStyle,
			containerBackground: first && getComputedStyle(first).backgroundColor,
			toolBackground: toolCard && getComputedStyle(toolCard).backgroundColor,
			containerBorderLeft: first && getComputedStyle(first).borderLeft,
			toolBorderLeft: toolCard && getComputedStyle(toolCard).borderLeft,
			containerBorderRadius: first && getComputedStyle(first).borderRadius,
			toolBorderRadius: toolCard && getComputedStyle(toolCard).borderRadius,
			containerPadding: first && getComputedStyle(first).padding,
			toolPadding: toolCard && getComputedStyle(toolCard).padding,
			hasToolAffordance: thoughts.some(el => el.querySelector('.tool-expand, .tool-hint')),
			strongWeight: first && Number.parseInt(getComputedStyle(first.querySelector('strong')).fontWeight, 10),
			richHeading: rich && rich.querySelector('h1') && rich.querySelector('h1').textContent,
			richHeadingSize: rich && rich.querySelector('h1') && Number.parseFloat(getComputedStyle(rich.querySelector('h1')).fontSize),
			strongSize: first && first.querySelector('strong') && Number.parseFloat(getComputedStyle(first.querySelector('strong')).fontSize),
			richParagraphs: rich && rich.querySelectorAll('p').length,
			richCode: rich && rich.querySelector('code') && rich.querySelector('code').textContent,
			richEmphasisStyle: rich && rich.querySelector('em') && getComputedStyle(rich.querySelector('em')).fontStyle,
			hasRoleLabel: thoughts.some(el => el.querySelector('.role')),
			hasExecutableElement: !!(safe && safe.querySelector('img, script, [onerror]')),
			hasUnsafeLink: !!(safe && safe.querySelector('a[href^="javascript:"]')),
			safeLink: safe && safe.querySelector('a') && safe.querySelector('a').href,
			safeTextContainsHTML: safe && safe.textContent.includes('<img src=x onerror="alert(\'boom\')">'),
			overflow
		};
	})()`)
}

func assertThoughtProgressState(t *testing.T, state map[string]interface{}) {
	t.Helper()
	if state["count"] != float64(6) {
		t.Fatalf("thought count = %v, want 6: %#v", state["count"], state)
	}
	if state["ids"] != "t1|t2|t3|||safe" {
		t.Fatalf("thought IDs = %v", state["ids"])
	}
	wantOrder := "user|thought:t1|tool:tool-read|thought:t2|thought:t3|thought:anon|tool:tool-boundary|thought:anon|thought:safe|agent"
	if state["order"] != wantOrder {
		t.Fatalf("message order = %v, want %v", state["order"], wantOrder)
	}
	if state["firstRaw"] != "**Inspecting the renderer**" ||
		state["firstText"] != "Inspecting the renderer" ||
		state["firstStrong"] != "Inspecting the renderer" || state["firstHasLiteralDelimiter"] != false {
		t.Fatalf("split Markdown state = %#v", state)
	}
	if state["containerFontStyle"] != "normal" || state["containerBackground"] == "rgba(0, 0, 0, 0)" ||
		state["containerBackground"] != state["toolBackground"] ||
		state["containerBorderLeft"] != state["toolBorderLeft"] ||
		state["containerBorderRadius"] != state["toolBorderRadius"] ||
		state["containerPadding"] != state["toolPadding"] || state["hasToolAffordance"] != false ||
		state["strongWeight"].(float64) < 600 {
		t.Fatalf("thought card/typography state = %#v", state)
	}
	if state["richHeading"] != "Layout check" || state["richParagraphs"].(float64) < 2 ||
		state["richCode"] != "inline code" || state["richEmphasisStyle"] != "italic" {
		t.Fatalf("rich Markdown state = %#v", state)
	}
	if state["richHeadingSize"].(float64) > state["strongSize"].(float64) {
		t.Fatalf("thought heading size = %v, want no larger than emphasized caption size %v",
			state["richHeadingSize"], state["strongSize"])
	}
	if state["hasRoleLabel"] != false || state["hasExecutableElement"] != false ||
		state["hasUnsafeLink"] != false || state["safeLink"] != "https://example.test/path" ||
		state["safeTextContainsHTML"] != true || state["overflow"] != false {
		t.Fatalf("thought safety/layout state = %#v", state)
	}
}

func TestHistorySearchDockOpensSeparateHistoryAndRefines(t *testing.T) {
	var mu sync.Mutex
	queries := []string{}
	var transcriptMu sync.Mutex
	transcriptCalls := 0
	transcriptStarted := make(chan struct{}, 1)
	transcriptFinished := make(chan struct{}, 1)
	transcriptRelease := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
	})
	mux.HandleFunc("/api/transcripts", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]interface{}{})
	})
	mux.HandleFunc("/api/preview", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"messages": []interface{}{}})
	})
	mux.HandleFunc("/api/transcript-search", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		queries = append(queries, req.Query)
		mu.Unlock()
		matchLine := 6
		if req.Query == "**orbit**" {
			matchLine = 9
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"query": req.Query,
			"results": []map[string]interface{}{{
				"file": "/tmp/fake/.agent-shell/transcripts/one.md", "project": "syzygy",
				"timestamp": "2026-09-01-12-00-00", "agent": "Codex",
				"preview": "Conversation preview", "sessionId": "session-1",
				"label": "Result " + req.Query, "snippet": "Matched " + req.Query,
				"matchField": "label", "matchCount": 1, "matchLine": matchLine,
			}},
			"truncated": false,
		})
	})
	mux.HandleFunc("/api/transcript", func(w http.ResponseWriter, r *http.Request) {
		transcriptMu.Lock()
		transcriptCalls++
		call := transcriptCalls
		transcriptMu.Unlock()
		if call == 2 {
			transcriptStarted <- struct{}{}
			select {
			case <-transcriptRelease:
			case <-r.Context().Done():
			}
			transcriptFinished <- struct{}{}
		}
		json.NewEncoder(w).Encode(map[string]string{"content": strings.Join([]string{
			"**Agent:** Codex", "", "---", "",
			"## User (2026-09-01 12:00)", "Please recall the orbit decision.", "",
			"## Agent (2026-09-01 12:01)", "The **orbit** decision is recorded.", "",
		}, "\n")})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	page := openChromePage(t, server.URL)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{
		"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": true,
	})
	page.call(t, "Page.reload", map[string]interface{}{})
	page.waitFor(t, `document.readyState === "complete" && document.getElementById('history-search-input') !== null`)
	state := page.evalObject(t, `(() => {
		const input = document.getElementById('history-search-input');
		const formRect = document.getElementById('history-search-form').getBoundingClientRect();
		const spawnRect = document.getElementById('spawn-btn').getBoundingClientRect();
		input.focus();
		return {
			historyVisible: document.getElementById('history').classList.contains('visible'),
			dockVisible: document.getElementById('history-dock').classList.contains('visible'),
			focused: document.activeElement === input,
			formRight: formRect.right,
			spawnLeft: spawnRect.left,
			spawnRight: spawnRect.right,
			spawnWidth: spawnRect.width,
			viewportWidth: window.innerWidth
		};
	})()`)
	if state["historyVisible"] != true || state["dockVisible"] != true || state["focused"] != true {
		t.Fatalf("focus state = %#v, want separate History open with persistent focused dock", state)
	}
	if state["spawnWidth"].(float64) < 47 || state["spawnRight"].(float64) > state["viewportWidth"].(float64) ||
		state["formRight"].(float64) >= state["spawnLeft"].(float64) {
		t.Fatalf("mobile dock geometry = %#v, want a full New Chat circle beside the search capsule", state)
	}

	page.eval(t, `(() => {
		const input = document.getElementById('history-search-input');
		input.value = 'orbit';
		document.getElementById('history-search-form').dispatchEvent(
			new Event('submit', {bubbles: true, cancelable: true}));
	})()`)
	page.waitFor(t, `document.querySelector('.hist-card') && document.querySelector('.hist-card').textContent.includes('Result orbit')`)

	page.eval(t, `(() => {
		const input = document.getElementById('history-search-input');
		input.value = 'recall';
		document.getElementById('history-search-form').dispatchEvent(
			new Event('submit', {bubbles: true, cancelable: true}));
	})()`)
	page.waitFor(t, `document.querySelector('.hist-card') && document.querySelector('.hist-card').textContent.includes('Result recall')`)
	state = page.evalObject(t, `(() => ({
		historyVisible: document.getElementById('history').classList.contains('visible'),
		dockVisible: document.getElementById('history-dock').classList.contains('visible'),
		newChatVisible: !!document.getElementById('spawn-btn').getClientRects().length,
		value: document.getElementById('history-search-input').value
	}))()`)
	if state["historyVisible"] != true || state["dockVisible"] != true ||
		state["newChatVisible"] != true || state["value"] != "recall" {
		t.Fatalf("refined state = %#v", state)
	}
	page.eval(t, `document.querySelector('.hist-card').click()`)
	page.waitFor(t, `document.querySelector('#history-body .pv-msg') !== null`)
	state = page.evalObject(t, `(() => ({
		dockVisible: document.getElementById('history-dock').classList.contains('visible'),
		highlighted: !!document.querySelector('#history-body .hist-match mark'),
		backVisible: !!document.getElementById('history-back').getClientRects().length
	}))()`)
	if state["dockVisible"] != true || state["highlighted"] != true || state["backVisible"] != true {
		t.Fatalf("opened match state = %#v", state)
	}
	page.eval(t, `document.getElementById('history-back').click()`)
	page.waitFor(t, `document.querySelector('.hist-card') && document.querySelector('.hist-card').textContent.includes('Result recall')`)
	state = page.evalObject(t, `(() => ({
		value: document.getElementById('history-search-input').value,
		dockVisible: document.getElementById('history-dock').classList.contains('visible')
	}))()`)
	if state["value"] != "recall" || state["dockVisible"] != true {
		t.Fatalf("restored search state = %#v", state)
	}
	page.eval(t, `document.querySelector('.hist-card').click()`)
	select {
	case <-transcriptStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed transcript request")
	}
	page.eval(t, `document.getElementById('history-back').click()`)
	close(transcriptRelease)
	select {
	case <-transcriptFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed transcript request to finish")
	}
	time.Sleep(250 * time.Millisecond)
	state = page.evalObject(t, `(() => ({
		cardVisible: !!document.querySelector('.hist-card'),
		transcriptVisible: !!document.querySelector('#history-body .pv-msg'),
		value: document.getElementById('history-search-input').value
	}))()`)
	if state["cardVisible"] != true || state["transcriptVisible"] != false || state["value"] != "recall" {
		t.Fatalf("state after Back during transcript fetch = %#v, want restored search to win", state)
	}
	page.eval(t, `(() => {
		const input = document.getElementById('history-search-input');
		input.value = '**orbit**';
		document.getElementById('history-search-form').dispatchEvent(
			new Event('submit', {bubbles: true, cancelable: true}));
	})()`)
	page.waitFor(t, `document.querySelector('.hist-card') && document.querySelector('.hist-card').textContent.includes('Result **orbit**')`)
	page.eval(t, `document.querySelector('.hist-card').click()`)
	page.waitFor(t, `document.querySelector('#history-body .pv-msg') !== null`)
	state = page.evalObject(t, `(() => ({
		outlined: !!document.querySelector('#history-body .hist-match'),
		highlighted: !!document.querySelector('#history-body .hist-match mark')
	}))()`)
	if state["outlined"] != true || state["highlighted"] != false {
		t.Fatalf("markdown-only raw match state = %#v, want line-located outline without inline mark", state)
	}
	page.eval(t, `document.getElementById('history-back').click()`)
	page.eval(t, `document.getElementById('history-close').click()`)
	page.eval(t, `document.getElementById('nav-pins-btn').click()`)
	state = page.evalObject(t, `(() => ({
		pinsVisible: document.getElementById('pins').classList.contains('visible'),
		dockVisible: document.getElementById('history-dock').classList.contains('visible')
	}))()`)
	if state["pinsVisible"] != true || state["dockVisible"] != false {
		t.Fatalf("pins state = %#v, want dock hidden", state)
	}
	page.eval(t, `document.getElementById('pins-close').click()`)
	page.eval(t, `document.getElementById('spawn-btn').click()`)
	state = page.evalObject(t, `(() => ({
		spawnVisible: document.getElementById('spawn-sheet').classList.contains('visible'),
		dockVisible: document.getElementById('history-dock').classList.contains('visible')
	}))()`)
	if state["spawnVisible"] != true || state["dockVisible"] != false {
		t.Fatalf("spawn state = %#v, want dock hidden", state)
	}
	page.eval(t, `document.getElementById('sp-close').click()`)
	page.eval(t, `showPreview({pid: 1}, 'preview')`)
	state = page.evalObject(t, `(() => {
		const dock = document.getElementById('history-dock');
		const preview = document.getElementById('preview-sheet');
		return {
			previewVisible: preview.classList.contains('visible'),
			dockVisible: dock.classList.contains('visible'),
			dockZ: getComputedStyle(dock).zIndex,
			previewZ: getComputedStyle(preview).zIndex
		};
	})()`)
	if state["previewVisible"] != true || state["dockVisible"] != false {
		t.Fatalf("preview state = %#v, want dock hidden behind preview", state)
	}
	page.eval(t, `hidePreview()`)
	page.eval(t, `showChat()`)
	state = page.evalObject(t, `({dockVisible: document.getElementById('history-dock').classList.contains('visible')})`)
	if state["dockVisible"] != false {
		t.Fatalf("chat state = %#v, want dock hidden", state)
	}
	page.eval(t, `showNavigator()`)
	state = page.evalObject(t, `({dockVisible: document.getElementById('history-dock').classList.contains('visible')})`)
	if state["dockVisible"] != true {
		t.Fatalf("navigator state = %#v, want dock restored", state)
	}
	state = page.evalObject(t, `(() => {
		const vv = window.visualViewport;
		if (!vv) return {supported: false};
		document.getElementById('history-search-input').blur();
		Object.defineProperty(vv, 'height', {configurable: true, value: window.innerHeight - 80});
		Object.defineProperty(vv, 'offsetTop', {configurable: true, value: 0});
		vv.dispatchEvent(new Event('resize'));
		return {
			supported: true,
			bottom: document.getElementById('history-dock').style.bottom,
			bodyHeight: document.body.style.height
		};
	})()`)
	if state["supported"] == true && (state["bottom"] != "" || state["bodyHeight"] != "") {
		t.Fatalf("launch viewport state = %#v, want CSS-controlled body and dock without focused input", state)
	}
	state = page.evalObject(t, `(() => {
		const vv = window.visualViewport;
		if (!vv) return {supported: false};
		document.getElementById('history-search-input').focus();
		Object.defineProperty(vv, 'height', {configurable: true, value: window.innerHeight - 200});
		Object.defineProperty(vv, 'offsetTop', {configurable: true, value: 0});
		vv.dispatchEvent(new Event('resize'));
		return {
			supported: true,
			bottom: document.getElementById('history-dock').style.bottom,
			bodyHeight: document.body.style.height
		};
	})()`)
	if state["supported"] == true && (state["bottom"] == "" || state["bodyHeight"] == "") {
		t.Fatalf("keyboard state = %#v, want dock lifted above visual viewport", state)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(queries) != "[orbit recall **orbit**]" {
		t.Fatalf("queries = %v, want [orbit recall **orbit**]", queries)
	}
}

type chromePage struct {
	ws     *websocket.Conn
	nextID int
}

func openChromePage(t *testing.T, pageURL string) *chromePage {
	t.Helper()
	chrome := chromeExecutable()
	if chrome == "" {
		t.Skip("Chrome not installed; skipping browser interaction test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	profile := t.TempDir()
	cmd := exec.CommandContext(ctx, chrome,
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--remote-debugging-port=0",
		"--remote-debugging-address=127.0.0.1", "--remote-allow-origins=*",
		"--user-data-dir="+profile, "about:blank")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	lineCh := make(chan string, 20)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
		close(lineCh)
	}()
	devtoolsRE := regexp.MustCompile(`DevTools listening on (ws://\S+)`)
	var browserWS string
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for browserWS == "" {
		select {
		case line, ok := <-lineCh:
			if !ok {
				t.Fatal("Chrome exited before exposing DevTools")
			}
			if match := devtoolsRE.FindStringSubmatch(line); match != nil {
				browserWS = match[1]
			}
		case <-timer.C:
			t.Fatal("timed out waiting for Chrome DevTools")
		}
	}
	debugURL, err := url.Parse(browserWS)
	if err != nil {
		t.Fatal(err)
	}
	requestURL := "http://" + debugURL.Host + "/json/new?" + url.QueryEscape(pageURL)
	req, err := http.NewRequest(http.MethodPut, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var target struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.Dial(target.WebSocketDebuggerURL, "", "http://"+debugURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return &chromePage{ws: ws}
}

func chromeExecutable() string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

func (p *chromePage) call(t *testing.T, method string, params interface{}) json.RawMessage {
	t.Helper()
	p.nextID++
	id := p.nextID
	if err := websocket.JSON.Send(p.ws, map[string]interface{}{
		"id": id, "method": method, "params": params,
	}); err != nil {
		t.Fatal(err)
	}
	for {
		var raw string
		if err := websocket.Message.Receive(p.ws, &raw); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil || envelope.ID != id {
			continue
		}
		if envelope.Error != nil {
			t.Fatalf("CDP %s: %s", method, envelope.Error.Message)
		}
		return envelope.Result
	}
}

func (p *chromePage) eval(t *testing.T, expression string) interface{} {
	t.Helper()
	raw := p.call(t, "Runtime.evaluate", map[string]interface{}{
		"expression": expression, "returnByValue": true, "awaitPromise": true,
	})
	var response struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Exception) > 0 && string(response.Exception) != "null" {
		t.Fatalf("browser evaluation failed: %s", response.Exception)
	}
	if len(response.Result.Value) == 0 {
		return nil
	}
	var value interface{}
	if err := json.Unmarshal(response.Result.Value, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (p *chromePage) evalObject(t *testing.T, expression string) map[string]interface{} {
	t.Helper()
	value := p.eval(t, expression)
	object, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("evaluation returned %T, want object: %v", value, value)
	}
	return object
}

func (p *chromePage) waitFor(t *testing.T, expression string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if value, ok := p.eval(t, expression).(bool); ok && value {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for browser condition: %s", strings.TrimSpace(expression))
}
