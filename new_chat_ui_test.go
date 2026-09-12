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
	"sync"
	"testing"
)

const newChatCatalogFixture = `{"defaultAgent":"codex","agents":[
 {"id":"codex","name":"Codex","models":[{"id":"sol","name":"Sol"},{"id":"astra","name":"Astra"}],"modes":[{"id":"agent","name":"Agent","description":"Ask outside the project"},{"id":"full","name":"Full access","description":"No approval prompts"}],"efforts":[{"id":"high","name":"High"},{"id":"low","name":"Low"}],"defaults":{"model":"sol","mode":"agent","effort":""}},
 {"id":"claude","name":"Claude Code","models":[{"id":"sonnet","name":"Sonnet"}],"modes":[{"id":"manual","name":"Manual"}],"efforts":[],"defaults":{"model":"sonnet","mode":"manual","effort":""}}
]}`

func newChatTestPage(t *testing.T) *chromePage {
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
	mux.HandleFunc("/api/launch-options", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, newChatCatalogFixture) })
	mux.HandleFunc("/api/presets", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"presets":[{"key":"s","label":"Sol · Full","agent":"codex","model":"sol","mode":"full","effort":"high"}]}`)
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"projects":[{"name":"acp-mobile","path":"/src/acp-mobile"},{"name":"dotfiles","path":"/src/dotfiles"}]}`)
	})
	var mu sync.Mutex
	spawned := false
	mux.HandleFunc("/api/spawn", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		spawned = true
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"bufferName":"intended chat"}`)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sessions := []map[string]interface{}{}
		if spawned {
			sessions = []map[string]interface{}{{"pid": 1, "sessionId": "other", "bufferName": "concurrent chat"}, {"pid": 2, "sessionId": "intended", "bufferName": "intended chat"}}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"sessions": sessions})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	page := openChromePage(t, server.URL)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": 393, "height": 852, "deviceScaleFactor": 1, "mobile": true})
	page.waitFor(t, `typeof openSpawnSheet === 'function'`)
	page.eval(t, `openSpawnSheet('/src/acp-mobile')`)
	return page
}

func TestNewChatPresetOverrideResetAndExactLaunch(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `document.querySelector('#sp-presets button') !== null`)
	state := page.evalObject(t, `(async()=>{
  document.querySelector('#sp-presets button').click();
  document.getElementById('sp-task').value='Keep this draft';document.getElementById('sp-task').dispatchEvent(new Event('input'));
  document.getElementById('sp-model').click();
  [...document.querySelectorAll('#sp-picker-list button')].find(b=>b.textContent.includes('Astra')).click();
  const changed={model:spDraft.settings.model,mode:spDraft.settings.mode,effort:spDraft.settings.effort,modified:document.getElementById('sp-preset-label').textContent,task:spTask.value};
  document.getElementById('sp-reset').click();
  const reset=spDraft.settings.model;
  window.__sent=null;const orig=fetch;window.fetch=async(url,opts)=>{if(url.endsWith('/api/spawn'))window.__sent=JSON.parse(opts.body);return orig(url,opts)};
  SPAWN_POLL_MS=5;window.__selected=null;selectSession=s=>{window.__selected=s.sessionId};
  document.getElementById('sp-go').click();
  for(let i=0;i<100&&!window.__selected;i++)await new Promise(r=>setTimeout(r,20));
  return {changed,reset,sent:window.__sent,selected:window.__selected};
 })()`)
	changed := state["changed"].(map[string]interface{})
	if changed["model"] != "astra" || changed["mode"] != "full" || changed["effort"] != "high" || changed["task"] != "Keep this draft" || !strings.Contains(strings.ToLower(changed["modified"].(string)), "modified") {
		t.Fatalf("override: %#v", state)
	}
	if state["reset"] != "sol" || state["selected"] != "intended" {
		t.Fatalf("reset/correlation: %#v", state)
	}
	sent := state["sent"].(map[string]interface{})
	if sent["settings"].(map[string]interface{})["mode"] != "full" || sent["task"] != "Keep this draft" {
		t.Fatalf("payload %#v", sent)
	}
}

func TestNewChatSearchCannotBecomePathAndDraftSurvives(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 2`)
	state := page.evalObject(t, `(()=>{
  spTask.value='Do not lose me';spTask.dispatchEvent(new Event('input'));
  document.getElementById('sp-project').click();
  const q=document.getElementById('sp-search');q.value='nonexistent';q.dispatchEvent(new Event('input'));
  document.getElementById('sp-back').click();
  const cwd=spDraft.cwd;
  document.getElementById('sp-close').click();openSpawnSheet();
  const task=spTask.value;
  document.getElementById('sp-agent').click();
  [...document.querySelectorAll('#sp-picker-list button')].find(b=>b.textContent.includes('Claude Code')).click();
  return {cwd,task,model:spDraft.settings.model,mode:spDraft.settings.mode,disabled:spGo.disabled};
 })()`)
	if state["cwd"] != "/src/acp-mobile" || state["task"] != "Do not lose me" {
		t.Fatalf("draft/path: %#v", state)
	}
	if state["model"] != "" || state["mode"] != "" || state["disabled"] != true {
		t.Fatalf("incompatible settings must need a choice: %#v", state)
	}
}

func TestNewChatCenteredHeadersAndKeyboardHeight(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 2`)
	for _, height := range []int{852, 430} {
		page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": 320, "height": height, "deviceScaleFactor": 1, "mobile": true})
		state := page.evalObject(t, `(()=>{const title=document.getElementById('sp-title').getBoundingClientRect(),sheet=spSheet.getBoundingClientRect(),go=spGo.getBoundingClientRect();document.getElementById('sp-project').click();const back=document.getElementById('sp-back');const out={center:Math.abs((title.left+title.right)/2-(sheet.left+sheet.right)/2),footer:go.bottom<=innerHeight,overflow:document.documentElement.scrollWidth>innerWidth,backLeft:back.getBoundingClientRect().left<80,backText:back.textContent.trim(),icon:!!back.querySelector('img')};back.click();return out})()`)
		if state["center"].(float64) > 1 || state["footer"] != true || state["overflow"] != false || state["backLeft"] != true || state["backText"] != "" || state["icon"] != true {
			t.Fatalf("height %d: %#v", height, state)
		}
	}
}

func TestNewChatCustomPresetPinsAndPartialFailure(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 2 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	state := page.evalObject(t, `(async()=>{
  document.querySelector('#sp-presets button').click();
  spEl('save').click();spEl('save-name').value='My setup';spEl('picker-action').click();
  const custom=spCustom.find(p=>p.label==='My setup');
  spEl('project').click();document.querySelector('#sp-picker-list button[aria-label="Pin dotfiles"]').click();
  const pinned=spPins.includes('/src/dotfiles');spEl('back').click();
  spTask.value='Keep unsent prompt';spTask.dispatchEvent(new Event('input'));
  let posts=0;const original=fetch;window.fetch=async(url,opts)=>{
    if(url.endsWith('/api/spawn')){posts++;return {ok:false,json:async()=>({ok:false,error:'Effort rejected',bufferName:'partial chat'})};}
    if(url.endsWith('/api/sessions'))return {ok:true,json:async()=>({sessions:[{pid:4,sessionId:'partial',bufferName:'partial chat'}]})};
    return original(url,opts);
  };
  SPAWN_POLL_MS=5;selectSession=s=>{};spGo.click();
  for(let i=0;i<100&&spBusy;i++)await new Promise(r=>setTimeout(r,10));
  const failed={message:spEl('message').textContent,task:spDraft.task,pending:spRecoveryBuffer};
  spGo.click();for(let i=0;i<100&&spBusy;i++)await new Promise(r=>setTimeout(r,10));
  return {custom,pinned,failed,posts,task:spDraft.task};
 })()`)
	custom := state["custom"].(map[string]interface{})
	failed := state["failed"].(map[string]interface{})
	if custom["model"] != "sol" || custom["mode"] != "full" || custom["effort"] != "high" || state["pinned"] != true {
		t.Fatalf("custom/pins: %#v", state)
	}
	if failed["pending"] != "partial chat" || failed["task"] != "Keep unsent prompt" || state["posts"] != float64(1) || state["task"] != "Keep unsent prompt" {
		t.Fatalf("partial launch must preserve unsent prompt and never post twice: %#v", state)
	}
}

func TestNewChatVisualReview(t *testing.T) {
	dir := os.Getenv("SYZYGY_UI_SHOTS")
	if dir == "" {
		t.Skip("Set SYZYGY_UI_SHOTS to capture mobile review images")
	}
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 2 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	page.eval(t, `document.querySelector('#sp-presets button').click()`)
	for _, view := range []string{"main", "project", "model", "mode"} {
		page.eval(t, fmt.Sprintf(`openSpawnView(%q)`, view))
		var shot struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(page.call(t, "Page.captureScreenshot", map[string]interface{}{"format": "png"}), &shot); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(shot.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "new-chat-"+view+".png"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewChatMissingRecoveryCanReturnToDraft(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 2 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(async()=>{
  document.querySelector('#sp-presets button').click();spTask.value='Unsent work';spTask.dispatchEvent(new Event('input'));
  spRecoveryBuffer='missing buffer';spRecoveryKeepsTask=true;persistSpawnDraft();renderSpawnMain();
  const locked=spEl('model').disabled;
  spEl('release').click();
  return {locked,editable:!spEl('model').disabled,task:spDraft.task,pending:spRecoveryBuffer,canLaunch:!spGo.disabled,stored:JSON.parse(localStorage.getItem('syzygy.launch.draft')).pendingBuffer};
 })()`)
	if state["locked"] != true || state["editable"] != true || state["task"] != "Unsent work" || state["pending"] != "" || state["stored"] != "" || state["canLaunch"] != true {
		t.Fatalf("recovery must be dismissible without losing the draft: %#v", state)
	}
}
