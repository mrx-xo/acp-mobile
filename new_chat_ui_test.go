package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

const newChatCatalogFixture = `{"defaultAgent":"codex","agents":[
 {"id":"codex","name":"Codex","models":[{"id":"sol","name":"Sol"},{"id":"astra","name":"Astra"}],"modes":[{"id":"agent","name":"Agent","description":"Ask outside the project"},{"id":"full","name":"Full access","description":"No approval prompts"}],"efforts":[{"id":"high","name":"High"},{"id":"low","name":"Low"}],"defaults":{"model":"sol","mode":"agent","effort":""}},
 {"id":"claude","name":"Claude Code","models":[{"id":"sonnet","name":"Sonnet"}],"modes":[{"id":"manual","name":"Manual"}],"efforts":[],"defaults":{"model":"sonnet","mode":"manual","effort":""}},
 {"id":"opencode","name":"OpenCode","models":[{"id":"google/gemini-a","name":"google/gemini-a"},{"id":"google/gemini-b","name":"google/gemini-b"},{"id":"openrouter/anthropic/claude-x","name":"openrouter/anthropic/claude-x"},{"id":"openai/gpt-y","name":"openai/gpt-y"}],"modes":[{"id":"build","name":"build"}],"efforts":[],"defaults":{"model":"","mode":"build","effort":""}}
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

func TestNewChatSearchCannotBecomePathAndDraftSurvives(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3`)
	state := page.evalObject(t, `(()=>{
  spTask.value='Do not lose me';spTask.dispatchEvent(new Event('input'));
  const q=document.getElementById('sp-search');q.value='nonexistent';q.dispatchEvent(new Event('input'));
  const cwd=spDraft.cwd;
  document.getElementById('sp-close').click();openSpawnSheet();
  const task=spTask.value,view=spView;
  openSpawnView('preset');
  document.getElementById('sp-agent').click();
  [...document.querySelectorAll('#sp-picker-list button')].find(b=>b.textContent.includes('Claude Code')).click();
  return {cwd,task,view,model:spDraft.settings.model,mode:spDraft.settings.mode,disabled:spGo.disabled};
 })()`)
	if state["cwd"] != "/src/acp-mobile" || state["task"] != "Do not lose me" || state["view"] != "project" {
		t.Fatalf("draft/path: %#v", state)
	}
	if state["model"] != "" || state["mode"] != "" || state["disabled"] != true {
		t.Fatalf("incompatible settings must need a choice: %#v", state)
	}
}

func TestNewChatCenteredHeadersAndKeyboardHeight(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3`)
	for _, height := range []int{852, 430} {
		page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": 320, "height": height, "deviceScaleFactor": 1, "mobile": true})
		state := page.evalObject(t, `(()=>{const title=document.getElementById('sp-title').getBoundingClientRect(),sheet=spSheet.getBoundingClientRect(),foot=document.getElementById('sp-footer').getBoundingClientRect();openSpawnView('preset');const back=document.getElementById('sp-back');const out={center:Math.abs((title.left+title.right)/2-(sheet.left+sheet.right)/2),footer:foot.bottom<=innerHeight,overflow:document.documentElement.scrollWidth>innerWidth,backLeft:back.getBoundingClientRect().left<80,backText:back.textContent.trim(),icon:!!back.querySelector('img')};back.click();return out})()`)
		if state["center"].(float64) > 1 || state["footer"] != true || state["overflow"] != false || state["backLeft"] != true || state["backText"] != "" || state["icon"] != true {
			t.Fatalf("height %d: %#v", height, state)
		}
	}
}

func TestNewChatCustomPresetPinsAndPartialFailure(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	state := page.evalObject(t, `(async()=>{
  openSpawnView('preset');document.querySelector('#sp-preset-list .sp-row').click();
  openSpawnView('preset');spEl('customize').click();
  spEl('save').click();spEl('save-name').value='My setup';spEl('picker-action').click();
  const custom=spCustom.find(p=>p.label==='My setup');
  openSpawnView('project');document.querySelector('#sp-picker-list button[aria-label="Pin dotfiles"]').click();
  const pinned=spPins.includes('/src/dotfiles');openSpawnView('prompt');
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
	if os.Getenv("SYZYGY_UI_SHOTS") == "" {
		t.Skip("Set SYZYGY_UI_SHOTS to capture mobile review images")
	}
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	page.eval(t, `spCombos={pins:[{cwd:'/src/acp-mobile',preset:'s',settings:{agent:'codex',model:'sol',mode:'full',effort:'high'}}],recent:[{cwd:'/src/dotfiles',preset:'',settings:{agent:'claude',model:'sonnet',mode:'manual',effort:''}}]};spDraft.cwd='/src/acp-mobile';spDraft.settings={agent:'codex',model:'sol',mode:'full',effort:'high'}`)
	for _, view := range []string{"home", "project", "preset", "prompt"} {
		page.eval(t, fmt.Sprintf(`openSpawnView(%q)`, view))
		saveUIShot(t, page, "new-chat-"+view)
	}
}

func TestNewChatMissingRecoveryCanReturnToDraft(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(async()=>{
  openSpawnView('preset');document.querySelector('#sp-preset-list .sp-row').click();spTask.value='Unsent work';spTask.dispatchEvent(new Event('input'));
  spRecoveryBuffer='missing buffer';spRecoveryKeepsTask=true;persistSpawnDraft();renderSpawnStep();
  const locked=spEl('summary').disabled;
  spEl('back').click();const stayed=spView;
  spEl('release').click();
  return {locked,stayed,editable:!spEl('summary').disabled,task:spDraft.task,pending:spRecoveryBuffer,canLaunch:!spGo.disabled,stored:JSON.parse(localStorage.getItem('syzygy.launch.draft')).pendingBuffer};
 })()`)
	if state["stayed"] != "prompt" || state["locked"] != true || state["editable"] != true || state["task"] != "Unsent work" || state["pending"] != "" || state["stored"] != "" || state["canLaunch"] != true {
		t.Fatalf("recovery must be dismissible without losing the draft: %#v", state)
	}
}

func TestNewChatModelPickerGroupsByProvider(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3`)
	state := page.evalObject(t, `(()=>{
  const rows=()=>[...document.querySelectorAll('#sp-picker-list .sp-row')].map(b=>b.textContent);
  const heads=()=>[...document.querySelectorAll('#sp-picker-list .sp-group')].map(b=>b.textContent+'|'+b.getAttribute('aria-expanded'));
  openSpawnView('preset');
  document.getElementById('sp-agent').click();
  [...document.querySelectorAll('#sp-picker-list button')].find(b=>b.textContent.includes('OpenCode')).click();
  document.getElementById('sp-model').click();
  const collapsed={heads:heads(),rows:rows()};
  document.querySelector('#sp-picker-list .sp-group').click();
  const opened={heads:heads(),rows:rows()};
  const q=document.getElementById('sp-search');q.value='claude-x';q.dispatchEvent(new Event('input'));
  const searched={heads:heads(),rows:rows()};
  q.value='';q.dispatchEvent(new Event('input'));
  const cleared={heads:heads(),rows:rows()};
  document.querySelector('#sp-picker-list .sp-row').click();
  const model=spDraft.settings.model;
  document.getElementById('sp-model').click();
  const reopened={heads:heads(),rows:rows()};
  document.getElementById('sp-agent').click();
  [...document.querySelectorAll('#sp-picker-list button')].find(b=>b.textContent.includes('Codex')).click();
  document.getElementById('sp-model').click();
  const flat={heads:heads(),rows:rows()};
  return {collapsed,opened,searched,cleared,model,reopened,flat};
 })()`)
	collapsed := state["collapsed"].(map[string]interface{})
	if heads := collapsed["heads"].([]interface{}); len(heads) != 3 || len(collapsed["rows"].([]interface{})) != 0 || !strings.HasPrefix(heads[0].(string), "google") || !strings.HasSuffix(heads[0].(string), "|false") {
		t.Fatalf("no selection: every provider collapsed: %#v", collapsed)
	}
	opened := state["opened"].(map[string]interface{})
	if rows := opened["rows"].([]interface{}); len(rows) != 2 || !strings.Contains(rows[0].(string), "google/gemini-a") || !strings.HasSuffix(opened["heads"].([]interface{})[0].(string), "|true") {
		t.Fatalf("tapping a header opens only that group: %#v", opened)
	}
	searched := state["searched"].(map[string]interface{})
	if heads := searched["heads"].([]interface{}); len(heads) != 1 || !strings.HasPrefix(heads[0].(string), "openrouter/anthropic") || len(searched["rows"].([]interface{})) != 1 {
		t.Fatalf("search keeps only matching groups, open: %#v", searched)
	}
	cleared := state["cleared"].(map[string]interface{})
	if len(cleared["heads"].([]interface{})) != 3 || len(cleared["rows"].([]interface{})) != 2 {
		t.Fatalf("clearing search restores the groups and the one the user opened: %#v", cleared)
	}
	if state["model"] != "google/gemini-a" {
		t.Fatalf("picked model: %#v", state["model"])
	}
	reopened := state["reopened"].(map[string]interface{})
	if rows := reopened["rows"].([]interface{}); len(rows) != 2 || len(reopened["heads"].([]interface{})) != 3 {
		t.Fatalf("reopening starts with the selected model's group open: %#v", reopened)
	}
	flat := state["flat"].(map[string]interface{})
	if len(flat["heads"].([]interface{})) != 0 || len(flat["rows"].([]interface{})) != 2 {
		t.Fatalf("single-provider lists stay flat: %#v", flat)
	}
}

func TestNewChatStepperWalksProjectAgentPrompt(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	state := page.evalObject(t, `(()=>{
  const out={};
  out.first={view:spView,title:spEl('title-text').textContent,step:spEl('step').textContent,close:!spEl('close').hidden,back:!spEl('back').hidden};
  [...document.querySelectorAll('#sp-picker-list .sp-row')].find(b=>b.textContent.includes('dotfiles')).click();
  out.agent={view:spView,title:spEl('title-text').textContent,step:spEl('step').textContent,presets:[...document.querySelectorAll('#sp-preset-list .sp-row')].map(b=>b.textContent)};
  document.querySelector('#sp-preset-list .sp-row').click();
  out.prompt={view:spView,step:spEl('step').textContent,summary:spEl('summary').textContent,go:spGo.disabled,goText:spGo.textContent,cwd:spDraft.cwd,settings:{...spDraft.settings}};
  spEl('back').click(); out.back1=spView;
  spEl('back').click(); out.back2=spView;
  return out;
 })()`)
	first := state["first"].(map[string]interface{})
	if first["view"] != "project" || first["title"] != "Project" || first["step"] != "1 / 3" || first["close"] != true || first["back"] != false {
		t.Fatalf("empty Home opens on Project with close: %#v", first)
	}
	agent := state["agent"].(map[string]interface{})
	presets := agent["presets"].([]interface{})
	if agent["view"] != "preset" || agent["title"] != "Agent" || agent["step"] != "2 / 3" || len(presets) != 1 || !strings.Contains(presets[0].(string), "Sol · Full") || !strings.Contains(presets[0].(string), "Codex / Sol / full / high") {
		t.Fatalf("agent step: %#v", agent)
	}
	prompt := state["prompt"].(map[string]interface{})
	if prompt["view"] != "prompt" || prompt["step"] != "3 / 3" || !strings.Contains(prompt["summary"].(string), "Sol · Full") || !strings.Contains(prompt["summary"].(string), "dotfiles") || prompt["go"] != false || prompt["goText"] != "Start" || prompt["cwd"] != "/src/dotfiles" {
		t.Fatalf("prompt step: %#v", prompt)
	}
	if state["back1"] != "preset" || state["back2"] != "project" {
		t.Fatalf("back path: %#v", state)
	}
}

func TestNewChatCustomizeGatesNextAndLaunchesExactSettings(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(async()=>{
  const pick=text=>[...document.querySelectorAll('#sp-picker-list button')].find(b=>b.textContent.includes(text)).click();
  openSpawnView('preset');
  document.querySelector('#sp-preset-list .sp-row').click();
  spTask.value='Keep this draft';spTask.dispatchEvent(new Event('input'));
  openSpawnView('preset');
  const matched={expanded:spEl('customize').getAttribute('aria-expanded'),sel:document.querySelectorAll('#sp-preset-list .sp-row.sel').length};
  spEl('customize').click();
  spEl('model').click();pick('Astra');
  const changed={view:spView,expanded:spEl('customize').getAttribute('aria-expanded'),sel:document.querySelectorAll('#sp-preset-list .sp-row.sel').length,next:spEl('next').disabled,save:!spEl('save').hidden};
  spEl('agent').click();pick('Claude Code');
  const incomplete={next:spEl('next').disabled,save:!spEl('save').hidden};
  spEl('agent').click();pick('Codex');spEl('model').click();pick('Astra');spEl('mode').click();pick('Full access');
  spEl('next').click();
  const prompt={view:spView,summary:spEl('summary').textContent,task:spTask.value};
  window.__sent=null;const orig=fetch;window.fetch=async(url,opts)=>{if(url.endsWith('/api/spawn'))window.__sent=JSON.parse(opts.body);return orig(url,opts)};
  SPAWN_POLL_MS=5;window.__selected=null;selectSession=s=>{window.__selected=s.sessionId};
  spGo.click();
  for(let i=0;i<100&&!window.__selected;i++)await new Promise(r=>setTimeout(r,20));
  return {matched,changed,incomplete,prompt,sent:window.__sent,selected:window.__selected};
 })()`)
	matched := state["matched"].(map[string]interface{})
	if matched["expanded"] != "false" || matched["sel"] != float64(1) {
		t.Fatalf("a matching preset is highlighted and Customize starts closed: %#v", matched)
	}
	changed := state["changed"].(map[string]interface{})
	if changed["view"] != "preset" || changed["expanded"] != "true" || changed["sel"] != float64(0) || changed["next"] != false || changed["save"] != true {
		t.Fatalf("customizing clears the highlight and keeps the section open: %#v", changed)
	}
	incomplete := state["incomplete"].(map[string]interface{})
	if incomplete["next"] != true || incomplete["save"] != false {
		t.Fatalf("Next and Save gate on complete settings: %#v", incomplete)
	}
	prompt := state["prompt"].(map[string]interface{})
	if prompt["view"] != "prompt" || !strings.Contains(prompt["summary"].(string), "Codex / Custom") || prompt["task"] != "Keep this draft" {
		t.Fatalf("custom summary: %#v", prompt)
	}
	sent := state["sent"].(map[string]interface{})["settings"].(map[string]interface{})
	if sent["agent"] != "codex" || sent["model"] != "astra" || sent["mode"] != "full" || sent["effort"] != "" || state["selected"] != "intended" {
		t.Fatalf("launch payload: %#v", state)
	}
}

// launchDotfilesSol walks the stepper to a Sol · Full launch in dotfiles.
const launchDotfilesSol = `(async()=>{
  [...document.querySelectorAll('#sp-picker-list .sp-row')].find(b=>b.textContent.includes('dotfiles')).click();
  document.querySelector('#sp-preset-list .sp-row').click();
  SPAWN_POLL_MS=5;window.__selected=null;selectSession=s=>{window.__selected=s.sessionId};
  spGo.click();
  for(let i=0;i<100&&!window.__selected;i++)await new Promise(r=>setTimeout(r,20));
  return {selected:window.__selected};
 })()`

func TestNewChatLaunchRemembersComboOnHome(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	if s := page.evalObject(t, launchDotfilesSol); s["selected"] != "intended" {
		t.Fatalf("launch: %#v", s)
	}
	state := page.evalObject(t, `(()=>{
  openSpawnSheet();
  const rows=[...document.querySelectorAll('#sp-home-list .sp-combo')];
  const r=rows[0];
  const home={view:spView,title:spEl('title-text').textContent,close:!spEl('close').hidden,next:spEl('next').textContent,heads:[...document.querySelectorAll('#sp-home-list .sp-label')].map(h=>h.textContent),count:rows.length,
    text:r.querySelector('.sp-row-title').textContent,mode:r.querySelector('.sp-mode-word').textContent,detail:r.querySelector('.sp-row-detail').textContent,icon:!!r.querySelector('.provider-icon.openai')};
  r.click();
  return {home,prompt:{view:spView,cwd:spDraft.cwd,settings:{...spDraft.settings},go:spGo.disabled,summary:spEl('summary').textContent}};
 })()`)
	home := state["home"].(map[string]interface{})
	if home["view"] != "home" || home["title"] != "New chat" || home["close"] != true || home["next"] != "Start from scratch" || home["count"] != float64(1) {
		t.Fatalf("home: %#v", home)
	}
	if heads := home["heads"].([]interface{}); len(heads) != 1 || heads[0] != "Recent" {
		t.Fatalf("only Recent has entries: %#v", home)
	}
	if home["text"] != "Sol in dotfiles" || home["mode"] != "full" || !strings.Contains(home["detail"].(string), "/src/dotfiles") || home["icon"] != true {
		t.Fatalf("row: %#v", home)
	}
	prompt := state["prompt"].(map[string]interface{})
	settings := prompt["settings"].(map[string]interface{})
	if prompt["view"] != "prompt" || prompt["cwd"] != "/src/dotfiles" || settings["model"] != "sol" || settings["mode"] != "full" || prompt["go"] != false || !strings.Contains(prompt["summary"].(string), "Sol · Full") {
		t.Fatalf("tapping a combo lands on Prompt ready to start: %#v", prompt)
	}
}

func TestNewChatPinnedComboSurvivesReload(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	page.evalObject(t, launchDotfilesSol)
	state := page.evalObject(t, `(()=>{
  openSpawnSheet();
  document.querySelector('#sp-home-list button[aria-label="Pin Sol in dotfiles"]').click();
  return {view:spView,heads:[...document.querySelectorAll('#sp-home-list .sp-label')].map(h=>h.textContent),pressed:document.querySelector('#sp-home-list button[aria-label="Unpin Sol in dotfiles"]')?.getAttribute('aria-pressed')};
 })()`)
	if state["view"] != "home" || state["pressed"] != "true" {
		t.Fatalf("pin must not launch or leave Home: %#v", state)
	}
	if heads := state["heads"].([]interface{}); len(heads) != 1 || heads[0] != "Pinned" {
		t.Fatalf("pinned combo leaves Recent: %#v", state)
	}
	page.call(t, "Page.reload", map[string]interface{}{})
	page.waitFor(t, `typeof openSpawnSheet === 'function'`)
	page.eval(t, `openSpawnSheet()`)
	page.waitFor(t, `spawnProjects.length === 2`)
	state = page.evalObject(t, `(()=>({view:spView,pinned:spCombos.pins.length,text:document.querySelector('#sp-home-list .sp-combo .sp-row-title')?.textContent}))()`)
	if state["view"] != "home" || state["pinned"] != float64(1) || state["text"] != "Sol in dotfiles" {
		t.Fatalf("pin survives reload: %#v", state)
	}
}

func TestNewChatStaleComboLandsOnPromptDisabled(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(()=>{
  spCombos.recent=[{cwd:'/src/dotfiles',preset:'s',settings:{agent:'codex',model:'retired',mode:'full',effort:'high'}},{cwd:'/src/atlas',preset:'',settings:{agent:'opencode',model:'google/gemini-a',mode:'build',effort:''}}];
  openSpawnView('home');
  const iconSlot=[...document.querySelectorAll('#sp-home-list .sp-combo')].map(r=>r.querySelector('.provider-icon')?.getBoundingClientRect().width || 0);
  document.querySelector('#sp-home-list .sp-combo').click();
  return {iconSlot,view:spView,go:spGo.disabled,summary:spEl('summary').textContent};
 })()`)
	if slots := state["iconSlot"].([]interface{}); len(slots) != 2 || slots[0] != float64(18) || slots[1] != float64(18) {
		t.Fatalf("every row keeps an 18px icon slot: %#v", state)
	}
	if state["view"] != "prompt" || state["go"] != true || !strings.Contains(state["summary"].(string), "Codex / Custom") {
		t.Fatalf("stale combo: %#v", state)
	}
}

func TestNewChatEntryPointsAndResume(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	state := page.evalObject(t, `(()=>{
  hideSpawnSheet();spawnInProject('/src/dotfiles');
  const perProject={view:spView,cwd:spDraft.cwd};
  document.querySelector('#sp-preset-list .sp-row').click();
  hideSpawnSheet();openSpawnSheet();
  return {perProject,resumed:spView};
 })()`)
	pp := state["perProject"].(map[string]interface{})
	if pp["view"] != "preset" || pp["cwd"] != "/src/dotfiles" || state["resumed"] != "prompt" {
		t.Fatalf("entry/resume: %#v", state)
	}
}

func TestNewChatResumeFallsBackWhenSettingsGoStale(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	page.eval(t, `openSpawnView('preset');document.querySelector('#sp-preset-list .sp-row').click();spDraft.settings.model='retired';persistSpawnDraft();hideSpawnSheet()`)
	page.call(t, "Page.reload", map[string]interface{}{})
	page.waitFor(t, `typeof openSpawnSheet === 'function'`)
	page.eval(t, `openSpawnSheet()`)
	page.waitFor(t, `spCatalog.agents.length === 3`)
	if view := page.eval(t, `spView`); view != "preset" {
		t.Fatalf("a stale resumed Prompt must fall back to Agent once the catalog loads, got %v", view)
	}
}
