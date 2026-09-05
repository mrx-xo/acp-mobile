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

func TestSentImageRemainsVisibleInUserMessage(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	page := openChromePage(t, server.URL)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{
		"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": true,
	})
	page.call(t, "Page.reload", map[string]interface{}{})
	page.waitFor(t, `typeof sendPromptText === 'function'`)
	state := page.evalObject(t, `(() => {
		const pixel = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=';
		messagesEl.innerHTML = '';
		showChat();
		sessionId = 'sent-image-session';
		ws = {readyState: 1, send: raw => { window.__sentImagePrompt = JSON.parse(raw); }};
		pendingImages = [{
			data: pixel,
			mimeType: 'image/png',
			thumb: 'data:image/png;base64,' + pixel
		}];
		sendPromptText('inspect this', []);
		const message = document.querySelector('#messages > .msg.user');
		const images = message ? [...message.querySelectorAll('img')] : [];
		const imageRect = images[0] ? images[0].getBoundingClientRect() : null;
		const messageRect = message ? message.getBoundingClientRect() : null;
		if (images[0]) images[0].click();
		const readerImage = document.querySelector('#reader-body .sent-images img');
		const result = {
			messageCount: document.querySelectorAll('#messages > .msg.user').length,
			imageCount: images.length,
			imageSource: images[0] ? images[0].getAttribute('src') : '',
			imageAlt: images[0] ? images[0].getAttribute('alt') : '',
			imageWidth: imageRect ? imageRect.width : 0,
			imageInsideMessage: !!(imageRect && messageRect &&
				imageRect.left >= messageRect.left && imageRect.right <= messageRect.right),
			text: message && message.querySelector('.text') ? message.querySelector('.text').textContent : '',
			placeholder: message ? message.textContent.includes('[1 image attached]') : false,
			promptTypes: window.__sentImagePrompt.params.prompt.map(block => block.type).join('|'),
			readerVisible: document.getElementById('reader').classList.contains('visible'),
			readerImageCount: document.querySelectorAll('#reader-body .sent-images img').length,
			readerImageMaxHeight: readerImage ? getComputedStyle(readerImage).maxHeight : '',
			normalizedImageKeys: message && message._images[0]
				? Object.keys(message._images[0]).sort().join('|') : ''
		};
		closeReader();
		result.readerCleared = document.getElementById('reader-body').childElementCount === 0;
		currentBufferName = 'image-pin-fixture';
		localStorage.removeItem(pinsKey());
		openMsgMenu(message);
		result.pinActionHidden = getComputedStyle(mmPin).display === 'none';
		closeMsgMenu();
		result.menuTargetCleared = mmTarget === null;
		togglePin(message);
		result.pinPersisted = loadPins().length > 0;
		localStorage.removeItem(pinsKey());
		return result;
	})()`)
	if state["messageCount"] != float64(1) || state["imageCount"] != float64(1) ||
		state["imageSource"] != "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=" ||
		state["imageAlt"] != "Attached image 1" || state["imageWidth"].(float64) < 200 ||
		state["imageInsideMessage"] != true || state["text"] != "inspect this" ||
		state["placeholder"] != false || state["promptTypes"] != "image|text" ||
		state["readerVisible"] != true || state["readerImageCount"] != float64(1) ||
		state["readerImageMaxHeight"] != "none" ||
		state["normalizedImageKeys"] != "data|mimeType" || state["readerCleared"] != true ||
		state["pinActionHidden"] != true || state["menuTargetCleared"] != true ||
		state["pinPersisted"] != false {
		t.Fatalf("sent image message state = %#v", state)
	}
}

func TestRemoteUserMessageBoundariesMatchReplay(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof handleMessage === 'function' && typeof flushReplay === 'function'`)
	state := page.evalObject(t, `(() => {
		const userUpdate = text => ({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'boundary-session',
			update: {sessionUpdate: 'user_message_chunk', content: {type: 'text', text}}
		}});
		const permission = {jsonrpc: '2.0', id: 91, method: 'session/request_permission', params: {
			sessionId: 'boundary-session', toolCall: {title: 'Boundary fixture'}, options: []
		}};
		const signature = () => [...messagesEl.children].map(message => {
			if (message.classList.contains('user')) {
				return 'user:' + [...message.querySelectorAll('.text')]
					.map(element => element.textContent).join('|');
			}
			if (message.classList.contains('permission')) return 'permission';
			return [...message.classList].join('.');
		}).join(',');
		const reset = () => {
			messagesEl.innerHTML = '';
			currentUserMsg = null;
			pendingPermissions = [];
		};

		reset();
		handleMessage(userUpdate('before response'));
		handleMessage({jsonrpc: '2.0', id: 90, result: {}});
		handleMessage(userUpdate('after response'));
		const liveResponse = signature();

		reset();
		handleMessage(userUpdate('before permission'));
		handleMessage(permission);
		handleMessage(userUpdate('after permission'));
		const livePermission = signature();

		reset();
		replayBuffer = [
			userUpdate('before response'),
			{jsonrpc: '2.0', id: 90, result: {}},
			userUpdate('after response')
		];
		replayMode = true;
		flushReplay();
		const replayResponse = signature();

		reset();
		replayBuffer = [
			userUpdate('before permission'),
			permission,
			userUpdate('after permission')
		];
		replayMode = true;
		flushReplay();
		const replayPermission = signature();

		return {liveResponse, livePermission, replayResponse, replayPermission};
	})()`)
	if state["liveResponse"] != "user:before response,user:after response" ||
		state["replayResponse"] != state["liveResponse"] ||
		state["livePermission"] != "user:before permission,permission,user:after permission" ||
		state["replayPermission"] != state["livePermission"] {
		t.Fatalf("remote user boundary state = %#v", state)
	}
}

func TestRemoteUserImagesRenderInLiveAndReplayPaths(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof handleMessage === 'function' && typeof flushReplay === 'function'`)
	state := page.evalObject(t, `(() => {
		const pixel = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=';
		const userUpdate = content => ({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'remote-image-session',
			update: {sessionUpdate: 'user_message_chunk', content}
		}});
		const agentUpdate = text => ({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'remote-image-session',
			update: {sessionUpdate: 'agent_message_chunk', content: {type: 'text', text}}
		}});

		messagesEl.innerHTML = '';
		handleMessage(userUpdate({type: 'image', data: pixel, mimeType: 'image/png'}));
		const firstLiveImage = document.querySelector('#messages > .msg.user img');
		handleMessage(userUpdate({type: 'image', data: pixel, mimeType: 'image/png'}));
		handleMessage(userUpdate({type: 'text', text: 'remote caption'}));
		const liveMessage = document.querySelector('#messages > .msg.user');
		const live = {
			messageCount: document.querySelectorAll('#messages > .msg.user').length,
			imageCount: liveMessage ? liveMessage.querySelectorAll('img').length : 0,
			multiple: !!(liveMessage && liveMessage.querySelector('.sent-images.multiple')),
			text: liveMessage && liveMessage.querySelector('.text') ? liveMessage.querySelector('.text').textContent : '',
			firstImageStable: liveMessage ? liveMessage.querySelector('img') === firstLiveImage : false
		};
		handleMessage(agentUpdate('boundary'));
		handleMessage(userUpdate({type: 'text', text: 'next turn'}));
		live.afterBoundaryCount = document.querySelectorAll('#messages > .msg.user').length;
		live.afterBoundaryLastText = [...document.querySelectorAll('#messages > .msg.user .text')].at(-1).textContent;
		allReplayTurns = [{type: 'user', text: '', images: [{data: pixel, mimeType: 'image/png'}]}];
		replayBuffer = [userUpdate({type: 'image', data: pixel, mimeType: 'image/png'})];
		replayTimer = setTimeout(() => {}, 10000);
		lastSentMsg = liveMessage;
		openMsgMenu(liveMessage);
		openReader(liveMessage);
		showOrrery();
		live.navigatorReset = currentUserMsg === null;
		live.navigatorReleased = allReplayTurns.length === 0 && replayBuffer.length === 0 &&
			messagesEl.childElementCount === 0 && readerBody.childElementCount === 0 &&
			lastSentMsg === null && mmTarget === null && replayTimer === null;

		messagesEl.innerHTML = '';
		allReplayTurns = [];
		replayBuffer = [
			{id: 1, result: {agentInfo: {name: 'fixture'}}},
			{id: 2, result: {sessionId: 'remote-image-session'}},
			userUpdate({type: 'image', data: pixel, mimeType: 'image/png'}),
			{jsonrpc: '2.0', method: 'session/update', params: {
				sessionId: 'remote-image-session', update: {sessionUpdate: 'turn_complete'}
			}}
		];
		replayMode = true;
		flushReplay();
		const replayMessage = document.querySelector('#messages > .msg.user');
		const replay = {
			messageCount: document.querySelectorAll('#messages > .msg.user').length,
			imageCount: replayMessage ? replayMessage.querySelectorAll('img').length : 0,
			text: replayMessage && replayMessage.querySelector('.text') ? replayMessage.querySelector('.text').textContent : '',
			turnCount: allReplayTurns.length,
			turnType: allReplayTurns[0] ? allReplayTurns[0].type : ''
		};

		messagesEl.innerHTML = '';
		currentUserMsg = null;
		handleMessage(userUpdate({
			type: 'image', data: 'PHN2Zz48L3N2Zz4=', mimeType: 'image/svg+xml'
		}));
		const unsafe = {
			messageCount: document.querySelectorAll('#messages > .msg.user').length,
			imageCount: document.querySelectorAll('#messages > .msg.user img').length
		};

		const mixedUpdates = [
			userUpdate({type: 'text', text: 'before image'}),
			userUpdate({type: 'image', data: pixel, mimeType: 'image/png'}),
			userUpdate({type: 'text', text: 'after image'})
		];
		const contentSignature = message => [...message.children]
			.filter(element => !element.classList.contains('role') && !element.classList.contains('meta'))
			.map(element => element.classList.contains('text')
				? 'text:' + element.textContent
				: element.classList.contains('sent-images')
					? 'images:' + element.querySelectorAll('img').length
					: element.className)
			.join('|');
		messagesEl.innerHTML = '';
		currentUserMsg = null;
		mixedUpdates.forEach(handleMessage);
		const mixedLive = contentSignature(document.querySelector('#messages > .msg.user'));
		messagesEl.innerHTML = '';
		currentUserMsg = null;
		replayBuffer = [...mixedUpdates, {jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'remote-image-session', update: {sessionUpdate: 'turn_complete'}
		}}];
		replayMode = true;
		flushReplay();
		const mixedReplay = contentSignature(document.querySelector('#messages > .msg.user'));

		messagesEl.innerHTML = '';
		currentUserMsg = null;
		replayBuffer = [
			userUpdate({type: 'image', data: 'PHN2Zz48L3N2Zz4=', mimeType: 'image/svg+xml'}),
			{jsonrpc: '2.0', method: 'session/update', params: {
				sessionId: 'remote-image-session', update: {sessionUpdate: 'turn_complete'}
			}}
		];
		replayMode = true;
		flushReplay();
		const unsafeReplay = {
			messageCount: document.querySelectorAll('#messages > .msg.user').length,
			imageCount: document.querySelectorAll('#messages > .msg.user img').length
		};
		return {live, replay, unsafe, unsafeReplay, mixedLive, mixedReplay};
	})()`)
	live := state["live"].(map[string]interface{})
	if live["messageCount"] != float64(1) || live["imageCount"] != float64(2) ||
		live["multiple"] != true || live["text"] != "remote caption" ||
		live["firstImageStable"] != true ||
		live["afterBoundaryCount"] != float64(2) || live["afterBoundaryLastText"] != "next turn" ||
		live["navigatorReset"] != true || live["navigatorReleased"] != true {
		t.Fatalf("live remote image state = %#v", live)
	}
	replay := state["replay"].(map[string]interface{})
	if replay["messageCount"] != float64(1) || replay["imageCount"] != float64(1) ||
		replay["text"] != "" || replay["turnCount"] != float64(1) || replay["turnType"] != "user" {
		t.Fatalf("replayed remote image state = %#v", replay)
	}
	unsafe := state["unsafe"].(map[string]interface{})
	if unsafe["messageCount"] != float64(0) || unsafe["imageCount"] != float64(0) {
		t.Fatalf("unsafe remote image state = %#v", unsafe)
	}
	unsafeReplay := state["unsafeReplay"].(map[string]interface{})
	if unsafeReplay["messageCount"] != float64(0) || unsafeReplay["imageCount"] != float64(0) {
		t.Fatalf("unsafe replay image state = %#v", unsafeReplay)
	}
	if state["mixedLive"] != "text:before image|images:1|text:after image" ||
		state["mixedReplay"] != state["mixedLive"] {
		t.Fatalf("mixed user content state = %#v", state)
	}
}

func TestReplayImageMemoryBudgetKeepsNewestImages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof limitReplayImageMemory === 'function'`)
	state := page.evalObject(t, `(() => {
		const turns = [
			{type: 'user', blocks: [{type: 'image', data: 'aaaa', mimeType: 'image/png'}]},
			{type: 'user', blocks: [{type: 'image', data: 'bbbb', mimeType: 'image/png'}]},
			{type: 'user', blocks: [{type: 'image', data: 'cccc', mimeType: 'image/png'}]}
		];
		limitReplayImageMemory(turns, 8);
		resetReplayBuffer();
		const replayMessage = data => ({jsonrpc: '2.0', method: 'session/update', params: {
			sessionId: 'memory-session', update: {sessionUpdate: 'user_message_chunk',
				content: {type: 'image', data, mimeType: 'image/png'}}
		}});
		bufferReplayMessage(replayMessage('aaaa'), 8);
		bufferReplayMessage(replayMessage('bbbb'), 8);
		bufferReplayMessage(replayMessage('cccc'), 8);
		return {
			types: turns.map(turn => turn.blocks[0].type).join('|'),
			reasons: turns.map(turn => turn.blocks[0].reason || '').join('|'),
			retainedChars: turns.reduce((total, turn) => total + turn.blocks.reduce(
				(sum, block) => sum + (block.type === 'image' ? block.data.length : 0), 0), 0),
			bufferedTypes: replayBuffer.map(message => message.params.update.content.type).join('|'),
			bufferedChars: replayBufferedImageChars,
			bufferedRefs: replayBufferedImages.length
		};
	})()`)
	if state["types"] != "image_omitted|image|image" ||
		state["reasons"] != "history-limit||" || state["retainedChars"] != float64(8) ||
		state["bufferedTypes"] != "image_omitted|image|image" ||
		state["bufferedChars"] != float64(8) || state["bufferedRefs"] != float64(2) {
		t.Fatalf("replay image budget state = %#v", state)
	}
}

func TestAgentCuePrefixRenderingMatchesAnswerContract(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
	})
	mux.HandleFunc("/api/preview", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"messages": []map[string]string{{"role": "agent", "text": "Next: preview"}},
		})
	})
	mux.HandleFunc("/api/transcript", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"content": "# Fixture\n\n## Agent (12:00)\nNext: history",
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof addAgentMsg === 'function' && typeof showPreview === 'function' &&
		typeof openTranscript === 'function'`)
	state := page.evalObject(t, `(async () => {
		messagesEl.innerHTML = '';
		const tick = String.fromCharCode(96);
		const fence = tick.repeat(3);
		const answerText = [
			'Next: do the thing',
			'Cause: stale state. Fix: refresh it.',
			'',
			'Separately: rotate later',
			'Step 2 of 4 done: browser tests pass.',
			'- Next: list item',
			'1. Next: numbered item',
			'A Cause: ordinary phrase.',
			'Inline ' + tick + 'state. Fix:' + tick + ' stays code.',
			'[Details. Cause: linked](https://example.test) stays linked.',
			fence + 'text',
			'Fix: inside fence',
			fence,
			fence + 'text',
			'Cause: inside unfinished fence'
		].join('\n');
		const answer = addAgentMsg(answerText);
		const cues = [...answer.querySelectorAll('.agent-cue-prefix')];
		const firstStyle = cues[0] ? getComputedStyle(cues[0]) : null;
		const first = cues[0];
		const aside = cues.find(cue => cue.textContent === 'Separately:');
		const asideStyle = aside ? getComputedStyle(aside) : null;

		const streaming = addAgentMsg('Ne');
		updateAgentMsg(streaming, 'Next: stream');
		updateAgentMsg(streaming, 'Next: stream complete');
		renderTurnBatch([{type: 'agent', text: 'Next: replay'}], false);
		const replay = messagesEl.lastElementChild;

		const user = addUserMsg('Next: user text', 'sent');
		const thought = addThoughtMsg('Next: thought text', 'cue-thought');

		currentBufferName = 'cue-prefix-fixture';
		localStorage.removeItem(pinsKey());
		togglePin(streaming);
		renderPins();
		openReader(streaming);

		await showPreview({pid: 7}, 'Cue preview');
		await openTranscript({
			file: 'fixture.md', agent: 'Fixture', project: 'syzygy', timestamp: 0
		});

		const result = {
			liveCount: cues.length,
			liveTexts: cues.map(cue => cue.textContent).join('|'),
			foreground: firstStyle ? firstStyle.color : '',
			background: firstStyle ? firstStyle.backgroundColor : '',
			weight: firstStyle ? firstStyle.fontWeight : '',
			firstCue: first ? first.dataset.cue + '/' + first.dataset.style : '',
			asideCue: aside ? aside.dataset.cue + '/' + aside.dataset.style : '',
			asideForeground: asideStyle ? asideStyle.color : '',
			asideBackground: asideStyle ? asideStyle.backgroundColor : '',
			asideWeight: asideStyle ? asideStyle.fontWeight : '',
			prefixOnly: !!(first && first.textContent === 'Next:' &&
				first.nextSibling && first.nextSibling.nodeValue === ' do the thing'),
			protectedCueCount: answer.querySelectorAll(
				'code .agent-cue-prefix, pre .agent-cue-prefix, a .agent-cue-prefix'
			).length,
			streamCount: streaming.querySelectorAll('.agent-cue-prefix').length,
			streamText: streaming.textContent,
			replayCount: replay.querySelectorAll('.agent-cue-prefix').length,
			userCount: user.querySelectorAll('.agent-cue-prefix').length,
			thoughtCount: thought.querySelectorAll('.agent-cue-prefix').length,
			pinCount: pinsBody.querySelectorAll('.agent-cue-prefix').length,
			readerCount: readerBody.querySelectorAll('.agent-cue-prefix').length,
			previewCount: pvBody.querySelectorAll('.agent-cue-prefix').length,
			historyCount: historyBody.querySelectorAll('.agent-cue-prefix').length
		};
		closeReader();
		localStorage.removeItem(pinsKey());
		return result;
	})()`)

	if state["liveCount"] != float64(7) ||
		state["liveTexts"] != "Next:|Cause:|Fix:|Separately:|Step 2 of 4 done:|Next:|Next:" {
		t.Fatalf("agent cue matches = %#v", state)
	}
	if state["foreground"] != "rgb(251, 73, 52)" ||
		state["background"] != "rgb(51, 25, 23)" || state["weight"] != "700" ||
		state["prefixOnly"] != true || state["protectedCueCount"] != float64(0) {
		t.Fatalf("agent cue styling/exclusions = %#v", state)
	}
	if state["firstCue"] != "next/cue" || state["asideCue"] != "separately/aside" ||
		state["asideForeground"] != "rgb(131, 165, 152)" ||
		state["asideBackground"] != "rgb(33, 39, 38)" || state["asideWeight"] != "700" {
		t.Fatalf("agent cue aside styling = %#v", state)
	}
	if state["streamCount"] != float64(1) || state["streamText"] != "AgentNext: stream complete" ||
		state["replayCount"] != float64(1) || state["pinCount"] != float64(1) ||
		state["readerCount"] != float64(1) || state["previewCount"] != float64(1) ||
		state["historyCount"] != float64(1) {
		t.Fatalf("agent cue answer surfaces = %#v", state)
	}
	if state["userCount"] != float64(0) || state["thoughtCount"] != float64(0) {
		t.Fatalf("non-answer cue counts = %#v", state)
	}
}

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
		showOrrery();
		return {identified: thoughtMsgsById.size, anonymousCleared: currentAnonymousThought === null};
	})()`)
	if reset["identified"] != float64(0) || reset["anonymousCleared"] != true {
		t.Fatalf("orrery thought reset = %#v", reset)
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
	page.eval(t, `showOrrery()`)
	state = page.evalObject(t, `({dockVisible: document.getElementById('history-dock').classList.contains('visible')})`)
	if state["dockVisible"] != true {
		t.Fatalf("orrery state = %#v, want dock restored", state)
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
	profile, err := os.MkdirTemp("", "acp-mobile-chrome-")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Chrome helpers can outlive the browser process briefly and touch the
		// profile after Wait returns. Retry removal so that race cannot turn a
		// passing browser assertion into a TempDir cleanup failure.
		deadline := time.Now().Add(2 * time.Second)
		for {
			if err := os.RemoveAll(profile); err == nil {
				break
			} else if time.Now().After(deadline) {
				t.Errorf("remove Chrome profile: %v", err)
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
	})
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

func TestOrderedListsRenderAndPinsRerenderFromSource(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	page := openChromePage(t, server.URL)
	page.waitFor(t, `typeof addAgentMsg === 'function' && typeof togglePin === 'function'`)
	state := page.evalObject(t, `(() => {
		messagesEl.innerHTML = '';
		showChat();
		const text = [
			'Intro line.',
			'1. **First** item',
			'2. **Second** item',
			'3. Third item',
			'',
			'- bullet one',
			'- bullet two',
			'',
			'7. resumed seven',
			'8. resumed eight',
			'',
			'Trailing. 1985. is not a list.'
		].join('\n');
		const msg = addAgentMsg(text);
		const md = msg.querySelector('.md');
		const ols = [...md.querySelectorAll('ol')];
		const uls = [...md.querySelectorAll('ul')];

		currentBufferName = 'ordered-list-fixture';
		localStorage.removeItem(pinsKey());
		togglePin(msg);
		// Stand in for a pin captured by an older renderer: same source text,
		// stale markup. It must come back styled the way the chat renders it now.
		const stored = JSON.parse(localStorage.getItem(pinsKey()));
		stored[0].html = '<div class="msg agent"><span class="role">Agent</span>' +
			'<div class="md"><p>STALE RENDER</p></div></div>';
		localStorage.setItem(pinsKey(), JSON.stringify(stored));
		renderPins();
		const pinMd = pinsBody.querySelector('.msg .md');

		const result = {
			olCount: ols.length,
			ulCount: uls.length,
			olItems: ols.map(ol => [...ol.children].map(li => li.tagName).join(',')).join('/'),
			ulItems: uls.map(ul => [...ul.children].map(li => li.tagName).join(',')).join('/'),
			firstOlText: ols[0] ? [...ols[0].children].map(li => li.textContent).join('|') : '',
			resumedValues: ols[1] ? [...ols[1].children].map(li => li.getAttribute('value')).join(',') : '',
			boldInItem: ols[0] ? ols[0].querySelectorAll('strong').length : 0,
			listStyle: ols[0] ? getComputedStyle(ols[0]).listStyleType : '',
			indented: ols[0] ? getComputedStyle(ols[0]).paddingLeft : '',
			markerColor: ols[0] ? getComputedStyle(ols[0].children[0], '::marker').color : '',
			markerWeight: ols[0] ? getComputedStyle(ols[0].children[0], '::marker').fontWeight : '',
			bulletColor: uls[0] ? getComputedStyle(uls[0].children[0], '::marker').color : '',
			panel: ols[0] ? getComputedStyle(ols[0]).backgroundColor : '',
			itemGap: ols[0] ? getComputedStyle(ols[0].children[1]).marginTop : '',
			firstItemGap: ols[0] ? getComputedStyle(ols[0].children[0]).marginTop : '',
			markerLeaked: md.innerHTML.indexOf('data-ol') >= 0 || md.innerHTML.indexOf('data-ul') >= 0,
			numberNotEatenMidLine: md.textContent.indexOf('Trailing. 1985. is not a list.') >= 0,
			pinStale: pinMd ? pinMd.textContent.indexOf('STALE RENDER') >= 0 : null,
			pinMatchesChat: pinMd ? pinMd.innerHTML === md.innerHTML : false
		};
		localStorage.removeItem(pinsKey());
		return result;
	})()`)

	if state["olCount"] != float64(2) || state["ulCount"] != float64(1) ||
		state["olItems"] != "LI,LI,LI/LI,LI" || state["ulItems"] != "LI,LI" {
		t.Fatalf("list structure = %#v, want two <ol> and one <ul> holding only <li>", state)
	}
	if state["firstOlText"] != "First item|Second item|Third item" ||
		state["resumedValues"] != "7,8" || state["boldInItem"] != float64(2) {
		t.Fatalf("list content = %#v", state)
	}
	if state["listStyle"] != "decimal" || state["indented"] != "30px" ||
		state["markerLeaked"] != false || state["numberNotEatenMidLine"] != true {
		t.Fatalf("list styling = %#v", state)
	}
	// Markers carry agent-shell's mr-x/agent-shell-list-marker yellow. Items are
	// separated by a gap rather than a panel, and only between items — no stray
	// leading space where the list meets the paragraph above it.
	if state["markerColor"] != "rgb(250, 189, 47)" || state["markerWeight"] != "700" ||
		state["bulletColor"] != "rgb(250, 189, 47)" ||
		state["panel"] != "rgba(0, 0, 0, 0)" ||
		state["itemGap"] != "12px" || state["firstItemGap"] != "0px" {
		t.Fatalf("list marker styling = %#v", state)
	}
	if state["pinStale"] != false || state["pinMatchesChat"] != true {
		t.Fatalf("pin rendering = %#v, want the pin re-rendered like the chat bubble", state)
	}
}

// openComposerTestPage serves the embedded UI at phone metrics. height may be
// smaller than screenHeight to emulate the iOS standalone keyboard-restore
// bug, where the layout viewport stays short after the keyboard dismisses.
func openComposerTestPage(t *testing.T, height, screenHeight int) *chromePage {
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
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	page := openChromePage(t, server.URL)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{
		"width": 390, "height": height, "screenWidth": 390, "screenHeight": screenHeight,
		"deviceScaleFactor": 1, "mobile": true,
	})
	page.call(t, "Page.reload", map[string]interface{}{})
	page.waitFor(t, `typeof sendPromptText === 'function'`)
	return page
}

// A touch on Send must send on that first contact and must not blur the
// composer: on iOS the click that used to carry the send only arrives after
// the textarea blurs, the keyboard drops, and the composer moves out from
// under the finger (one tap dismissed the keyboard, a second tap sent).
func TestTouchSendFiresOnPointerdownAndKeepsComposerFocused(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	state := page.evalObject(t, `(async () => {
		messagesEl.innerHTML = '';
		showChat();
		sessionId = 'touch-send-session';
		window.__sends = [];
		ws = {readyState: 1, send: raw => window.__sends.push(JSON.parse(raw))};
		setProcessing(false);
		sendBtn.disabled = false;
		promptEl.focus();
		promptEl.value = 'first tap';
		const down = new PointerEvent('pointerdown', {pointerType: 'touch', bubbles: true, cancelable: true});
		sendBtn.dispatchEvent(down);
		const sentOnPointerdown = window.__sends.length;
		// The synthetic click that follows a touch must not send or queue again.
		promptEl.value = 'ghost';
		sendBtn.click();
		await new Promise(r => setTimeout(r, 30));
		const afterClick = window.__sends.length + messageQueue.length;
		const ghostKept = promptEl.value;
		// Mouse and keyboard still send via click once the touch window closes.
		await new Promise(r => setTimeout(r, 700));
		setProcessing(false);
		messageQueue = [];
		promptEl.value = 'desktop click';
		sendBtn.click();
		return {
			sentOnPointerdown,
			iconIgnoresPointer: getComputedStyle(sendBtn.querySelector('svg')).pointerEvents === 'none',
			defaultPrevented: down.defaultPrevented,
			focused: document.activeElement === promptEl,
			afterClick,
			ghostKept,
			total: window.__sends.length,
			firstText: window.__sends[0] ? window.__sends[0].params.prompt[0].text : '',
			lastText: window.__sends[1] ? window.__sends[1].params.prompt[0].text : ''
		};
	})()`)
	if state["sentOnPointerdown"] != float64(1) || state["firstText"] != "first tap" {
		t.Fatalf("touch pointerdown should send once, got %v", state)
	}
	if state["defaultPrevented"] != true || state["focused"] != true {
		t.Fatalf("touch send should keep the composer focused, got %v", state)
	}
	if state["iconIgnoresPointer"] != true {
		t.Fatalf("send icon must not be the hit target (iOS drops touches on it while the textarea is focused), got %v", state)
	}
	if state["afterClick"] != float64(1) || state["ghostKept"] != "ghost" {
		t.Fatalf("trailing click after touch must not send again, got %v", state)
	}
	if state["total"] != float64(2) || state["lastText"] != "desktop click" {
		t.Fatalf("click should still send outside the touch window, got %v", state)
	}
}

// iOS home-screen web apps shrink the layout viewport for the keyboard and
// never grow it back after blur; forcing the root element to the screen
// height for a beat makes WebKit recompute it. The poke must happen only
// when the viewport is actually short and must clear itself afterwards.
func TestStandaloneViewportRestoreAfterKeyboardDismiss(t *testing.T) {
	probe := `(async () => {
		Object.defineProperty(navigator, 'standalone', {value: true, configurable: true});
		showChat();
		promptEl.focus();
		await new Promise(r => setTimeout(r, 30));
		promptEl.blur();
		const started = Date.now();
		let poked = '';
		while (Date.now() - started < 1500 && !poked) {
			poked = document.documentElement.style.height;
			await new Promise(r => setTimeout(r, 20));
		}
		await new Promise(r => setTimeout(r, 600));
		return {poked, cleared: document.documentElement.style.height};
	})()`

	short := openComposerTestPage(t, 812, 874)
	state := short.evalObject(t, probe)
	if state["poked"] != "874px" || state["cleared"] != "" {
		t.Fatalf("short standalone viewport should be poked to screen height then cleared, got %v", state)
	}

	full := openComposerTestPage(t, 874, 874)
	state = full.evalObject(t, probe)
	if state["poked"] != "" {
		t.Fatalf("full-height viewport must not be poked, got %v", state)
	}
}

// The send spring must spread its motion over enough frames to be seen, and
// the thinking bar that appears on send must not cover the new bubble.
func TestSendMotionIsVisibleAndThinkingBarKeepsBubbleInView(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	state := page.evalObject(t, `(() => {
		messagesEl.innerHTML = '';
		showChat();
		sessionId = 'send-motion-session';
		ws = {readyState: 1, send: () => {}};
		setProcessing(false);
		for (let i = 0; i < 40; i++) addAgentMsg('filler line ' + i);
		const bubble = addUserMsg('outgoing', 'sending');
		const style = getComputedStyle(bubble);
		// Sample the easing at 18% of the duration: the old spring curve had
		// already travelled ~90% by then, which is 3-4 frames at 340ms.
		const bezier = style.animationTimingFunction.match(/cubic-bezier\(([^)]+)\)/);
		const [x1, y1, x2, y2] = bezier ? bezier[1].split(',').map(Number) : [0, 0, 1, 1];
		const at = (p, a, b) => 3 * (1 - p) * (1 - p) * p * a + 3 * (1 - p) * p * p * b + p * p * p;
		let lo = 0, hi = 1;
		for (let i = 0; i < 40; i++) { const mid = (lo + hi) / 2; if (at(mid, x1, x2) < 0.18) lo = mid; else hi = mid; }
		const progressAt18 = at(lo, y1, y2);
		setProcessing(true);
		const gap = messagesEl.scrollHeight - messagesEl.clientHeight - messagesEl.scrollTop;
		return {
			duration: parseFloat(style.animationDuration),
			progressAt18,
			gapAfterThinkingBar: gap,
			thinkingVisible: thinkingEl.classList.contains('visible')
		};
	})()`)
	if d := state["duration"].(float64); d < 0.4 {
		t.Fatalf("send animation too short to register: %v", state)
	}
	if p := state["progressAt18"].(float64); p > 0.7 {
		t.Fatalf("send easing front-loads its motion (%.2f done at 18%% of duration): %v", p, state)
	}
	if state["thinkingVisible"] != true || state["gapAfterThinkingBar"].(float64) > 1 {
		t.Fatalf("thinking bar should not push the new bubble out of view, got %v", state)
	}
}

// The jump-to-bottom button must land on the real bottom and hand over to
// auto-follow. It used to receive the click event as the "smooth" flag,
// which animated toward a stale height and never marked the list at bottom,
// so streamed content kept arriving below the landing point.
func TestJumpToBottomLandsOnRealBottomAndFollows(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	state := page.evalObject(t, `(async () => {
		messagesEl.innerHTML = '';
		showChat();
		sessionId = 'jump-session';
		ws = {readyState: 1, send: () => {}};
		setProcessing(false);
		for (let i = 0; i < 60; i++) addAgentMsg('line ' + i);
		messagesEl.scrollTop = 0;
		await new Promise(r => setTimeout(r, 150));   // let the scroll event mark us away from the bottom
		const shownBefore = scrollBtn.classList.contains('visible') && !isAtBottom;
		scrollBtn.click();
		// Read synchronously: an instant snap has already landed and flagged
		// at-bottom; a smooth scroll has done neither yet.
		const gapAfterClick = messagesEl.scrollHeight - messagesEl.clientHeight - messagesEl.scrollTop;
		const atBottomAfterClick = isAtBottom;
		const hiddenAfterClick = !scrollBtn.classList.contains('visible');
		// Content that streams in after the jump must stay in view.
		for (let i = 0; i < 5; i++) addAgentMsg('late line ' + i);
		const gapAfterStream = messagesEl.scrollHeight - messagesEl.clientHeight - messagesEl.scrollTop;
		return {shownBefore, gapAfterClick, atBottomAfterClick, hiddenAfterClick, gapAfterStream};
	})()`)
	if state["shownBefore"] != true {
		t.Fatalf("button should show when scrolled up, got %v", state)
	}
	if state["gapAfterClick"].(float64) > 1 || state["atBottomAfterClick"] != true || state["hiddenAfterClick"] != true {
		t.Fatalf("jump should snap to the real bottom, mark at-bottom and hide, got %v", state)
	}
	if state["gapAfterStream"].(float64) > 1 {
		t.Fatalf("list should keep following after the jump, got %v", state)
	}
}

// The multiplex keeps answered permissions in the replay so their cards stay
// visible as history. Only a permission that is the final turn of the replay
// is still waiting; an answered one must not make an idle chat look busy.
func TestReplayAnsweredPermissionDoesNotLookBusy(t *testing.T) {
	page := openComposerTestPage(t, 844, 844)
	state := page.evalObject(t, `(() => {
		const upd = (update) => ({jsonrpc: '2.0', method: 'session/update', params: {sessionId: 's1', update}});
		const perm = (id) => ({jsonrpc: '2.0', id, method: 'session/request_permission', params: {sessionId: 's1',
			toolCall: {title: 'Read facts file', kind: 'read'},
			options: [{optionId: 'allow', name: 'Allow', kind: 'allow_once'}, {optionId: 'deny', name: 'Deny', kind: 'reject_once'}]}});
		const run = (records) => {
			messagesEl.innerHTML = ''; showChat(); sessionId = null; pendingPermissions = []; setProcessing(false);
			replayMode = true; resetReplayBuffer();
			for (const r of records) bufferReplayMessage(r);
			flushReplay();
			const card = messagesEl.querySelector('.msg.permission');
			return {processing, resolved: !!card && card.classList.contains('resolved'),
				buttons: card ? card.querySelectorAll('.perm-btn:not([disabled])').length : -1};
		};
		const answered = run([
			{jsonrpc: '2.0', id: 0, result: {sessionId: 's1'}},
			upd({sessionUpdate: 'user_message_chunk', content: {type: 'text', text: 'check the facts file'}}),
			upd({sessionUpdate: 'agent_message_chunk', content: {type: 'text', text: 'Looking.'}}),
			perm(7),
			upd({sessionUpdate: 'agent_message_chunk', content: {type: 'text', text: 'Done, it is up to date.'}}),
			upd({sessionUpdate: 'usage_update', used: 10, size: 100}),
			upd({sessionUpdate: 'turn_complete', stopReason: 'end_turn'})
		]);
		const waiting = run([
			{jsonrpc: '2.0', id: 0, result: {sessionId: 's1'}},
			upd({sessionUpdate: 'user_message_chunk', content: {type: 'text', text: 'check the facts file'}}),
			upd({sessionUpdate: 'agent_message_chunk', content: {type: 'text', text: 'Looking.'}}),
			perm(8)
		]);
		return {answered, waiting};
	})()`)
	answered := state["answered"].(map[string]interface{})
	waiting := state["waiting"].(map[string]interface{})
	if answered["processing"] != false || answered["resolved"] != true || answered["buttons"] != float64(0) {
		t.Fatalf("answered permission in history must replay resolved and idle, got %v", answered)
	}
	if waiting["processing"] != true || waiting["resolved"] != false || waiting["buttons"].(float64) < 1 {
		t.Fatalf("permission that ends the replay must stay interactive and busy, got %v", waiting)
	}
}
