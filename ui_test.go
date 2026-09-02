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
