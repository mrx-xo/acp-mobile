package main

import (
	"encoding/json"
	"fmt"
	"io"
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

func TestCloneSettingsFailureRetainsExactChatForRetry(t *testing.T) {
	page := newChatTestPage(t)
	state := page.evalObject(t, `(async()=>{
  currentBufferName='source';let posts=0;const targets=[],alerts=[];
  window.alert=msg=>alerts.push(msg);
  const original=fetch;window.fetch=async(url,opts)=>{
    if(url.endsWith('/api/spawn')){posts++;return {ok:false,json:async()=>({error:'Model refused',bufferName:'partial clone'})};}
    return original(url,opts);
  };
  waitForSpawn=async name=>{targets.push(name);return targets.length>1;};
  await cloneSession();await cloneSession();
  return {posts,targets,alerts,draft:spDraft.task};
 })()`)
	if state["posts"] != float64(1) || fmt.Sprint(state["targets"]) != "[partial clone partial clone]" || !strings.Contains(fmt.Sprint(state["alerts"]), "Model refused") {
		t.Fatalf("clone recovery must reopen its existing buffer without spawning again: %#v", state)
	}
}

func TestNewChatStartWaitsForPhotoDecoding(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(async()=>{
  applySpawnPreset('s');openSpawnView('prompt');
  const original=encodeAttachment;let release;
  encodeAttachment=async f=>{await new Promise(r=>release=r);return original(f);};
  const canvas=document.createElement('canvas');canvas.width=canvas.height=4;
  const blob=await new Promise(r=>canvas.toBlob(r,'image/png'));
  const dt=new DataTransfer();dt.items.add(new File([blob],'photo.png',{type:'image/png'}));
  spEl('attach-input').files=dt.files;spEl('attach-input').dispatchEvent(new Event('change'));
  const blocked=spGo.disabled;release();
  for(let i=0;i<100&&!spImages.length;i++)await new Promise(r=>setTimeout(r,10));
  return {blocked,ready:!spGo.disabled,images:spImages.length};
 })()`)
	if state["blocked"] != true || state["ready"] != true || state["images"] != float64(1) {
		t.Fatalf("Start must wait for photo decoding: %#v", state)
	}
}

func TestNewChatPhotoDeliveryTimeoutKeepsRecoverableDraft(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(async()=>{
  applySpawnPreset('s');openSpawnView('prompt');
  spDraft.task='Keep my photo';spImages=[{data:'photo',mimeType:'image/jpeg',thumb:''}];
  SPAWN_POLL_MS=1;selectSession=s=>{currentBufferName=s.bufferName;sessionId=null;};
  await spStart();
  currentBufferName='another chat';
  const now=Date.now;Date.now=()=>now()+61000;
  await new Promise(r=>setTimeout(r,300));Date.now=now;
  const stored=JSON.parse(localStorage.getItem('syzygy.launch.draft'));
  openSpawnSheet();
  return {task:spDraft.task,images:spImages.length,pending:spRecoveryBuffer,
    storedTask:stored.task,view:spView,canChangeSettings:!spEl('summary').disabled};
 })()`)
	if state["task"] != "Keep my photo" || state["storedTask"] != "Keep my photo" || state["images"] != float64(1) || state["pending"] != "intended chat" || state["view"] != "prompt" || state["canChangeSettings"] != false {
		t.Fatalf("a delayed photo launch must remain recoverable after leaving the chat: %#v", state)
	}
}

func TestNewChatPhotoRecoveryDoesNotSendAfterSettingsFailure(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(async()=>{
  applySpawnPreset('s');openSpawnView('prompt');spDraft.task='Do not send';
  spImages=[{data:'photo',mimeType:'image/jpeg',thumb:''}];
  const original=fetch;window.fetch=async(url,opts)=>{
    if(url.endsWith('/api/spawn'))return {ok:false,json:async()=>({error:'Permissions rejected',bufferName:'intended chat'})};
    if(url.endsWith('/api/sessions'))return {ok:true,json:async()=>({sessions:[{pid:2,sessionId:'intended',bufferName:'intended chat'}]})};
    return original(url,opts);
  };
  SPAWN_POLL_MS=1;const sent=[];
  selectSession=s=>{currentBufferName=s.bufferName;sessionId=s.sessionId;replayMode=false;processing=false;ws={readyState:1,send:raw=>sent.push(raw)};};
  await spStart();await spStart();
  return {sent:sent.length,task:spDraft.task,images:spImages.length};
 })()`)
	if state["sent"] != float64(0) || state["task"] != "Do not send" || state["images"] != float64(1) {
		t.Fatalf("opening a partially configured chat must not submit the photo prompt: %#v", state)
	}
}

func TestNewChatPhotoReloadKeepsTextForReview(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	page.eval(t, `(async()=>{
  applySpawnPreset('s');openSpawnView('prompt');spDraft.task='Text survives reload';
  spImages=[{data:'photo',mimeType:'image/jpeg',thumb:''}];
  SPAWN_POLL_MS=1;selectSession=s=>{currentBufferName=s.bufferName;sessionId=null;};
  await spStart();return true;
 })()`)
	page.call(t, "Page.reload", map[string]interface{}{})
	page.waitFor(t, `typeof openSpawnSheet === 'function'`)
	page.eval(t, `openSpawnSheet()`)
	page.waitFor(t, `spCatalog.agents.length === 3`)
	state := page.evalObject(t, `(async()=>{
  const before={task:spDraft.task,pending:spRecoveryBuffer,images:spImages.length};
  const sent=[];SPAWN_POLL_MS=1;
  selectSession=s=>{currentBufferName=s.bufferName;sessionId=s.sessionId;replayMode=false;processing=false;ws={readyState:1,send:raw=>sent.push(raw)};};
  await spStart();
  return {before,task:spDraft.task,sent:sent.length,stored:JSON.parse(localStorage.getItem('syzygy.launch.draft')).task};
 })()`)
	before := state["before"].(map[string]interface{})
	if before["pending"] != "intended chat" || before["task"] != "Text survives reload" || before["images"] != float64(0) || state["sent"] != float64(0) || state["task"] != "Text survives reload" || state["stored"] != "Text survives reload" {
		t.Fatalf("reload must retain text for review without sending an incomplete message: %#v", state)
	}
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
	page.eval(t, `(()=>{const c=document.createElement('canvas');c.width=c.height=64;const x=c.getContext('2d');x.fillStyle='#d79921';x.fillRect(0,0,64,64);const u=c.toDataURL('image/jpeg');spImages=[{data:'',mimeType:'image/jpeg',thumb:u},{data:'',mimeType:'image/jpeg',thumb:u}];openSpawnView('prompt');})()`)
	saveUIShot(t, page, "new-chat-prompt-photos")
}

func TestNewChatMissingRecoveryCanReturnToDraft(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(async()=>{
  openSpawnView('preset');document.querySelector('#sp-preset-list .sp-row').click();spTask.value='Unsent work';spTask.dispatchEvent(new Event('input'));
  spRecoveryBuffer='missing buffer';spRecoveryKeepsTask=true;persistSpawnDraft();renderSpawnStep();
  const locked=spEl('summary').disabled;
  const nav={close:!spEl('close').hidden,back:!spEl('back').hidden};
  spEl('back').click();const stayed=spView;
  spEl('release').click();
  const after={close:!spEl('close').hidden,back:!spEl('back').hidden};
  return {nav,after,locked,stayed,editable:!spEl('summary').disabled,task:spDraft.task,pending:spRecoveryBuffer,canLaunch:!spGo.disabled,stored:JSON.parse(localStorage.getItem('syzygy.launch.draft')).pendingBuffer};
 })()`)
	if nav, after := state["nav"].(map[string]interface{}), state["after"].(map[string]interface{}); nav["close"] != true || nav["back"] != false || after["close"] != false || after["back"] != true {
		t.Fatalf("recovery shows close, not a dead back button; releasing restores back: %#v", state)
	}
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
  return {iconSlot,view:spView,go:spGo.disabled,summary:spEl('summary').textContent,reason:spEl('blocked-reason')?.textContent};
 })()`)
	if slots := state["iconSlot"].([]interface{}); len(slots) != 2 || slots[0] != float64(18) || slots[1] != float64(18) {
		t.Fatalf("every row keeps an 18px icon slot: %#v", state)
	}
	if state["view"] != "prompt" || state["go"] != true || !strings.Contains(state["summary"].(string), "Codex / Custom") {
		t.Fatalf("stale combo: %#v", state)
	}
	if reason, _ := state["reason"].(string); !strings.Contains(reason, "Model") || !strings.Contains(reason, "summary") {
		t.Fatalf("disabled Start must explain which setting to repair: %#v", state)
	}
	page.eval(t, `spDraft.settings.model='sol';renderSpawnStep()`)
	if hidden := page.eval(t, `spEl('blocked-reason').hidden && !spGo.disabled`); hidden != true {
		t.Fatal("correcting the model must clear the explanation and enable Start")
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

func TestNewChatLoadOrderEdges(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	state := page.evalObject(t, `(async()=>{
  const sol={agent:'codex',model:'sol',mode:'full',effort:'high'};
  // Agent opened before presets arrive: Customize follows the late match.
  spawnPresets=[];spDraft.settings={...sol};openSpawnView('preset');
  const early=spEl('customize').getAttribute('aria-expanded');
  await loadSpawnPresets();
  const late=spEl('customize').getAttribute('aria-expanded');
  // Once toggled by hand, a late preset load leaves Customize alone.
  spawnPresets=[];openSpawnView('prompt');openSpawnView('preset');spEl('customize').click();spEl('customize').click();
  await loadSpawnPresets();
  const touched=spEl('customize').getAttribute('aria-expanded');
  // Prompt before the catalog: no raw agent id.
  const cat=spCatalog;spCatalog={defaultAgent:'',agents:[]};spawnPresets=[];openSpawnView('prompt');
  const loading=spEl('summary').textContent;spCatalog=cat;
  // Typing on a resumed stale Prompt: stay put, explain.
  await loadSpawnPresets();spDraft.settings={...sol,model:'retired'};openSpawnView('prompt');spTask.focus();spResumeCheck=true;
  await loadSpawnCatalog();
  const typing={view:spView,focus:document.activeElement===spTask,msg:spEl('message').textContent};
  return {early,late,touched,loading,typing};
 })()`)
	if state["early"] != "true" || state["late"] != "false" || state["touched"] != "true" {
		t.Fatalf("Customize follows a late preset match until toggled: %#v", state)
	}
	if l := state["loading"].(string); strings.Contains(l, "codex") || !strings.Contains(l, "Loading") {
		t.Fatalf("summary before catalog: %#v", state)
	}
	typing := state["typing"].(map[string]interface{})
	if typing["view"] != "prompt" || typing["focus"] != true || !strings.Contains(typing["msg"].(string), "no longer available") {
		t.Fatalf("typing on a stale resume: %#v", typing)
	}
}

func TestNewChatPinKeepsFocusAndEmptyProjectResume(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1 && spawnProjects.length === 2`)
	state := page.evalObject(t, `(()=>{
  spCombos.recent=[{cwd:'/src/dotfiles',preset:'s',settings:{agent:'codex',model:'sol',mode:'full',effort:'high'}}];
  openSpawnView('home');
  {const b=document.querySelector('#sp-home-list button[aria-label="Pin Sol in dotfiles"]');b.focus();b.click();}
  const homeFocus=document.activeElement.getAttribute('aria-label');
  openSpawnView('project');
  {const b=document.querySelector('#sp-picker-list button[aria-label="Pin dotfiles"]');b.focus();b.click();}
  const projectFocus=document.activeElement.getAttribute('aria-label');
  openSpawnView('prompt');spDraft.cwd='';spRecent=['/src/acp-mobile'];persistSpawnDraft();
  hideSpawnSheet();openSpawnSheet();
  return {homeFocus,projectFocus,resumed:spView,cwd:spDraft.cwd};
 })()`)
	if state["homeFocus"] != "Unpin Sol in dotfiles" || state["projectFocus"] != "Unpin dotfiles" {
		t.Fatalf("pin toggles keep focus: %#v", state)
	}
	if state["resumed"] != "project" || state["cwd"] != "/src/acp-mobile" {
		t.Fatalf("a Prompt draft with no project resumes on Project, prefilled: %#v", state)
	}
}

func TestNewChatComboMatchingPresetButMissingFromCatalog(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(()=>{
  const ghost={agent:'codex',model:'ghost',mode:'full',effort:'high'};
  spawnPresets=[...spawnPresets,{key:'g',label:'Ghost · Full',...ghost}];
  spCombos.recent=[{cwd:'/src/dotfiles',preset:'g',settings:ghost}];
  openSpawnView('home');document.querySelector('#sp-home-list .sp-combo').click();
  return {view:spView,go:spGo.disabled,summary:spEl('summary').textContent};
 })()`)
	if state["view"] != "prompt" || state["go"] != true || !strings.Contains(state["summary"].(string), "Codex / Custom") || strings.Contains(state["summary"].(string), "Ghost") {
		t.Fatalf("a preset whose ids left the catalog reads Custom: %#v", state)
	}
}

// Photos cannot ride the rig's text-only spawn bridge: the launch goes out
// without the task, and text plus photos arrive as the new chat's first
// prompt once it is connected. A touch on Start launches and drops the
// keyboard in the same tap.
func TestNewChatPhotosSendAsFirstPromptAndTouchStartLaunches(t *testing.T) {
	page := newChatTestPage(t)
	page.waitFor(t, `spCatalog.agents.length === 3 && spawnPresets.length === 1`)
	state := page.evalObject(t, `(async()=>{
  openSpawnView('preset');document.querySelector('#sp-preset-list .sp-row').click();
  spTask.value='Look at this';spTask.dispatchEvent(new Event('input'));
  const canvas=document.createElement('canvas');canvas.width=4;canvas.height=4;
  const blob=await new Promise(r=>canvas.toBlob(r,'image/png'));
  const dt=new DataTransfer();dt.items.add(new File([blob],'shot.png',{type:'image/png'}));
  const input=spEl('attach-input');input.files=dt.files;input.dispatchEvent(new Event('change'));
  for(let i=0;i<100&&!spImages.length;i++)await new Promise(r=>setTimeout(r,10));
  const attached={chips:document.querySelectorAll('#sp-attach-row .attach-chip').length,row:!spEl('attach-row').hidden,label:spEl('attach').textContent};
  pendingImages=[{data:'other-chat-photo',mimeType:'image/png',thumb:''}];
  window.__spawn=null;window.__spawnCount=0;const orig=fetch;window.fetch=async(url,opts)=>{if(url.endsWith('/api/spawn')){window.__spawn=JSON.parse(opts.body);window.__spawnCount++;}return orig(url,opts)};
  window.__ws=[];
  SPAWN_POLL_MS=5;selectSession=s=>{currentBufferName=s.bufferName;replayMode=true;
    setTimeout(()=>{sessionId='intended';replayMode=false;ws={readyState:WebSocket.OPEN,send:m=>window.__ws.push(JSON.parse(m))};},50);};
  spTask.focus();
  const focused=document.activeElement===spTask;
  const down=new PointerEvent('pointerdown',{pointerType:'touch',bubbles:true,cancelable:true});
  spGo.dispatchEvent(down);
  const keyboardDown=document.activeElement!==spTask;
  spGo.click(); // the synthetic click after a touch must not launch twice
  for(let i=0;i<200&&!window.__ws.length;i++)await new Promise(r=>setTimeout(r,10));
  const prompt=window.__ws.find(m=>m.method==='session/prompt');
  return {attached,focused,keyboardDown,prevented:down.defaultPrevented,spawn:window.__spawn,spawnCount:window.__spawnCount,
    blocks:prompt ? prompt.params.prompt.map(b=>b.type+':'+(b.text||b.mimeType)) : null,
    unrelatedPhotos:pendingImages.map(i=>i.data),
    left:{images:spImages.length,task:spDraft.task,pending:spFirstPrompt,open:spSheet.classList.contains('visible')}};
 })()`)
	attached := state["attached"].(map[string]interface{})
	if attached["chips"] != float64(1) || attached["row"] != true || !strings.Contains(attached["label"].(string), "1 attached") {
		t.Fatalf("photo shows in the Prompt step: %#v", attached)
	}
	if state["focused"] != true || state["keyboardDown"] != true || state["prevented"] != true {
		t.Fatalf("touch Start drops the keyboard itself: %#v", state)
	}
	spawn := state["spawn"].(map[string]interface{})
	if spawn["task"] != "" || spawn["cwd"] != "/src/acp-mobile" {
		t.Fatalf("spawn carries no task when photos go separately: %#v", spawn)
	}
	blocks := fmt.Sprint(state["blocks"])
	if blocks != "[image:image/jpeg text:Look at this]" {
		t.Fatalf("first prompt blocks: %s", blocks)
	}
	if fmt.Sprint(state["unrelatedPhotos"]) != "[other-chat-photo]" || state["spawnCount"] != float64(1) {
		t.Fatalf("launch must happen once and must not consume another composer's photos: %#v", state)
	}
	left := state["left"].(map[string]interface{})
	if left["images"] != float64(0) || left["task"] != "" || left["pending"] != nil || left["open"] != false {
		t.Fatalf("draft cleared after delivery: %#v", left)
	}
}

// fleetChat is a hub (mrx) with one peer (vengeance) behind the New chat sheet.
// log records every API request as path?query, in order; spawns the /api/spawn
// bodies; wsQueries the query of each /ws dial.
type fleetChat struct {
	mu        sync.Mutex
	log       []string
	spawns    []string
	wsQueries []string
	spawned   bool
	hold      chan struct{} // when set, the peer's options and presets wait for it to close
}

func (f *fleetChat) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (f *fleetChat) has(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.log {
		if l == want {
			return true
		}
	}
	return false
}

func (f *fleetChat) spawnBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.spawns...)
}

const fleetPeerCatalog = `{"defaultAgent":"gemini","agents":[{"id":"gemini","name":"Gemini","models":[{"id":"pro","name":"Pro"}],"modes":[{"id":"auto","name":"Auto"}],"efforts":[],"defaults":{"model":"pro","mode":"auto","effort":""}}]}`

// newFleetChatPage serves the hub's catalog, presets and projects and the
// peer's, selected by ?machine=. peerState is the peer's registry state;
// failSpawn makes a spawn naming the peer answer 502.
func newFleetChatPage(t *testing.T, peerState string, failSpawn bool, seed string) (*chromePage, *fleetChat) {
	t.Helper()
	f := &fleetChat{}
	record := func(r *http.Request) string {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		f.mu.Lock()
		f.log = append(f.log, key)
		f.mu.Unlock()
		m := r.URL.Query().Get("machine")
		f.mu.Lock()
		hold := f.hold
		f.mu.Unlock()
		if m == "vengeance" && hold != nil {
			<-hold
		}
		return m
	}
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
	mux.HandleFunc("/api/launch-options", func(w http.ResponseWriter, r *http.Request) {
		if record(r) == "vengeance" {
			fmt.Fprint(w, fleetPeerCatalog)
			return
		}
		fmt.Fprint(w, newChatCatalogFixture)
	})
	mux.HandleFunc("/api/presets", func(w http.ResponseWriter, r *http.Request) {
		if record(r) == "vengeance" {
			fmt.Fprint(w, `{"presets":[{"key":"g","label":"Gemini Pro","agent":"gemini","model":"pro","mode":"auto"}]}`)
			return
		}
		fmt.Fprint(w, `{"presets":[{"key":"s","label":"Sol · Full","agent":"codex","model":"sol","mode":"full","effort":"high"}]}`)
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, `{"projects":[{"name":"acp-mobile","path":"/src/acp-mobile","machine":"mrx"},{"name":"dotfiles","path":"/src/dotfiles","machine":"mrx"},{"name":"dotfiles","path":"/src/dotfiles","machine":"vengeance"}]}`)
	})
	mux.HandleFunc("/api/spawn", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.spawns = append(f.spawns, string(body))
		f.spawned = true
		f.mu.Unlock()
		if failSpawn && strings.Contains(string(body), `"machine":"vengeance"`) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":"NEMESIS did not answer"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"bufferName":"new chat"}`)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		spawned := f.spawned
		f.mu.Unlock()
		sessions := `[]`
		if spawned {
			sessions = `[{"pid":1,"sessionId":"hub","bufferName":"new chat","machine":"mrx"},{"pid":2,"sessionId":"peer","bufferName":"new chat","machine":"vengeance"}]`
		}
		fmt.Fprintf(w, `{"sessions":%s,"machine":"mrx","machines":[{"id":"mrx","name":"POLLUX","state":"awake"},{"id":"vengeance","name":"NEMESIS","state":%q}]}`, sessions, peerState)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.wsQueries = append(f.wsQueries, r.URL.RawQuery)
		f.mu.Unlock()
		http.Error(w, "test", http.StatusBadRequest)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	page := openChromePage(t, server.URL)
	page.call(t, "Emulation.setDeviceMetricsOverride", map[string]interface{}{"width": 393, "height": 852, "deviceScaleFactor": 1, "mobile": true})
	page.waitFor(t, `typeof openSpawnSheet === 'function' && HUB_MACHINE === 'mrx'`)
	if seed != "" {
		page.eval(t, `localStorage.setItem('syzygy.launch.combos', `+seed+`)`)
		page.call(t, "Page.reload", map[string]interface{}{})
		page.waitFor(t, `typeof openSpawnSheet === 'function' && HUB_MACHINE === 'mrx'`)
	}
	page.eval(t, `openSpawnSheet()`)
	page.waitFor(t, `spawnProjects.length === 3 && spawnPresets.length === 1 && spCatalog.agents.length === 3`)
	return page, f
}

const clickRow = `const pick=(text,nth)=>[...document.querySelectorAll('#sp-picker-list .sp-row')].filter(b=>b.textContent.includes(text))[nth||0];`

func TestNewChatProjectStepBadgesMachines(t *testing.T) {
	page, _ := newFleetChatPage(t, "awake", false, "")
	state := page.evalObject(t, `(()=>{
  const rows=[...document.querySelectorAll('#sp-picker-list .sp-row')].filter(b=>b.textContent.includes('dotfiles'));
  return {rows:rows.length,badges:rows.map(r=>[...r.querySelectorAll('.machine-badge')].map(b=>b.textContent))};
 })()`)
	if state["rows"] != float64(2) || fmt.Sprint(state["badges"]) != "[[] [NEMESIS]]" {
		t.Fatalf("two dotfiles rows, only the second badged NEMESIS: %#v", state)
	}
}

func TestNewChatAgentsComeFromTheProjectsMachine(t *testing.T) {
	page, f := newFleetChatPage(t, "awake", false, "")
	state := page.evalObject(t, `(async()=>{
  `+clickRow+`
  pick('dotfiles',1).click();
  for(let i=0;i<100&&!(spCatalog.agents.length===1&&spawnPresets.length===1&&spawnPresets[0].key==='g');i++)await new Promise(r=>setTimeout(r,20));
  return {agents:spCatalog.agents.map(a=>a.id),machine:spDraft.machine,presets:[...document.querySelectorAll('#sp-preset-list .sp-row')].map(b=>b.textContent)};
 })()`)
	if fmt.Sprint(state["agents"]) != "[gemini]" || state["machine"] != "vengeance" || len(state["presets"].([]interface{})) != 1 || !strings.Contains(fmt.Sprint(state["presets"]), "Gemini Pro") {
		t.Fatalf("the Agent step must list the peer's choices: %#v", state)
	}
	if !f.has("/api/launch-options?machine=vengeance") || !f.has("/api/presets?machine=vengeance") {
		t.Fatalf("options and presets must name the machine: %v", f.log)
	}
	before := f.count("/api/launch-options") + f.count("/api/presets")
	state = page.evalObject(t, `(async()=>{
  `+clickRow+`
  openSpawnView('project');pick('dotfiles',0).click();
  await new Promise(r=>setTimeout(r,300));
  return {agents:spCatalog.agents.length,machine:spDraft.machine,presets:[...document.querySelectorAll('#sp-preset-list .sp-row')].map(b=>b.textContent)};
 })()`)
	if state["agents"] != float64(3) || state["machine"] != "" || !strings.Contains(fmt.Sprint(state["presets"]), "Sol · Full") {
		t.Fatalf("back on the hub its own choices return: %#v", state)
	}
	if after := f.count("/api/launch-options") + f.count("/api/presets"); after != before {
		t.Fatalf("a machine already loaded must not be fetched again: %d -> %d", before, after)
	}
}

// launchOnPeer walks Project -> Agent -> Prompt on the peer's dotfiles and
// presses Start, with selection stubbed to record the chat it opens.
const launchOnPeer = `(async()=>{
  const row=[...document.querySelectorAll('#sp-picker-list .sp-row')].filter(b=>b.textContent.includes('dotfiles'))[1];
  row.click();
  for(let i=0;i<100&&!spawnPresets.length;i++)await new Promise(r=>setTimeout(r,20));
  document.querySelector('#sp-preset-list .sp-row').click();
  spTask.value='keep me';spTask.dispatchEvent(new Event('input'));
  return true;
 })()`

func TestNewChatSpawnNamesTheMachine(t *testing.T) {
	page, f := newFleetChatPage(t, "awake", false, "")
	page.eval(t, `(async()=>{await `+launchOnPeer+`;
  SPAWN_POLL_MS=5;window.__sel=null;const real=selectSession;selectSession=s=>{window.__sel=s.sessionId+'/'+s.machine;real(s)};
  spGo.click();return true})()`)
	page.waitFor(t, `window.__sel === 'peer/vengeance'`)
	bodies := f.spawnBodies()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"machine":"vengeance"`) || !strings.Contains(bodies[0], `"cwd":"/src/dotfiles"`) {
		t.Fatalf("spawn must name the machine: %v", bodies)
	}
	dialed := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, q := range f.wsQueries {
			if strings.Contains(q, "sock=2") && strings.Contains(q, "machine=vengeance") {
				return true
			}
		}
		return false
	}
	for i := 0; i < 100 && !dialed(); i++ {
		page.eval(t, `new Promise(r=>setTimeout(r,20))`)
	}
	if !dialed() {
		t.Fatalf("the page must dial the new chat on its machine: %v", f.wsQueries)
	}
}

func TestNewChatFirstPromptWaitsForTheDraftsMachine(t *testing.T) {
	page, _ := newFleetChatPage(t, "awake", false, "")
	state := page.evalObject(t, `(async()=>{await `+launchOnPeer+`;
  spImages=[{data:'photo',mimeType:'image/jpeg',thumb:''}];
  SPAWN_POLL_MS=5;const sent=[];
  // The hub's chat of the same name is open first; only the peer's may get the message.
  selectSession=s=>{currentBufferName=s.bufferName;currentSockMachine=s.machine;sessionId=s.sessionId;replayMode=false;processing=false;ws={readyState:1,send:raw=>sent.push(raw)};};
  currentBufferName='new chat';currentSockMachine='mrx';sessionId='hub';replayMode=false;processing=false;ws={readyState:1,send:raw=>sent.push('WRONG')};
  spGo.click();
  for(let i=0;i<100&&!sent.length;i++)await new Promise(r=>setTimeout(r,20));
  return {sent:sent.length,wrong:sent.includes('WRONG'),machine:currentSockMachine};
 })()`)
	if state["wrong"] != false || state["sent"] != float64(1) || state["machine"] != "vengeance" {
		t.Fatalf("first message goes only to the chat on the draft's machine: %#v", state)
	}
}

func TestSpawnToAsleepMachineKeepsDraft(t *testing.T) {
	page, f := newFleetChatPage(t, "awake", true, "")
	state := page.evalObject(t, `(async()=>{await `+launchOnPeer+`;
  spImages=[{data:'photo',mimeType:'image/jpeg',thumb:''}];
  await spStart();
  return {message:spEl('message').textContent,open:spSheet.classList.contains('visible'),task:spDraft.task,cwd:spDraft.cwd,machine:spDraft.machine,
    settings:{...spDraft.settings},images:spImages.length,stored:JSON.parse(localStorage.getItem('syzygy.launch.draft')),busy:spBusy,recovery:spRecoveryBuffer};
 })()`)
	stored := state["stored"].(map[string]interface{})
	settings := state["settings"].(map[string]interface{})
	if state["message"] != "NEMESIS did not answer" || state["open"] != true || state["task"] != "keep me" || state["cwd"] != "/src/dotfiles" || state["machine"] != "vengeance" ||
		settings["agent"] != "gemini" || state["images"] != float64(1) || stored["machine"] != "vengeance" || stored["task"] != "keep me" || state["busy"] != false || state["recovery"] != "" {
		t.Fatalf("a failed spawn must keep the whole draft and show the server's text: %#v", state)
	}
	if len(f.spawnBodies()) != 1 {
		t.Fatalf("one attempt: %v", f.spawnBodies())
	}
}

const comboSeed = `JSON.stringify({pins:[],recent:[
 {cwd:'/src/dotfiles',preset:'s',settings:{agent:'codex',model:'sol',mode:'full',effort:'high'}},
 {cwd:'/src/ghost',preset:'',machine:'ghost',settings:{agent:'codex',model:'sol',mode:'full',effort:'high'}}]})`

func TestCombosWithoutMachineAreHubCombos(t *testing.T) {
	page, f := newFleetChatPage(t, "awake", false, comboSeed)
	state := page.evalObject(t, `(async()=>{
  openSpawnView('home');
  const rows=[...document.querySelectorAll('#sp-home-list .sp-combo')];
  const info=rows.map(r=>({cls:r.className,title:r.title,aria:r.getAttribute('aria-disabled'),text:r.textContent}));
  rows[1].click();
  const afterGhost={view:spView,cwd:spDraft.cwd};
  rows[0].click();
  SPAWN_POLL_MS=5;selectSession=()=>{};
  const view=spView;
  spGo.click();
  return {info,afterGhost,view};
 })()`)
	info := state["info"].([]interface{})
	hub, ghost := info[0].(map[string]interface{}), info[1].(map[string]interface{})
	if strings.Contains(hub["cls"].(string), "asleep") || hub["aria"] != nil {
		t.Fatalf("a combo without machine is a hub combo: %#v", hub)
	}
	if !strings.Contains(ghost["cls"].(string), "asleep") || ghost["title"] != "ghost is not in the fleet" || ghost["aria"] != "true" {
		t.Fatalf("a combo for a machine outside the fleet is disabled: %#v", ghost)
	}
	if state["view"] != "prompt" || state["afterGhost"].(map[string]interface{})["view"] != "home" {
		t.Fatalf("tapping the ghost does nothing, the hub combo opens Prompt: %#v", state)
	}
	for i := 0; i < 100 && len(f.spawnBodies()) == 0; i++ {
		page.eval(t, `new Promise(r=>setTimeout(r,20))`)
	}
	bodies := f.spawnBodies()
	if len(bodies) != 1 || strings.Contains(bodies[0], "machine") {
		t.Fatalf("a hub launch sends no machine field: %v", bodies)
	}
}

func TestComboForAsleepMachineSaysWakeFirst(t *testing.T) {
	seed := `JSON.stringify({pins:[],recent:[{cwd:'/src/dotfiles',preset:'g',machine:'vengeance',settings:{agent:'gemini',model:'pro',mode:'auto',effort:''}}]})`
	page, f := newFleetChatPage(t, "asleep", false, seed)
	state := page.evalObject(t, `(async()=>{
  openSpawnView('home');
  const r=document.querySelector('#sp-home-list .sp-combo');
  r.click();
  await new Promise(r=>setTimeout(r,100));
  return {cls:r.className,aria:r.getAttribute('aria-disabled'),text:r.textContent,view:spView};
 })()`)
	if !strings.Contains(state["cls"].(string), "asleep") || state["aria"] != "true" || !strings.Contains(state["text"].(string), "wake first") || state["view"] != "home" {
		t.Fatalf("an asleep machine's combo is dimmed and inert: %#v", state)
	}
	if len(f.spawnBodies()) != 0 {
		t.Fatalf("must not launch: %v", f.spawnBodies())
	}
}

// pinnedRows lists, per dotfiles row in the Project step, whether it is pinned
// and carries the NEMESIS badge.
const pinnedRows = `(()=>[...document.querySelectorAll('#sp-picker-list .sp-project-item')].filter(i=>i.textContent.includes('dotfiles')).map(i=>(i.querySelector('.machine-badge')?'NEMESIS:':'hub:')+i.querySelector('button[aria-pressed]:not(.sp-row)').getAttribute('aria-pressed')))()`

func TestNewChatPinnedProjectIsPerMachine(t *testing.T) {
	page, _ := newFleetChatPage(t, "awake", false, "")
	reopen := func() {
		page.call(t, "Page.reload", map[string]interface{}{})
		page.waitFor(t, `typeof openSpawnSheet === 'function' && HUB_MACHINE === 'mrx'`)
		page.eval(t, `openSpawnSheet()`)
		page.waitFor(t, `spawnProjects.length === 3`)
	}
	page.eval(t, `[...document.querySelectorAll('#sp-picker-list .sp-project-item')].find(i=>i.querySelector('.machine-badge')).querySelector('button[aria-pressed]:not(.sp-row)').click()`)
	if got := fmt.Sprint(page.eval(t, "JSON.stringify("+pinnedRows+")")); !strings.Contains(got, "NEMESIS:true") || !strings.Contains(got, "hub:false") {
		t.Fatalf("pinning the NEMESIS row pins only it: %s", got)
	}
	reopen()
	if got := fmt.Sprint(page.eval(t, "JSON.stringify("+pinnedRows+")")); !strings.Contains(got, "NEMESIS:true") || !strings.Contains(got, "hub:false") {
		t.Fatalf("the pin survives a reload on its machine only: %s", got)
	}
	page.eval(t, `localStorage.setItem('syzygy.launch.pins', JSON.stringify(['/src/dotfiles']))`)
	reopen()
	if got := fmt.Sprint(page.eval(t, "JSON.stringify("+pinnedRows+")")); !strings.Contains(got, "NEMESIS:false") || !strings.Contains(got, "hub:true") {
		t.Fatalf("a bare-path pin stored earlier is a hub pin: %s", got)
	}
}

func TestNewChatTypedPathTargetsTheHub(t *testing.T) {
	page, f := newFleetChatPage(t, "awake", false, "")
	state := page.evalObject(t, `(async()=>{await `+launchOnPeer+`;
  const before=spDraft.machine;
  openSpawnView('project');spEl('picker-action').click();
  spDir.value='/src/typed';spEl('picker-action').click();
  await new Promise(r=>setTimeout(r,200));
  const agents=spCatalog.agents.map(a=>a.id);
  document.querySelector('#sp-preset-list .sp-row').click();
  SPAWN_POLL_MS=5;selectSession=()=>{};
  spGo.click();
  for(let i=0;i<100&&spBusy;i++)await new Promise(r=>setTimeout(r,20));
  return {before,machine:spDraft.machine,agents,view:spView};
 })()`)
	if state["before"] != "vengeance" || state["machine"] != "" || fmt.Sprint(state["agents"]) != "[codex claude opencode]" {
		t.Fatalf("a typed path targets the hub with its agents: %#v", state)
	}
	for i := 0; i < 100 && len(f.spawnBodies()) == 0; i++ {
		page.eval(t, `new Promise(r=>setTimeout(r,20))`)
	}
	bodies := f.spawnBodies()
	if len(bodies) != 1 || strings.Contains(bodies[0], "machine") || !strings.Contains(bodies[0], "/src/typed") {
		t.Fatalf("a hub launch sends no machine: %v", bodies)
	}
}

// The peer's slow options and presets answer after the user has moved on to the
// hub.  They are cached for the peer and never reach the sheet; returning to
// the peer then uses that cache (a late reply is kept, no refetch is needed).
func TestNewChatSlowOptionsForAnotherMachineAreDropped(t *testing.T) {
	page, f := newFleetChatPage(t, "awake", false, "")
	hold := make(chan struct{})
	f.mu.Lock()
	f.hold = hold
	f.mu.Unlock()
	state := page.evalObject(t, `(async()=>{
  `+clickRow+`
  pick('dotfiles',1).click();
  await new Promise(r=>setTimeout(r,100));
  openSpawnView('project');pick('dotfiles',0).click();
  await new Promise(r=>setTimeout(r,200));
  return {agents:spCatalog.agents.map(a=>a.id),presets:spawnPresets.map(p=>p.key),machine:spDraft.machine};
 })()`)
	if fmt.Sprint(state["agents"]) != "[codex claude opencode]" || fmt.Sprint(state["presets"]) != "[s]" || state["machine"] != "" {
		t.Fatalf("hub choices before release: %#v", state)
	}
	close(hold)
	state = page.evalObject(t, `(async()=>{
  await new Promise(r=>setTimeout(r,400));
  const hub={agents:spCatalog.agents.map(a=>a.id),presets:[...document.querySelectorAll('#sp-preset-list .sp-row')].map(b=>b.textContent),machine:spDraft.machine,settings:spDraft.settings.agent};
  `+clickRow+`
  openSpawnView('project');pick('dotfiles',1).click();
  await new Promise(r=>setTimeout(r,100));
  return {hub,peer:{agents:spCatalog.agents.map(a=>a.id),presets:[...document.querySelectorAll('#sp-preset-list .sp-row')].map(b=>b.textContent)}};
 })()`)
	hub := state["hub"].(map[string]interface{})
	if fmt.Sprint(hub["agents"]) != "[codex claude opencode]" || strings.Contains(fmt.Sprint(hub["presets"]), "Gemini") || hub["machine"] != "" || hub["settings"] != "codex" {
		t.Fatalf("a late reply for the peer must not land on the hub's sheet: %#v", hub)
	}
	peer := state["peer"].(map[string]interface{})
	if fmt.Sprint(peer["agents"]) != "[gemini]" || !strings.Contains(fmt.Sprint(peer["presets"]), "Gemini Pro") {
		t.Fatalf("going back to the peer shows its choices: %#v", peer)
	}
}
