# Chat Header and Pull-Down Toolbar Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the raw-data chat header with a designed title, a provider-icon row, a status line, and a pull-down toolbar that absorbs the bottom review bar and most of the kebab menu.

**Architecture:** One new read-only Go endpoint returns branch and dirty counts for a live session. Everything else is in `index.html`: the header keeps its existing state variables (`statusText`, `currentMode`, `currentBufferName`) and gains render functions that map them onto the new elements. The socket block that `index_test.mjs` extracts (`index.html` between `// --- Socket: connect, reconnect, keepalive ---` and `// --- Socket: end ---`) is not edited; a MutationObserver on `#status-text` drives the new visuals from outside it.

**Tech Stack:** Go 1.22 standard library plus `golang.org/x/net/websocket`; vanilla JS in a single `index.html`; Chrome DevTools driven Go tests (`openChromePage` in `ui_test.go`); node tests in `index_test.mjs`.

**Spec:** `docs/superpowers/specs/2026-09-23-chat-header-toolbar-design.md`

## Global Constraints

- No emojis anywhere, including test fixtures and commit messages.
- Every git read on the server uses `gitReadOnlyEnv` (`GIT_OPTIONAL_LOCKS=0`) and the 2 MiB `diffOutputLimit` cap.
- Tap targets are at least 44px tall.
- Fonts stay `var(--mono)`; colors come from the `:root` variables, never new hex values in CSS.
- Do not edit the socket block in `index.html` (lines between the two `// --- Socket:` markers); `index_test.mjs` extracts it verbatim.
- Existing tests keep passing: `go test ./...` and `node --test index_test.mjs`.
- Chrome tests run at 393x852 with `Emulation.setDeviceMetricsOverride`.
- localStorage keys: `acp-toolbar-open`.
- Endpoint: `GET /api/git-status?pid=PID`.

---

## File Structure

- `diff.go` (modify): add `gitStatusResult`, `gitStatus(ctx, dir)`, `parsePorcelainStatus(raw)`, `handleGitStatus`. Lives beside `inspectRepository` because it shares `runGitBounded` and `sessionCwd`.
- `diff_test.go` (modify): `TestParsePorcelainStatus`, `TestGitStatusHandler`.
- `main.go` (modify): register `/api/git-status`.
- `index.html` (modify): header markup and CSS, row 2 render functions, status observer, grabber and toolbar markup, CSS and handlers, kebab trim, review bar removal.
- `header_ui_test.go` (create): Chrome tests for title, row 2, status visuals, grabber, toolbar, kebab.
- `ui_test.go` (modify): the clone test at the `chat-menu-btn` click uses the toolbar item.
- `README.md` (modify): the review entry point paragraph.

---

### Task 1: Git status endpoint

**Files:**
- Modify: `diff.go` (append after `inspectRepository`, around line 392)
- Modify: `main.go:335` (route table)
- Test: `diff_test.go` (append)

**Interfaces:**
- Consumes: `runGitBounded(ctx, dir, env, limit, args...) ([]byte, bool, error)`, `inspectRepository(ctx, dir) (repoInfo, error)`, `sessionCwd(pid) string`, `findSocket(pid string) string`, `diffOutputLimit`.
- Produces: `type gitStatusResult struct { Branch string; Staged, Unstaged, Untracked int; Truncated bool }` serialized as `{"branch","staged","unstaged","untracked","truncated"}`; `func handleGitStatus(w, r)`.

- [ ] **Step 1: Write the failing parser test**

Append to `diff_test.go`:

```go
func TestParsePorcelainStatus(t *testing.T) {
	// git status --porcelain=v1 -z: "XY path\x00", renames add "\x00orig".
	raw := []byte("M  a.txt\x00 M b.txt\x00MM c.txt\x00?? new.txt\x00R  moved.txt\x00old.txt\x00A  added.txt\x00")
	got := parsePorcelainStatus(raw)
	want := gitStatusResult{Staged: 4, Unstaged: 2, Untracked: 1}
	if got != want {
		t.Fatalf("parsePorcelainStatus = %+v, want %+v", got, want)
	}
	if got := parsePorcelainStatus(nil); got != (gitStatusResult{}) {
		t.Fatalf("empty status = %+v, want zero", got)
	}
}
```

Staged counts `M `, `MM`, `R `, `A ` (4). Unstaged counts ` M`, `MM` (2). Untracked counts `??` (1).

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -run TestParsePorcelainStatus ./...`
Expected: FAIL with `undefined: parsePorcelainStatus`.

- [ ] **Step 3: Write the parser and the status reader**

Append to `diff.go` after `inspectRepository`:

```go
// gitStatusResult is the header's view of a repository: the branch and
// how many paths are staged, unstaged and untracked. A path that is
// both staged and unstaged counts once in each.
type gitStatusResult struct {
	Branch    string `json:"branch"`
	Staged    int    `json:"staged"`
	Unstaged  int    `json:"unstaged"`
	Untracked int    `json:"untracked"`
	Truncated bool   `json:"truncated,omitempty"`
}

// parsePorcelainStatus counts entries of `git status --porcelain=v1 -z`.
// Entries are "XY path" separated by NUL; a rename or copy carries the
// original path in the following NUL field, which is skipped.
func parsePorcelainStatus(raw []byte) gitStatusResult {
	var res gitStatusResult
	fields := bytes.Split(raw, []byte{0})
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 3 {
			continue
		}
		x, y := entry[0], entry[1]
		if x == '?' && y == '?' {
			res.Untracked++
			continue
		}
		if x == '!' && y == '!' {
			continue
		}
		if x != ' ' {
			res.Staged++
		}
		if y != ' ' {
			res.Unstaged++
		}
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++ // the original path travels in the next field
		}
	}
	return res
}

// gitStatus reads the branch and counts for dir. A directory that is
// not a repository yields an empty branch and zero counts, not an error.
func gitStatus(ctx context.Context, dir string) (gitStatusResult, error) {
	info, err := inspectRepository(ctx, dir)
	if err != nil {
		return gitStatusResult{}, nil
	}
	raw, truncated, err := runGitBounded(ctx, info.Root, nil, diffOutputLimit, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return gitStatusResult{}, err
	}
	res := parsePorcelainStatus(raw)
	res.Branch = info.Branch
	res.Truncated = truncated
	return res, nil
}

// handleGitStatus serves GET /api/git-status?pid=PID for a live session.
func handleGitStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pid, err := strconv.Atoi(r.URL.Query().Get("pid"))
	if err != nil || pid <= 0 {
		http.Error(w, "invalid pid", http.StatusBadRequest)
		return
	}
	if findSocket(strconv.Itoa(pid)) == "" {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var res gitStatusResult
	if dir := sessionCwd(pid); dir != "" {
		if res, err = gitStatus(ctx, dir); err != nil {
			log.Printf("git-status: %v", err)
			http.Error(w, "status failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}
```

Add `"bytes"` to the import block of `diff.go` if it is not already there.

- [ ] **Step 4: Run the parser test**

Run: `go test -run TestParsePorcelainStatus ./...`
Expected: PASS.

- [ ] **Step 5: Write the failing handler test**

Append to `diff_test.go`:

```go
func TestGitStatusHandler(t *testing.T) {
	get := func(t *testing.T, query string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		handleGitStatus(rec, httptest.NewRequest(http.MethodGet, "/api/git-status?"+query, nil))
		return rec
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) gitStatusResult {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("git status must not be cached: %q", rec.Header().Get("Cache-Control"))
		}
		var res gitStatusResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	t.Setenv("HOME", t.TempDir())
	dir := initTestRepo(t)
	pid, _ := fakeSession(t, dir)

	rec := httptest.NewRecorder()
	handleGitStatus(rec, httptest.NewRequest(http.MethodPost, "/api/git-status?pid=1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be refused: %d", rec.Code)
	}
	for _, bad := range []string{"", "pid=abc", "pid=0"} {
		if rec := get(t, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q must be rejected with 400, got %d", bad, rec.Code)
		}
	}
	if rec := get(t, "pid=999999"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown pid must be 404, got %d", rec.Code)
	}

	clean := decode(t, get(t, fmt.Sprintf("pid=%d", pid)))
	if clean.Branch != "main" || clean.Staged+clean.Unstaged+clean.Untracked != 0 {
		t.Fatalf("clean repo = %+v", clean)
	}

	writeRepoFile(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")
	gitIn(t, dir, "add", "a.txt")
	writeRepoFile(t, dir, "a.txt", "one\ntwo\nthree\nfour\nfive\n")
	writeRepoFile(t, dir, "new.txt", "hi\n")
	dirty := decode(t, get(t, fmt.Sprintf("pid=%d", pid)))
	if dirty.Branch != "main" || dirty.Staged != 1 || dirty.Unstaged != 1 || dirty.Untracked != 1 {
		t.Fatalf("partially staged repo = %+v, want staged 1 unstaged 1 untracked 1", dirty)
	}

	plain := t.TempDir()
	previous := sessionCwd
	sessionCwd = func(int) string { return plain }
	t.Cleanup(func() { sessionCwd = previous })
	none := decode(t, get(t, fmt.Sprintf("pid=%d", pid)))
	if none != (gitStatusResult{}) {
		t.Fatalf("non-repo cwd = %+v, want empty", none)
	}
}
```

- [ ] **Step 6: Run the handler test**

Run: `go test -run TestGitStatusHandler ./...`
Expected: PASS. If `Branch` comes back empty for the clean repo, `initTestRepo` created the branch with `-b main`; check `inspectRepository` is reading `symbolic-ref`.

- [ ] **Step 7: Register the route**

In `main.go` next to `mux.HandleFunc("/api/diff-review", handleDiffReview)` add:

```go
	mux.HandleFunc("/api/git-status", handleGitStatus)
```

- [ ] **Step 8: Run the whole Go suite and commit**

Run: `go test ./...`
Expected: PASS.

```bash
git add diff.go diff_test.go main.go
git commit -m "feat: serve branch and dirty counts for a live session"
```

---

### Task 2: Header title and row 2

**Files:**
- Modify: `index.html` header markup (`<div id="header">` at line 1734), header CSS (lines 254 to 350), `updateChatHeader` (line 1962), `updateModeBtn` (line 3883), `setProcessing` (line 3876), `selectSession` (line 5522), `chooseModel` (line 6101), the `acp-multiplex/meta` handlers (lines 4368 and 4637).
- Create: `header_ui_test.go`

**Interfaces:**
- Consumes: `providerIcon(name) string` (line 5472), `modeColor(id)`, `modeName(id)`, `currentBufferName`, `lastSessions`, `currentSockPid`, `basePath`, `chatBusy.set(reason, on)`, `historyLoadingEl`.
- Produces: `let headerModel = ''`, `let headerGit = null`, `function renderHeaderSub()`, `async function fetchHeaderModel()`, `async function fetchGitStatus()`, `function modeShortName(id)`, `function renderConnectionState()`. Task 3 calls `fetchGitStatus()` and reads `headerGit` and `headerModel`.

- [ ] **Step 1: Write the failing Chrome test**

Create `header_ui_test.go`:

```go
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

// newHeaderTestPage serves the app with one labeled-or-not session whose
// socket replays a short conversation, plus stubbed models and git status.
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
	mux.HandleFunc("/api/git-status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"branch":"syzygy","staged":3,"unstaged":2,"untracked":1}`)
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
		return page.evalObject(t, `(()=>{const icon=document.getElementById('hs-agent');const line=document.getElementById('history-loading');
			return {iconOpacity:getComputedStyle(icon).opacity, lineHidden:line.hidden, dead:line.classList.contains('dead'), label:line.getAttribute('aria-label')||''};})()`)
	}
	connected := read()
	if connected["iconOpacity"] != "1" || connected["lineHidden"] != true {
		t.Fatalf("connected visuals = %v", connected)
	}
	page.eval(t, `statusText.textContent='Reconnecting...'; statusText.className='header-status reconnecting'`)
	page.waitFor(t, `document.getElementById('history-loading').hidden === false`)
	busy := read()
	if busy["dead"] != false || busy["label"] != "Reconnecting..." {
		t.Fatalf("reconnecting visuals = %v", busy)
	}
	page.eval(t, `statusText.textContent='Disconnected'; statusText.className='header-status error'`)
	page.waitFor(t, `document.getElementById('history-loading').classList.contains('dead')`)
	dead := read()
	if dead["iconOpacity"] != "0.4" || dead["lineHidden"] != false || dead["label"] != "Disconnected" {
		t.Fatalf("disconnected visuals = %v", dead)
	}
	page.eval(t, `statusText.textContent='Connected'; statusText.className='header-status connected'`)
	page.waitFor(t, `document.getElementById('history-loading').hidden === true && !document.getElementById('history-loading').classList.contains('dead')`)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -run 'TestHeaderTitleAndRow|TestHeaderConnectionVisuals' -count=1 ./...`
Expected: FAIL. `hs-model` does not exist, so `waitFor` times out.

- [ ] **Step 3: Replace the header markup**

In `index.html`, replace the `<div id="header">` block (line 1734 to its closing `</div>` before `<div id="file-browser">`) with:

```html
<div id="header">
  <button class="back-btn" id="back-btn">&larr;</button>
  <div class="header-info">
    <div class="header-row">
      <div class="header-title" id="header-title"><span class="ht-main">ACP</span><span class="ht-ord"></span></div>
      <button class="bell-btn" id="bell-btn" aria-label="Phone push" aria-pressed="false" title="Phone push for this chat"><svg viewBox="0 0 24 24"><path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/></svg></button>
    </div>
    <div class="header-sub">
      <span class="header-status" id="status-text" role="status" aria-live="polite">Connecting...</span>
      <span class="hs-agent" id="hs-agent"><span id="hs-provider"></span><span id="hs-model"></span></span>
      <span class="hs-sep" id="hs-sep-mode" hidden>&middot;</span>
      <button class="mode-btn" id="mode-btn" hidden>Ask</button>
      <span class="hs-sep" id="hs-sep-git" hidden>&middot;</span>
      <button class="hs-git" id="hs-git" hidden aria-label="Review repository changes"><svg viewBox="0 0 24 24" aria-hidden="true"><line x1="6" x2="6" y1="3" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/></svg><span id="hs-branch"></span></button>
    </div>
  </div>
  <div class="header-actions">
    <button class="files-btn" id="files-btn">Files</button>
    <button class="menu-btn" id="chat-menu-btn" aria-label="Menu" style="display:none"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="5" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="12" cy="19" r="1.6"/></svg></button>
  </div>
  <div id="history-loading" class="load-line" role="status" aria-live="polite" hidden><span>Loading chat…</span></div>
</div>
```

`#header-buf` is gone. `#mode-btn` starts `hidden` instead of `style="display:none"`.

- [ ] **Step 4: Replace the header CSS**

In the CSS, delete the rules for `#header .header-buf`, `#header .header-status`, `#header .header-status.connected`, `#header .header-status.connected::before`, `#header .header-status.error`, `#header .header-status.reconnecting`, `#header .mode-btn`, `#header .mode-btn:active` (lines 305 to 334, keep the `#messages.reconnecting` rule). Add in their place:

```css
/* Row 2 is one line: provider icon, model, mode word, branch. */
#header .header-sub {
  display: flex; align-items: center; gap: 6px; margin-top: 2px;
  font-size: 11px; font-family: var(--mono); color: var(--fg-dim); min-width: 0;
}
/* The connection words stay for screen readers and tests; the icon and
   the load line carry the state visually. */
#header .header-status {
  position: absolute; width: 1px; height: 1px; overflow: hidden;
  clip: rect(0 0 0 0); white-space: nowrap;
}
#header .hs-agent { display: inline-flex; align-items: center; gap: 4px; min-width: 0; transition: opacity 0.2s ease; }
#header .hs-agent.offline { opacity: 0.4; }
#header .hs-agent .provider-icon { width: 14px; height: 14px; flex-basis: 14px; }
#header .hs-sep { color: var(--fg-mute); }
#header .mode-btn {
  border: none; background: none; padding: 0; margin: -8px 0;
  min-height: 28px; font-size: 11px; font-weight: 600;
  font-family: var(--mono); cursor: pointer; color: var(--fg-dim);
}
#header .mode-btn:active { opacity: 0.7; }
#header .hs-git {
  display: inline-flex; align-items: center; gap: 4px;
  border: none; background: none; padding: 0; margin: -8px 0; min-height: 28px;
  color: var(--fg); font-family: var(--mono); font-size: 11px; cursor: pointer;
  position: relative; -webkit-tap-highlight-color: transparent;
}
#header .hs-git svg { width: 12px; height: 12px; fill: none; stroke: var(--fg-dim); stroke-width: 2; stroke-linecap: round; stroke-linejoin: round; }
#header .hs-git.dirty::after {
  content: ""; position: absolute; left: 8px; top: 6px; width: 6px; height: 6px;
  border-radius: 50%; background: var(--orange);
}
#header .hs-git:active { opacity: 0.7; }
#header .header-title .ht-ord { color: var(--fg-mute); font-weight: 400; margin-left: 5px; }
#header .header-title.labeled .ht-main { color: var(--orange); }
/* Menu: a bare icon button, same shape as the bell. */
#header .menu-btn {
  width: 28px; height: 28px; padding: 0; border: none; background: none;
  border-radius: 6px; color: var(--fg-dim); display: flex; align-items: center;
  justify-content: center; -webkit-tap-highlight-color: transparent;
}
#header .menu-btn svg { width: 18px; height: 18px; fill: currentColor; }
#header .menu-btn:active { background: var(--bg2); }
/* Dead socket: the load line goes solid red instead of sweeping. */
.load-line.dead::before { animation: none; left: 0; width: 100%; background: var(--red); }
```

Also change `#header .header-title.labeled { color: var(--orange); }` to leave the base color alone: delete that line, the `.labeled .ht-main` rule above replaces it. Change `#header .header-title` to `color: var(--fg)`.

- [ ] **Step 5: Rewrite the header render functions**

Replace `updateChatHeader` (line 1962) with:

```js
const headerTitleMain = headerTitle.querySelector('.ht-main');
const headerTitleOrd = headerTitle.querySelector('.ht-ord');
const hsAgent = document.getElementById('hs-agent');
const hsProvider = document.getElementById('hs-provider');
const hsModel = document.getElementById('hs-model');
const hsSepMode = document.getElementById('hs-sep-mode');
const hsSepGit = document.getElementById('hs-sep-git');
const hsGit = document.getElementById('hs-git');
const hsBranch = document.getElementById('hs-branch');
let headerModel = '';
let headerGit = null;

// "Claude Agent @ acp-mobile<4>" -> {project: "acp-mobile", ordinal: "4"}.
function parseBufferName(name) {
  const m = /^(.*?) @ (.+?)(?:<(\d+)>)?$/.exec(name || '');
  return m ? {agent: m[1], project: m[2], ordinal: m[3] || ''} : {agent: '', project: '', ordinal: ''};
}

function updateChatHeader() {
  const s = lastSessions.find(x => x.bufferName === currentBufferName) || {};
  const label = (s.label || '').trim();
  const parsed = parseBufferName(currentBufferName);
  const project = s.project || parsed.project;
  headerTitle.classList.toggle('labeled', !!label);
  if (label) {
    headerTitleMain.textContent = label;
    headerTitleOrd.textContent = '';
  } else if (project) {
    headerTitleMain.textContent = project;
    headerTitleOrd.textContent = parsed.ordinal;
  } else if (currentBufferName) {
    headerTitleMain.textContent = currentBufferName;
    headerTitleOrd.textContent = '';
  }
  bellBtn.classList.toggle('visible', !!currentBufferName);
  setBell(!!s.push);
  renderHeaderSub();
}

// Row 2: provider icon and model, mode word, branch. Separators show
// only between visible neighbours.
function renderHeaderSub() {
  hsProvider.innerHTML = providerIcon(currentBufferName || '');
  hsModel.textContent = headerModel;
  const modeVisible = !modeBtn.hidden;
  hsSepMode.hidden = !modeVisible;
  const gitVisible = !!(headerGit && headerGit.branch);
  hsGit.hidden = !gitVisible;
  hsSepGit.hidden = !gitVisible;
  if (gitVisible) {
    hsBranch.textContent = headerGit.branch;
    const dirty = (headerGit.staged || 0) + (headerGit.unstaged || 0) + (headerGit.untracked || 0) > 0;
    hsGit.classList.toggle('dirty', dirty);
    hsGit.title = dirty
      ? headerGit.staged + ' staged, ' + headerGit.unstaged + ' changed, ' + headerGit.untracked + ' new'
      : 'Clean tree';
  }
}

// Model name for row 2: one Emacs round trip per chat open.
let headerModelToken = 0;
async function fetchHeaderModel() {
  const token = ++headerModelToken;
  const bufferName = currentBufferName;
  headerModel = '';
  renderHeaderSub();
  if (!bufferName) return;
  try {
    const resp = await fetch(basePath + '/api/models', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({bufferName})
    });
    if (!resp.ok) return;
    const data = await resp.json();
    if (token !== headerModelToken || bufferName !== currentBufferName) return;
    headerModel = typeof data.current === 'string' ? data.current : '';
    renderHeaderSub();
  } catch (_) { /* icon only until the picker is opened */ }
}

// Branch and dirty counts: on chat open, after each reply, on toolbar open.
let gitStatusToken = 0;
async function fetchGitStatus() {
  const token = ++gitStatusToken;
  const pid = currentSockPid;
  if (!pid) { headerGit = null; renderHeaderSub(); return; }
  try {
    const resp = await fetch(basePath + '/api/git-status?pid=' + encodeURIComponent(pid), {cache: 'no-store'});
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const data = await resp.json();
    if (token !== gitStatusToken || pid !== currentSockPid) return;
    headerGit = data && typeof data.branch === 'string' ? data : null;
  } catch (_) {
    if (token !== gitStatusToken) return;
    // keep the last value; a later fetch or chat switch replaces it
  }
  renderHeaderSub();
}

// Connection state drives the icon opacity and the load line. The words
// in #status-text stay the single source of truth so the socket block
// (extracted verbatim by index_test.mjs) is untouched.
function renderConnectionState() {
  const cls = statusText.className;
  const connected = cls.includes('connected');
  const dead = cls.includes('error');
  hsAgent.classList.toggle('offline', !connected);
  historyLoadingEl.classList.toggle('dead', dead);
  chatBusy.set('socket', !connected && !dead);
  chatBusy.set('dead', dead);
  historyLoadingEl.setAttribute('aria-label', connected ? '' : statusText.textContent);
}
```

`chatBusy` and `historyLoadingEl` are declared later in the file (lines 3766 and 3780). Move this observer registration to just after `const chatBusy = busyLine(historyLoadingEl);`:

```js
new MutationObserver(renderConnectionState).observe(statusText, {attributes: true, attributeFilter: ['class'], childList: true, characterData: true, subtree: true});
renderConnectionState();
```

Delete the `const headerBuf = ...` line (1954).

- [ ] **Step 6: Mode word, model refresh, and fetch triggers**

Replace `updateModeBtn` (line 3883) with:

```js
const MODE_SHORT_NAMES = {bypassPermissions: 'bypass', acceptEdits: 'accept edits', plan: 'plan', default: 'ask', ask: 'ask'};
function modeShortName(id) {
  if (MODE_SHORT_NAMES[id]) return MODE_SHORT_NAMES[id];
  return String(modeName(id) || id).toLowerCase();
}
function updateModeBtn() {
  modeBtn.textContent = modeShortName(currentMode);
  modeBtn.style.color = modeColor(currentMode);
  modeBtn.hidden = availableModes.length === 0;
  renderHeaderSub();
}
```

Where the two session-init handlers show the pill (lines 4354 and 4618, `modeBtn.style.display = '';`), change both to `modeBtn.hidden = false;`. In `selectSession` after `availableModes = [];` add `modeBtn.hidden = true;`.

In `setProcessing` (line 3876), after `chatBusy.set('reply', v);` add:

```js
  if (!v && processingWas) fetchGitStatus();
```

and capture `const processingWas = processing;` as the first line of the function, before `processing = v;`.

In both `acp-multiplex/meta` handlers (after `updateChatHeader();` at lines 4370 and 4639) add `fetchHeaderModel();`.

In `selectSession` (line 5522), after `updateReviewBar();` add:

```js
  headerGit = null;
  headerModel = '';
  fetchGitStatus();
```

In `chooseModel` (line 6101), after `modelPickerData = {...modelPickerData, current: data.current || id};` add:

```js
    headerModel = modelPickerData.current;
    renderHeaderSub();
```

Wire the git segment near the other header listeners (line 6637, `document.getElementById('mode-btn').addEventListener('click', cycleMode);`):

```js
hsGit.addEventListener('click', () => openDiffReview('repository'));
```

- [ ] **Step 7: Run the new tests and the whole suite**

Run: `go test -run 'TestHeader' -count=1 ./...`
Expected: PASS.

Run: `go test ./... && node --test index_test.mjs`
Expected: PASS. If `ui_test.go` reconnect tests fail on `statusText.className`, the class strings were changed; they must stay `header-status connected`, `header-status reconnecting`, `header-status error`.

- [ ] **Step 8: Commit**

```bash
git add index.html header_ui_test.go
git commit -m "feat: chat header shows project, provider icon, mode word and branch"
```

---

### Task 3: Grabber, toolbar, kebab trim, review bar removal

**Files:**
- Modify: `index.html` header markup (grabber inside `#header`), new `#chat-toolbar` after `#header`, CSS, `#chat-menu` markup (line 1813), `cloneSession` (line 5871), `forkSession` (line 5894), `refreshForkEntry` (line 5936), chat menu handlers (lines 6893 to 6990), `paintCatalogueEntry` (line 6908), `updateReviewBar` (line 8296), `#review-bar` markup (line 1765) and CSS (lines 592 to 598).
- Modify: `ui_test.go:2140-2145`
- Modify: `README.md:35`
- Test: `header_ui_test.go`

**Interfaces:**
- Consumes: `fetchGitStatus()`, `headerGit`, `headerModel`, `renderHeaderSub()` from Task 2; `openDiffReview(scope)`, `openModelPicker()`, `renderPins()`, `pinsEl`, `loadPins()`, `cloneSession()`, `forkSession()`, `refreshForkEntry(btn)`, `catalogueFlow(id, stillHere)`, `fetchCatalogueState(id)`, `catalogueStates`, `currentSessionIdForCatalogue()`.
- Produces: `function setToolbarOpen(open)`, `function toolbarOpen()`, `function setToolLabel(btn, text)`, `function renderToolbar()`.

- [ ] **Step 1: Write the failing Chrome test**

Append to `header_ui_test.go`:

```go
func TestHeaderToolbar(t *testing.T) {
	page := newHeaderTestPage(t, "")
	page.waitFor(t, `document.getElementById('hs-branch').textContent === 'syzygy'`)
	closed := page.evalObject(t, `(()=>{const g=document.getElementById('header-grabber');const r=g.getBoundingClientRect();
		return {height:r.height, hidden:document.getElementById('chat-toolbar').hidden, messages:document.getElementById('messages').getBoundingClientRect().height,
			reviewBar:!!document.getElementById('review-bar'), kebab:[...document.querySelectorAll('#chat-menu button')].filter(b=>b.style.display!=='none').map(b=>b.id).join(',')};})()`)
	if closed["hidden"] != true || closed["reviewBar"] != false {
		t.Fatalf("closed state = %v", closed)
	}
	if h, _ := closed["height"].(float64); h < 44 {
		t.Fatalf("grabber tap target %v px, want at least 44", h)
	}
	if closed["kebab"] != "cm-turn-nav,cm-pin,cm-kill" {
		t.Fatalf("kebab items = %v", closed["kebab"])
	}
	page.eval(t, `document.getElementById('header-grabber').click()`)
	page.waitFor(t, `document.getElementById('chat-toolbar').hidden === false`)
	open := page.evalObject(t, `(()=>{const tb=document.getElementById('chat-toolbar');
		return {items:[...tb.querySelectorAll('button')].map(b=>b.id).join(','), labels:[...tb.querySelectorAll('.tb-label')].map(e=>e.textContent).join(','),
			dirty:document.getElementById('tb-git').classList.contains('dirty'), height:tb.getBoundingClientRect().height,
			messages:document.getElementById('messages').getBoundingClientRect().height, stored:localStorage.getItem('acp-toolbar-open'),
			widths:[...tb.querySelectorAll('button')].map(b=>Math.round(b.getBoundingClientRect().width)).join(',')};})()`)
	if open["items"] != "tb-git,tb-model,tb-pinned,tb-fork,tb-clone,tb-catalogue" {
		t.Fatalf("toolbar items = %v", open["items"])
	}
	if open["labels"] != "syzygy,fable,Pinned,Fork,Clone,Catalogue" || open["dirty"] != true || open["stored"] != "true" {
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -run TestHeaderToolbar -count=1 ./...`
Expected: FAIL, `header-grabber` is null.

- [ ] **Step 3: Add the grabber and toolbar markup**

Inside `#header`, after the `<div class="header-actions">...</div>` block and before the `history-loading` line, add:

```html
  <button id="header-grabber" type="button" aria-label="Toolbar" aria-expanded="false" aria-controls="chat-toolbar"><span></span></button>
```

Directly after `</div>` that closes `#header`, add:

```html
<div id="chat-toolbar" hidden>
  <button id="tb-git" type="button" disabled><svg viewBox="0 0 24 24" aria-hidden="true"><line x1="6" x2="6" y1="3" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/></svg><span class="tb-label">git</span></button>
  <button id="tb-model" type="button"><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="16" rx="2"/><rect x="9" y="9" width="6" height="6"/><path d="M15 2v2M9 2v2M15 20v2M9 20v2M2 15h2M2 9h2M20 15h2M20 9h2"/></svg><span class="tb-label">model</span></button>
  <button id="tb-pinned" type="button"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1z"/></svg><span class="tb-label">Pinned</span></button>
  <button id="tb-fork" type="button"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><circle cx="18" cy="6" r="3"/><path d="M18 9v2c0 .6-.4 1-1 1H7c-.6 0-1-.4-1-1V9"/><path d="M12 12v3"/></svg><span class="tb-label">Fork</span></button>
  <button id="tb-clone" type="button"><svg viewBox="0 0 24 24" aria-hidden="true"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg><span class="tb-label">Clone</span></button>
  <button id="tb-catalogue" type="button"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m19 21-7-4-7 4V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v16z"/></svg><span class="tb-label">Catalogue</span></button>
</div>
```

Replace the `#chat-menu` block (line 1813) with:

```html
<div id="chat-menu">
  <button id="cm-turn-nav">Show turn nav</button>
  <button id="cm-pin">Pin chat</button>
  <button id="cm-uncatalogue" style="display:none">Uncatalogue</button>
  <button id="cm-kill" style="color:var(--red)">Kill session</button>
</div>
```

Delete the `<div id="review-bar">...</div>` line (1765).

- [ ] **Step 4: Add the CSS**

Replace the `#review-bar` and `#review-repo-btn` rules (lines 592 to 598) with:

```css
/* Grabber: a pill under row 2 that pulls the toolbar down. The header
   wraps so the grabber takes its own full-width line. */
#header { flex-wrap: wrap; padding-bottom: 0; }
#header-grabber {
  flex-basis: 100%; height: 22px; margin-top: -8px; padding: 0;
  border: none; background: none; display: flex; align-items: flex-end;
  justify-content: center; cursor: pointer; -webkit-tap-highlight-color: transparent;
  touch-action: pan-x; position: relative;
}
#header-grabber::before { content: ""; position: absolute; inset: -22px 0 0 0; }
#header-grabber span {
  width: 36px; height: 4px; border-radius: 2px; background: var(--bg2);
  margin-bottom: 6px; transition: background 0.15s ease;
}
#header-grabber[aria-expanded="true"] span { background: var(--fg-mute); }
#chat-toolbar {
  display: flex; justify-content: space-between; align-items: flex-start;
  padding: 6px 12px 8px; background: var(--bg0); border-bottom: 1px solid var(--bg2);
  flex-shrink: 0;
}
#chat-toolbar[hidden] { display: none; }
#chat-toolbar button {
  width: 56px; min-height: 48px; padding: 4px 0 2px; border: none; background: none;
  display: flex; flex-direction: column; align-items: center; gap: 4px;
  color: var(--fg-dim); font-family: var(--mono); font-size: 10px; cursor: pointer;
  position: relative; -webkit-tap-highlight-color: transparent;
}
#chat-toolbar button:active { color: var(--fg); }
#chat-toolbar button:disabled { color: var(--fg-mute); cursor: default; }
#chat-toolbar button svg { width: 20px; height: 20px; fill: none; stroke: currentColor; stroke-width: 2; stroke-linecap: round; stroke-linejoin: round; }
#chat-toolbar .tb-label { max-width: 56px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
#tb-git .tb-label { color: var(--fg); }
#tb-git:disabled .tb-label { color: var(--fg-mute); }
#tb-git.dirty::after {
  content: ""; position: absolute; left: 34px; top: 2px; width: 7px; height: 7px;
  border-radius: 50%; background: var(--orange);
}
#tb-catalogue.kept svg { fill: currentColor; }
#tb-catalogue.kept { color: var(--yellow); }
```

Because `#header` now has `padding-bottom: 0`, the grabber's 22px supplies the bottom spacing. The `::before` pseudo extends the hit area up over row 2 so the target is 44px.

- [ ] **Step 5: Toolbar state and handlers**

Replace `updateReviewBar` (line 8296) and the `reviewRepoBtn` listener at line 8309, and delete `const reviewRepoBtn = ...` (line 7892), with this block placed right after the `const cmUncatalogue = ...` line (line 6904):

```js
// --- Toolbar: pulled down from the header grabber ---
const TOOLBAR_KEY = 'acp-toolbar-open';
const headerGrabber = document.getElementById('header-grabber');
const chatToolbar = document.getElementById('chat-toolbar');
const tbGit = document.getElementById('tb-git');
const tbModel = document.getElementById('tb-model');
const tbCatalogue = document.getElementById('tb-catalogue');
function setToolLabel(btn, text) { btn.querySelector('.tb-label').textContent = text; }
function toolbarOpen() {
  try { return localStorage.getItem(TOOLBAR_KEY) === 'true'; } catch (_) { return false; }
}
function setToolbarOpen(open) {
  try { localStorage.setItem(TOOLBAR_KEY, open ? 'true' : 'false'); } catch (_) {}
  chatToolbar.hidden = !open;
  headerGrabber.setAttribute('aria-expanded', open ? 'true' : 'false');
  if (open) { renderToolbar(); fetchGitStatus(); }
}
function renderToolbar() {
  const git = headerGit && headerGit.branch ? headerGit : null;
  tbGit.disabled = !git;
  setToolLabel(tbGit, git ? git.branch : 'git');
  tbGit.classList.toggle('dirty', !!git && (git.staged + git.unstaged + git.untracked) > 0);
  setToolLabel(tbModel, headerModel || 'model');
  refreshForkEntry(document.getElementById('tb-fork'));
  refreshCatalogueEntry();
}
headerGrabber.addEventListener('click', () => setToolbarOpen(chatToolbar.hidden));
// Drag: down past 24px opens, up past 24px closes. Tap still toggles.
(() => {
  let startY = null;
  headerGrabber.addEventListener('touchstart', e => { startY = e.touches[0].clientY; }, {passive: true});
  headerGrabber.addEventListener('touchmove', e => {
    if (startY === null) return;
    const dy = e.touches[0].clientY - startY;
    if (dy > 24 && chatToolbar.hidden) { setToolbarOpen(true); startY = null; }
    else if (dy < -24 && !chatToolbar.hidden) { setToolbarOpen(false); startY = null; }
  }, {passive: true});
  headerGrabber.addEventListener('touchend', () => { startY = null; });
})();
tbGit.addEventListener('click', () => openDiffReview('repository'));
tbModel.addEventListener('click', openModelPicker);
document.getElementById('tb-pinned').addEventListener('click', () => { renderPins(); pinsEl.classList.add('visible'); });
document.getElementById('tb-clone').addEventListener('click', () => { cloneSession(); });
document.getElementById('tb-fork').addEventListener('click', () => { forkSession(); });
tbCatalogue.addEventListener('click', async () => {
  const id = currentSessionIdForCatalogue();
  if (!id) { alert('This chat has no session id yet.'); return; }
  const chat = currentBufferName;
  const stillHere = () => currentBufferName === chat && chatViewEl.classList.contains('visible');
  try { await catalogueFlow(id, stillHere); }
  catch (err) { if (stillHere()) alert('Catalogue failed: ' + err.message); }
});
setToolbarOpen(toolbarOpen());
```

`renderToolbar` runs after `renderHeaderSub` so the two stay in step: at the end of `renderHeaderSub()` (Task 2) add `if (typeof renderToolbar === 'function' && !chatToolbar.hidden) renderToolbar();`. Since `chatToolbar` is declared later, guard with `typeof chatToolbar !== 'undefined' &&` in front.

Update the existing functions:

- `cloneSession` (line 5871): `const btn = document.getElementById('tb-clone');` and replace the two `btn.textContent = ...` with `setToolLabel(btn, 'Cloning…')` and `setToolLabel(btn, 'Clone')`. Remove `closeChatMenu();` from its `finally`.
- `forkSession` (line 5894): same with `tb-fork`, `'Forking…'`, `'Fork'`.
- `refreshForkEntry(btn)` (line 5936): every `btn.textContent = X` becomes `setToolLabel(btn, X)`.
- `paintCatalogueEntry(state)` (line 6908):

```js
function paintCatalogueEntry(state) {
  const kept = !!(state && state.catalogued);
  tbCatalogue.classList.toggle('kept', kept);
  setToolLabel(tbCatalogue, kept ? 'Catalogued' : 'Catalogue');
  tbCatalogue.title = kept && state.note ? state.note : '';
  cmUncatalogue.style.display = kept ? '' : 'none';
}
```

- `refreshCatalogueEntry` (line 6915): `cmCatalogue.disabled = !sessionIdNow;` becomes `tbCatalogue.disabled = !sessionIdNow;` and `cmCatalogue.textContent = 'Catalogue'` becomes `setToolLabel(tbCatalogue, 'Catalogue')`.
- Delete `const cmPinned`, `const cmFork`, `const cmCatalogue` and their listeners, the `cm-clone` and `cm-model` listeners, and in the `chat-menu-btn` click handler delete the `cmPinned.style.display`, `refreshForkEntry(cmFork)` and `refreshCatalogueEntry()` lines, then add `refreshCatalogueEntry();` back so `Uncatalogue` visibility is current.
- Remove the two `updateReviewBar();` calls (lines 4110 and 5526) and the function.

- [ ] **Step 6: Update the clone UI test and the README**

In `ui_test.go` around line 2140, replace:

```js
		document.getElementById('chat-menu-btn').click();
		const menuOpen = chatMenu.classList.contains('visible');
		const clone = document.getElementById('cm-clone');
		clone.click();
		const busyLabel = clone.textContent;
```

with:

```js
		setToolbarOpen(true);
		const menuOpen = !document.getElementById('chat-toolbar').hidden;
		const clone = document.getElementById('tb-clone');
		clone.click();
		const busyLabel = clone.querySelector('.tb-label').textContent;
```

Read the assertions that follow: if one expects `menuOpen === true` keep it; the toolbar being open plays the same role.

In `README.md` replace the sentence on line 35 starting `- **Before commit** is computed fresh` so that it opens with: `- **Before commit** opens from the branch segment in the chat header or the git button on the pull-down toolbar, and is computed fresh from the session's repository on every open or `Refresh`: ...` keeping the rest of the paragraph.

- [ ] **Step 7: Run everything**

Run: `go test -count=1 ./... && node --test index_test.mjs`
Expected: PASS. `TestDiffReviewUI` uses `openDiffReview('repository')` directly, so it does not depend on the removed bar.

- [ ] **Step 8: Visual check**

Run: `SYZYGY_UI_SHOTS=/tmp/shots go test -run TestHeaderToolbar -count=1 ./...` after adding `saveUIShot(t, page, "header-toolbar-open")` right after the toolbar `waitFor` in the test. View `/tmp/shots/header-toolbar-open.png` and compare with the Figma frame `D toolbar open`. Then remove the `saveUIShot` line if it was only for this check, or keep it: `saveUIShot` is a no-op without the env var.

- [ ] **Step 9: Commit**

```bash
git add index.html header_ui_test.go ui_test.go README.md
git commit -m "feat: pull-down chat toolbar replaces the review bar and trims the menu"
```

---

## Self-review notes

- Spec coverage: title rule (Task 2 step 5), provider icon and model (Task 2 steps 5 and 6), mode word (Task 2 step 6), branch segment with dot and tap (Task 2 steps 4 to 6), status via icon and line (Task 2 step 5), grabber, toolbar, persisted state, kebab trim, review bar removal (Task 3), server endpoint and fetch policy (Task 1, Task 2 step 6, Task 3 step 5), README (Task 3 step 6). The spec's `Uncatalogue` removal is relaxed: it stays in the kebab, hidden unless the chat is catalogued, because the toolbar catalogue button only saves or edits.
- Names used across tasks: `gitStatusResult`, `handleGitStatus`, `headerGit`, `headerModel`, `renderHeaderSub`, `fetchGitStatus`, `fetchHeaderModel`, `setToolLabel`, `renderToolbar`, `setToolbarOpen`, `toolbarOpen`.
