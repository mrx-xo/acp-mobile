package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// diffReviewFixture is the deterministic /api/diff-review reply for the
// browser tests: a 152-line Go hunk with lines long enough to wrap at
// phone widths, a partially staged text file, and enough small files
// for the list itself to scroll.
func diffReviewFixture(scope string) reviewResult {
	long := reviewFile{Path: "cmd/redial/redial.go", Status: "modified", Source: "staged"}
	hunk := reviewHunk{Header: "@@ -3107,80 +3107,94 @@ func (d *Dialer) redial(ctx context.Context) error {"}
	old, new := 3107, 3107
	for i := 0; i < 152; i++ {
		text := fmt.Sprintf("line %d := dial(ctx, %d)", i, i)
		if i%5 == 0 {
			text = fmt.Sprintf("backoff := time.Duration(math.Min(float64(base)*math.Pow(2, float64(attempt)), float64(maxBackoff))) // line %d wraps", i)
		}
		switch {
		case i >= 12 && i < 60:
			hunk.Lines = append(hunk.Lines, reviewLine{Kind: "removed", Old: old, Text: text})
			old++
			long.Removed++
		case i >= 60 && i < 130:
			hunk.Lines = append(hunk.Lines, reviewLine{Kind: "added", New: new, Text: text})
			new++
			long.Added++
		default:
			hunk.Lines = append(hunk.Lines, reviewLine{Kind: "context", Old: old, New: new, Text: text})
			old++
			new++
		}
	}
	long.Hunks = []reviewHunk{hunk, {Header: "@@ -3300,2 +3314,3 @@", Lines: []reviewLine{
		{Kind: "context", Old: 3300, New: 3314, Text: "return nil"},
		{Kind: "added", New: 3315, Text: "// second change"},
		{Kind: "context", Old: 3301, New: 3316, Text: "}"},
	}}}
	files := []reviewFile{long,
		{Path: "a.txt", Status: "modified", Source: "staged", Added: 1, Hunks: []reviewHunk{{Header: "@@ -1,3 +1,4 @@", Lines: []reviewLine{
			{Kind: "context", Old: 1, New: 1, Text: "one"}, {Kind: "added", New: 2, Text: "two <b>bold</b>"}}}}},
		{Path: "a.txt", Status: "modified", Source: "unstaged", Added: 1, Removed: 1, Hunks: []reviewHunk{{Header: "@@ -4 +4 @@", Lines: []reviewLine{
			{Kind: "removed", Old: 4, Text: "x"}, {Kind: "added", New: 4, Text: "y"}}}}},
		{Path: "img.png", Status: "added", Source: "untracked", Binary: true},
	}
	for i := 0; i < 8; i++ {
		files = append(files, reviewFile{Path: fmt.Sprintf("notes/untracked %d.md", i), Status: "added", Source: "untracked", Added: 1,
			Hunks: []reviewHunk{{Header: "@@ -0,0 +1 @@", Lines: []reviewLine{{Kind: "added", New: 1, Text: "# note"}}}}})
	}
	res := reviewResult{Scope: scope, Repository: "/src/demo", Branch: "main", CapturedAt: time.Date(2026, 9, 19, 17, 42, 0, 0, time.UTC), Available: true, Files: files}
	if scope == "turn" {
		for i := range res.Files {
			res.Files[i].Source = "turn"
		}
	}
	finishReview(&res, "No changes")
	return res
}

func newDiffReviewTestPage(t *testing.T, width, height int) *chromePage {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/assets/", handleAsset)
	mux.Handle("/fonts/", http.FileServerFS(fontsFS))
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"sessions":[]}`)
	})
	mux.HandleFunc("/api/diff-review", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(diffReviewFixture(r.URL.Query().Get("scope")))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	page := openChromePage(t, server.URL)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": true})
	page.waitFor(t, `typeof openDiffReview === 'function'`)
	page.eval(t, `currentSockPid = 1; sessionId = 'ui-sess'; openDiffReview('repository')`)
	page.waitFor(t, `document.querySelectorAll('#dr-list .dr-file').length === 12`)
	return page
}

func saveUIShot(t *testing.T, page *chromePage, name string) {
	t.Helper()
	dir := os.Getenv("SYZYGY_UI_SHOTS")
	if dir == "" {
		return
	}
	var shot struct{ Data string }
	if err := json.Unmarshal(page.call(t, "Page.captureScreenshot", map[string]interface{}{"format": "png"}), &shot); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".png"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// fixedChrome returns the top edges of the parts that must not move when
// the diff viewport scrolls.
const fixedChromeJS = `(()=>{const r=id=>document.getElementById(id).getBoundingClientRect();
  return [r('dr-header').top, r('dr-header').height, r('dr-tabs').top, r('dr-controls').top, r('dr-nav').top, r('dr-nav').bottom].join(',');})()`

func TestDiffReviewUI(t *testing.T) {
	page := newDiffReviewTestPage(t, 393, 852)

	list := page.evalObject(t, `(()=>{
    const rows=[...document.querySelectorAll('#dr-list .dr-file')];
    const heights=rows.map(r=>r.getBoundingClientRect().height);
    const widths=rows.map(r=>r.getBoundingClientRect().width);
    const a=rows.filter(r=>r.querySelector('.dr-file-path').textContent==='a.txt').map(r=>r.querySelector('.dr-badge').textContent);
    const sections=[...document.querySelectorAll('#dr-list .dr-section')].map(s=>s.textContent);
    return {minHeight:Math.min(...heights), minWidth:Math.min(...widths), listWidth:document.getElementById('dr-list').clientWidth,
      a, sections, summary:document.getElementById('dr-summary').textContent,
      overflow:document.documentElement.scrollWidth<=window.innerWidth, header:document.getElementById('dr-header').getBoundingClientRect().height,
      escaped:!rows.some(r=>r.querySelector('b'))};
  })()`)
	if list["minHeight"].(float64) < 44 || list["minWidth"].(float64) < list["listWidth"].(float64)-33 {
		t.Fatalf("file rows must be full-width touch targets of at least 44px: %#v", list)
	}
	if a, _ := list["a"].([]interface{}); len(a) != 2 || a[0] == a[1] {
		t.Fatalf("staged and unstaged copies of a.txt must stay separate: %#v", list["a"])
	}
	if s, _ := list["sections"].([]interface{}); len(s) != 3 || s[0] != "Staged" || s[1] != "Unstaged" || s[2] != "Untracked" {
		t.Fatalf("repository scope labels each source: %#v", list["sections"])
	}
	if list["overflow"] != true || list["header"].(float64) != 64 {
		t.Fatalf("no horizontal page overflow and a 64px header: %#v", list)
	}
	if !strings.Contains(list["summary"].(string), "12 files") {
		t.Fatalf("summary counts files: %q", list["summary"])
	}
	saveUIShot(t, page, "diff-files")

	// Back from a file restores the list's scroll position.
	page.eval(t, `document.getElementById('dr-list').scrollTop = 150`)
	page.eval(t, `openDiffFile(6)`)
	page.waitFor(t, `!document.getElementById('dr-reader').hidden && document.getElementById('dr-list-view').hidden`)
	page.eval(t, `document.getElementById('dr-reader-back').click()`)
	page.waitFor(t, `!document.getElementById('dr-list-view').hidden`)
	if back := page.eval(t, `document.getElementById('dr-list').scrollTop`); back.(float64) != 150 {
		t.Fatalf("back must restore the list scroll position, got %v", back)
	}

	// The long file: independent viewport scroll, fixed chrome, wrapping.
	page.eval(t, `openDiffFile(0)`)
	page.waitFor(t, `document.querySelectorAll('#dr-viewport .diff-row').length === 155`)
	before := page.eval(t, fixedChromeJS)
	reader := page.evalObject(t, `(()=>{
    const v=document.getElementById('dr-viewport'); v.scrollTop=v.scrollHeight; const scrolled=v.scrollTop;
    const rows=[...v.querySelectorAll('.diff-row')];
    const codes=rows.map(r=>r.querySelector('.diff-code'));
    const wrapped=rows.filter(r=>r.getBoundingClientRect().height>=44).length;
    const single=rows.filter(r=>r.getBoundingClientRect().height===26).length;
    const codeWidth=codes[1].getBoundingClientRect().width;
    const numWidth=rows[1].querySelector('.diff-num').getBoundingClientRect().width;
    const markWidth=rows[1].querySelector('.diff-mark').getBoundingClientRect().width;
    const removed=rows.find(r=>r.classList.contains('removed')); const added=rows.find(r=>r.classList.contains('added'));
    return {scrolled, viewportHeight:v.clientHeight, pageScroll:document.scrollingElement.scrollTop, wrapped, single,
      codeWidth, numWidth, markWidth, noXOverflow:v.scrollWidth<=v.clientWidth && codes.every(c=>c.scrollWidth<=c.clientWidth+1),
      removedNum:removed.querySelector('.diff-num').textContent, removedMark:removed.querySelector('.diff-mark').textContent,
      addedNum:added.querySelector('.diff-num').textContent, addedMark:added.querySelector('.diff-mark').textContent,
      heading:document.getElementById('dr-change-heading').textContent, hunks:v.querySelectorAll('.diff-hunk').length,
      kw:!!v.querySelector('.tok-kw'), fn:!!v.querySelector('.tok-fn'), cm:!!v.querySelector('.tok-cm'),
      after:(()=>{const r=id=>document.getElementById(id).getBoundingClientRect();
        return [r('dr-header').top, r('dr-header').height, r('dr-tabs').top, r('dr-controls').top, r('dr-nav').top, r('dr-nav').bottom].join(',');})()};
  })()`)
	if reader["scrolled"].(float64) <= 0 || reader["pageScroll"].(float64) != 0 {
		t.Fatalf("the diff viewport must scroll on its own, not the page: %#v", reader)
	}
	if reader["after"] != before {
		t.Fatalf("header, tabs, controls and navigation must stay fixed while the viewport scrolls: %v -> %v", before, reader["after"])
	}
	if reader["wrapped"].(float64) < 20 || reader["single"].(float64) < 100 || reader["noXOverflow"] != true {
		t.Fatalf("long code wraps inside its row without horizontal overflow: %#v", reader)
	}
	if reader["codeWidth"].(float64) != 301 || reader["numWidth"].(float64) != 24 || reader["markWidth"].(float64) != 12 {
		t.Fatalf("with numbers the code column is 301px next to a 24px number and 12px marker: %#v", reader)
	}
	if reader["removedNum"] != "3119" || reader["removedMark"] != "-" || reader["addedNum"] != "3119" || reader["addedMark"] != "+" {
		t.Fatalf("removals show old numbers, additions new: %#v", reader)
	}
	if !strings.HasPrefix(reader["heading"].(string), "Change 1 of 2") || reader["hunks"].(float64) != 2 {
		t.Fatalf("every hunk renders and the heading counts them: %#v", reader)
	}
	if reader["kw"] != true || reader["fn"] != true || reader["cm"] != true {
		t.Fatalf("Go code gets keyword, callable and comment colors: %#v", reader)
	}
	if reader["viewportHeight"].(float64) < 500 {
		t.Fatalf("the reader viewport should keep most of the screen: %#v", reader)
	}
	saveUIShot(t, page, "diff-reader-numbers")

	// Next change jumps to the second hunk; Previous returns.
	page.eval(t, `document.getElementById('dr-next').click()`)
	page.waitFor(t, `document.getElementById('dr-change-heading').textContent.startsWith('Change 2 of 2')`)
	nav := page.evalObject(t, `(()=>{const v=document.getElementById('dr-viewport'); const h=v.querySelectorAll('.diff-hunk')[1];
    const vr=v.getBoundingClientRect(), hr=h.getBoundingClientRect();
    // The last hunk sits at the end of the file, so the viewport can only
    // scroll as far as its bottom: the hunk must be in view, scrolled as far as it goes.
    return {visible:hr.top>=vr.top-1 && hr.bottom<=vr.bottom+1, atEnd:Math.abs(v.scrollTop-(v.scrollHeight-v.clientHeight))<2,
      prev:document.getElementById('dr-prev').disabled, next:document.getElementById('dr-next').disabled};})()`)
	if nav["visible"] != true || nav["atEnd"] != true || nav["prev"] != false || nav["next"] != false {
		t.Fatalf("next change brings the second hunk into view and more files remain: %#v", nav)
	}
	// Past the last hunk, Next crosses into the next file; Previous comes back.
	page.eval(t, `document.getElementById('dr-next').click()`)
	page.waitFor(t, `document.getElementById('dr-change-heading').textContent === 'Change 1 of 1 · file 2 of 12' && document.getElementById('dr-file-label').textContent.includes('a.txt')`)
	page.eval(t, `document.getElementById('dr-prev').click()`)
	page.waitFor(t, `document.getElementById('dr-change-heading').textContent.startsWith('Change 2 of 2 · file 1 of 12')`)

	// Hiding numbers widens the code column; the marker stays visible.
	page.eval(t, `document.getElementById('dr-numbers-btn').click()`)
	page.waitFor(t, `document.getElementById('diff-review').classList.contains('diff-numbers-hidden')`)
	hidden := page.evalObject(t, `(()=>{
    const row=document.querySelectorAll('#dr-viewport .diff-row.added')[0];
    const mark=row.querySelector('.diff-mark').getBoundingClientRect();
    return {codeWidth:row.querySelector('.diff-code').getBoundingClientRect().width, markWidth:mark.width, markVisible:mark.height>0,
      numVisible:row.querySelector('.diff-num').getBoundingClientRect().width>0,
      label:document.getElementById('dr-numbers-btn').textContent, pressed:document.getElementById('dr-numbers-btn').getAttribute('aria-pressed'),
      stored:localStorage.getItem('acp-diff-line-numbers'), btn:document.getElementById('dr-numbers-btn').getBoundingClientRect().height};
  })()`)
	if hidden["codeWidth"].(float64) != 329 || hidden["markWidth"].(float64) != 12 || hidden["markVisible"] != true || hidden["numVisible"] != false {
		t.Fatalf("hidden numbers hand their column to the code and keep the marker: %#v", hidden)
	}
	if hidden["label"] != "Show numbers" || hidden["pressed"] != "true" || hidden["stored"] != "false" || hidden["btn"].(float64) < 44 {
		t.Fatalf("the toggle reports its state and persists it: %#v", hidden)
	}
	saveUIShot(t, page, "diff-reader-hidden")

	// The reader scroll position survives a trip to the list.
	page.eval(t, `document.getElementById('dr-viewport').scrollTop = 333`)
	page.eval(t, `document.getElementById('dr-reader-back').click()`)
	page.waitFor(t, `!document.getElementById('dr-list-view').hidden`)
	page.eval(t, `openDiffFile(0)`)
	page.waitFor(t, `document.getElementById('dr-viewport').scrollTop === 333`)

	// The preference survives a reload.
	page.call(t, "Page.reload", map[string]interface{}{})
	page.waitFor(t, `typeof openDiffReview === 'function' && document.getElementById('diff-review').classList.contains('diff-numbers-hidden')`)
	if label := page.eval(t, `document.getElementById('dr-numbers-btn').textContent`); label != "Show numbers" {
		t.Fatalf("the hidden preference must restore after reload, got %v", label)
	}

	// The turn scope never borrows the repository diff.
	page.eval(t, `currentSockPid = 1; sessionId = 'ui-sess'; openDiffReview('turn')`)
	page.waitFor(t, `document.querySelectorAll('#dr-list .dr-file').length === 12 && document.getElementById('dr-scope').textContent.includes('phone')`)
	turn := page.evalObject(t, `(()=>({sections:document.querySelectorAll('#dr-list .dr-section').length,
    badge:document.querySelector('#dr-list .dr-badge').textContent, identity:document.getElementById('dr-identity').textContent}))()`)
	if turn["sections"].(float64) != 0 || turn["badge"] != "This turn" || !strings.HasPrefix(turn["identity"].(string), "Turn saved") {
		t.Fatalf("turn scope: %#v", turn)
	}
	saveUIShot(t, page, "diff-turn-files")
}

func TestDiffReviewUINarrow(t *testing.T) {
	page := newDiffReviewTestPage(t, 320, 700)
	state := page.evalObject(t, `(()=>{
    const rows=[...document.querySelectorAll('#dr-list .dr-file')];
    const minHeight=Math.min(...rows.map(r=>r.getBoundingClientRect().height));
    openDiffFile(0);
    const v=document.getElementById('dr-viewport'); v.scrollTop=v.scrollHeight;
    const codes=[...v.querySelectorAll('.diff-code')];
    return {minHeight, scrolled:v.scrollTop, pageScroll:document.scrollingElement.scrollTop,
      noXOverflow:document.documentElement.scrollWidth<=window.innerWidth && codes.every(c=>c.scrollWidth<=c.clientWidth+1),
      codeWidth:codes[1].getBoundingClientRect().width, navBottom:document.getElementById('dr-nav').getBoundingClientRect().bottom,
      viewportHeight:v.clientHeight, buttons:[...document.querySelectorAll('#diff-review button')].filter(b=>b.offsetParent).every(b=>b.getBoundingClientRect().height>=44)};
  })()`)
	if state["minHeight"].(float64) < 44 || state["scrolled"].(float64) <= 0 || state["pageScroll"].(float64) != 0 || state["noXOverflow"] != true {
		t.Fatalf("narrow phone: rows stay tappable, the viewport scrolls, nothing overflows: %#v", state)
	}
	if state["codeWidth"].(float64) != 228 || state["navBottom"].(float64) > 700 || state["viewportHeight"].(float64) < 300 || state["buttons"] != true {
		t.Fatalf("narrow phone geometry: %#v", state)
	}
}
