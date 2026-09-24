# New Chat Stepper Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

Status: active, not started | Owner: acp-mobile New chat sheet (`index.html` spawn block)

**Goal:** Replace the one-screen New chat sheet with a Home, Project, Agent, Prompt stepper that remembers launch combos.

**Architecture:** Everything stays inside the existing `#spawn-sheet` and the `// --- Spawn sheet ---` block of `index.html`. `spView` becomes a small state machine over four steps (`home`, `project`, `preset`, `prompt`) plus the existing sub-views (`agent`, `model`, `mode`, `effort`, `path`, `save`). Launch combos live in a new localStorage key, `syzygy.launch.combos`. No server changes.

**Tech Stack:** Vanilla JS/HTML/CSS in `index.html`; Go Chrome-driven UI tests (`new_chat_ui_test.go`, `ui_test.go`); Node `node:test` unit tests (`index_test.mjs`) that load the spawn block into a `vm` context.

**Spec:** `docs/superpowers/specs/2026-09-19-new-chat-stepper-design.md`
**Design:** Figma SYZYGY, page `05 New Chat Screen`, the `New Chat stepper` row (https://www.figma.com/design/iUV2tD2yHjWE1sU5eEjcPb/SYZYGY?node-id=31-199)

## Global Constraints

- No server changes. Everything new lives in `index.html` and localStorage.
- Words come from the header helpers, never new tables: preset labels are verbatim from the rig; model names go through `canonicalModelName`; mode words go through `modeShortName` (fed by `/api/mode-words`). The UI never shows "Bypass" or "Full access" as a mode word.
- Row names stay `Agent`, `Model`, `Permissions`, `Effort`; empty effort reads `Agent default`; settings that match no preset read `<Agent name> / Custom`.
- Home row: provider icon, then the title `<model> in <project>`, then a detail line with the mode word coloured by `modeColor` followed by the short path, then the pin icon.
- Step titles show `1 / 3`, `2 / 3`, `3 / 3` beneath `Project`, `Agent`, `Prompt`.
- `recent` combos are capped at 8. Two combos are the same when `cwd` and all four settings match.
- No emojis anywhere, including test names and commit messages.
- Test commands: `go test ./...` and `node --test index_test.mjs`. Both must pass at the end of every task.

## Review Focus

1. A draft resumed on Prompt whose settings are no longer valid for the catalog. The catalog loads asynchronously, so the sheet must move to Agent **after** the catalog arrives, not judge validity against an empty catalog. Covered in Task 4 (`TestNewChatResumeFallsBackWhenSettingsGoStale`).
2. A combo tapped on Home whose model or mode was since removed must land on Prompt with `Start` disabled and a summary reading `Custom`, not the preset label. Covered in Task 3 (`TestNewChatStaleComboLandsOnPromptDisabled`).
3. Agents with no provider mark (`opencode`, `deepseek`): `providerIcon` returns `''`. The row must keep its alignment with an empty 18px slot rather than shifting the text left. Covered in Task 3 (the `iconSlot` assertion).
4. An in-flight or partially failed launch (`spRecoveryBuffer` set) must pin the sheet to Prompt. Back, the summary card and Home rows must not let the user edit settings for a chat that already exists. Covered in Task 2 (`TestNewChatMissingRecoveryCanReturnToDraft` update).
5. Pinning or unpinning on Home must not start a launch, because the pin sits inside a tappable list. Covered in Task 3 (the pin assertion checks `spView` stays `home`).

---

## File map

- Modify `index.html`:
  - CSS block around lines 1450-1500 (`#spawn-sheet` rules)
  - markup around lines 1961-1985 (`#spawn-sheet` dialog)
  - JS spawn block from `// --- Spawn sheet ---` (about line 5838) to `// --- Spawn sheet: end ---` (about line 6192)
- Modify `index_test.mjs`: `loadSpawnSheet` context stubs and the spawn tests (about lines 894-1000)
- Modify `new_chat_ui_test.go`: rewrite the flow tests, delete the swipe test, add the stepper tests
- Modify `ui_test.go:1745-1760`: the keyboard test that opens the old `options` view

---

### Task 1: Combo store and label helpers

**Files:**
- Modify: `index.html` (spawn block, directly after the `spRecent = ...filter(...)` line)
- Test: `index_test.mjs`

**Interfaces:**
- Produces (all global in the spawn block):
  - `spCombos: {pins: Combo[], recent: Combo[]}`, where `Combo = {cwd: string, preset: string, settings: {agent, model, mode, effort}}`
  - `spComboValid(c) -> bool`, `spCombo(c) -> Combo`, `spComboSame(a, b) -> bool`, `spComboPinned(c) -> bool`
  - `spRecordCombo(c)` (moves to the front of recent unless pinned, caps at 8, writes storage)
  - `spToggleComboPin(c)` (pin: removes from recent and appends to pins; unpin: removes from pins and puts it at the front of recent)
  - `spAgentFor(settings) -> agent | undefined`, `spMatchingPreset(settings) -> preset | undefined`
  - `spProjectName(cwd) -> string`, `spModelName(settings) -> string`, `spModeWord(settings) -> string`
  - `spComboTitle(c) -> "<model> in <project>"`
  - `spSummaryLabel(settings) -> preset label | "<Agent> / Custom"`
  - `spPresetDetail(p) -> "<Agent> / <model> / <mode word> / <effort id or Agent default>"`
- Consumes: `canonicalModelName`, `modeShortName` (globals defined outside the spawn block; the Node test stubs them)

- [ ] **Step 1: Add stubs to the Node loader and write the failing tests**

In `index_test.mjs`, inside `loadSpawnSheet`'s `context` object, add these defaults just before `...overrides`:

```js
    canonicalModelName: (id, models = []) => ({'gpt-5.6-sol':'Sol 5.6','opus[1m]':'Opus 5.5'}[id] || models.find(m => m.id === id)?.name || id),
    modeShortName: id => ({bypassPermissions:'full','agent-full-access':'full',plan:'plan'}[id] || id),
    modeColor: () => '#928374',
    providerIcon: agent => agent === 'claude' || agent === 'codex' ? '<span class="provider-icon"></span>' : '',
    localStorage: (() => { const m = new Map(); return {getItem:k => m.has(k) ? m.get(k) : null, setItem:(k,v) => m.set(k,String(v)), removeItem:k => m.delete(k)}; })(),
```

Then append these tests after `project search matches names...`:

```js
const combo = (cwd, model='gpt-5.6-sol', mode='agent-full-access') => ({cwd, preset:'c', settings:{agent:'codex', model, mode, effort:'high'}});

test('combos drop malformed entries on read and normalise settings', () => {
  const store = new Map([['syzygy.launch.combos', JSON.stringify({pins:[combo('/a'), {cwd:''}, null], recent:[{cwd:'/b', settings:{agent:'codex'}}, combo('/c')]})]]);
  const {get} = loadSpawnSheet({localStorage:{getItem:k => store.get(k) ?? null, setItem(){}, removeItem(){}}});
  assert.deepEqual(JSON.parse(JSON.stringify(get('spCombos'))), {pins:[combo('/a')], recent:[combo('/c')]});
});

test('recording a combo dedupes on cwd plus settings, caps recent at 8, and skips pinned combos', () => {
  const {context, get} = loadSpawnSheet();
  for (let i = 0; i < 10; i++) context.spRecordCombo(combo('/p' + i));
  assert.equal(get('spCombos.recent.length'), 8);
  assert.equal(get('spCombos.recent[0].cwd'), '/p9');
  context.spRecordCombo(combo('/p5'));
  assert.equal(get('spCombos.recent[0].cwd'), '/p5');
  assert.equal(get('spCombos.recent.length'), 8);
  context.spRecordCombo({...combo('/p5'), preset:'other'});
  assert.equal(get('spCombos.recent.filter(c => c.cwd === "/p5").length'), 1, 'preset key is informational, not identity');
  context.spToggleComboPin(combo('/p5'));
  context.spRecordCombo(combo('/p5'));
  assert.equal(get('spCombos.recent.some(c => c.cwd === "/p5")'), false);
  assert.equal(get('spCombos.pins.length'), 1);
});

test('unpinning puts the combo back at the front of recent and persists', () => {
  const {context, get} = loadSpawnSheet();
  context.spRecordCombo(combo('/a')); context.spRecordCombo(combo('/b'));
  context.spToggleComboPin(combo('/a'));
  assert.deepEqual(JSON.parse(JSON.stringify(get('spCombos.recent.map(c => c.cwd)'))), ['/b']);
  context.spToggleComboPin(combo('/a'));
  assert.deepEqual(JSON.parse(JSON.stringify(get('spCombos.recent.map(c => c.cwd)'))), ['/a', '/b']);
  assert.equal(JSON.parse(get('localStorage').getItem('syzygy.launch.combos')).recent[0].cwd, '/a');
});

test('labels use the header vocabulary', () => {
  const {context, set} = loadSpawnSheet();
  set('spCatalog', {defaultAgent:'codex', agents:[{id:'codex', name:'Codex', models:[], modes:[], efforts:[]}, {id:'claude', name:'Claude Code', models:[], modes:[], efforts:[]}]});
  set('spawnPresets', [{key:'c', label:'Sol 5.6 · Full', agent:'codex', model:'gpt-5.6-sol', mode:'agent-full-access', effort:'high'}]);
  set('spawnProjects', [{name:'dotfiles', path:'/u/.dotfiles'}]);
  assert.equal(context.spComboTitle(combo('/u/.dotfiles')), 'Sol 5.6 in dotfiles');
  assert.equal(context.spComboTitle(combo('/u/src/atlas')), 'Sol 5.6 in atlas');
  assert.equal(context.spModeWord(combo('/x').settings), 'full');
  assert.equal(context.spSummaryLabel(combo('/x').settings), 'Sol 5.6 · Full');
  assert.equal(context.spSummaryLabel({agent:'claude', model:'opus[1m]', mode:'plan', effort:''}), 'Claude Code / Custom');
  assert.equal(context.spPresetDetail({agent:'codex', model:'gpt-5.6-sol', mode:'agent-full-access', effort:'high'}), 'Codex / Sol 5.6 / full / high');
  assert.equal(context.spPresetDetail({agent:'claude', model:'opus[1m]', mode:'plan', effort:''}), 'Claude Code / Opus 5.5 / plan / Agent default');
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `node --test index_test.mjs`
Expected: the four new tests FAIL (`spCombos is not defined` / `context.spRecordCombo is not a function`).

- [ ] **Step 3: Implement**

In `index.html`, immediately after the line
`spRecent = Array.isArray(spRecent) ? spRecent.filter(p => typeof p === 'string') : [];` insert:

```js
// Launch combos: a project plus the four settings, remembered on this device.
// The preset key is informational; identity is cwd plus settings.
function spComboValid(c) { return !!c && typeof c.cwd === 'string' && !!c.cwd && !!c.settings && ['agent','model','mode'].every(k => typeof c.settings[k] === 'string' && c.settings[k]) && (c.settings.effort === undefined || typeof c.settings.effort === 'string'); }
function spCombo(c) { return {cwd:c.cwd, preset:typeof c.preset === 'string' ? c.preset : '', settings:spSettings(c.settings)}; }
function spComboSame(a, b) { return a.cwd === b.cwd && spFields.every(k => a.settings[k] === b.settings[k]); }
let spCombos = spRead('combos', {});
spCombos = {pins:Array.isArray(spCombos.pins) ? spCombos.pins.filter(spComboValid).map(spCombo) : [], recent:Array.isArray(spCombos.recent) ? spCombos.recent.filter(spComboValid).map(spCombo) : []};
function spComboPinned(c) { return spCombos.pins.some(p => spComboSame(p, c)); }
function spRecordCombo(c) {
  c = spCombo(c);
  if (!spComboPinned(c)) spCombos.recent = [c, ...spCombos.recent.filter(r => !spComboSame(r, c))].slice(0, 8);
  spWrite('combos', spCombos);
}
function spToggleComboPin(c) {
  c = spCombo(c);
  if (spComboPinned(c)) { spCombos.pins = spCombos.pins.filter(p => !spComboSame(p, c)); spCombos.recent = [c, ...spCombos.recent.filter(r => !spComboSame(r, c))].slice(0, 8); }
  else { spCombos.recent = spCombos.recent.filter(r => !spComboSame(r, c)); spCombos.pins = [...spCombos.pins, c]; }
  spWrite('combos', spCombos);
}
```

Then, directly after the existing `function spComplete() {...}` line, insert:

```js
// Labels reuse the header's words: canonicalModelName, modeShortName.
function spAgentFor(settings) { return spCatalog.agents.find(a => a.id === settings.agent); }
function spMatchingPreset(settings) { return spAllPresets().find(p => spFields.every(k => (p[k] || '') === settings[k])); }
function spProjectName(cwd) { return spawnProjects.find(p => p.path === cwd)?.name || cwd.split('/').filter(Boolean).pop() || cwd; }
function spModelName(settings) { const a = spAgentFor(settings); return canonicalModelName(settings.model, a?.models || [], (a?.name || '') + ' '); }
function spModeWord(settings) { return modeShortName(settings.mode); }
function spComboTitle(c) { return spModelName(c.settings) + ' in ' + spProjectName(c.cwd); }
function spSummaryLabel(settings) { const p = spMatchingPreset(settings); return p ? p.label : (spAgentFor(settings)?.name || settings.agent || 'Agent') + ' / Custom'; }
function spPresetDetail(p) { return [spAgentFor(p)?.name || p.agent, spModelName(p), spModeWord(p), p.effort || 'Agent default'].join(' / '); }
```

`spComplete` and `spAllPresets` are already defined above these lines. `spCatalog` and `spawnProjects` are declared earlier in the block.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `node --test index_test.mjs && go test ./...`
Expected: all PASS. Nothing renders the new helpers yet, so the Chrome tests are unaffected.

- [ ] **Step 5: Commit**

```bash
git add index.html index_test.mjs
git commit -m "feat(new-chat): launch combo store and stepper label helpers"
```

---

### Task 2: Stepper shell with the Project, Agent and Prompt steps

This task replaces the old main view, so the markup, the view machine and the three steps land together. Home arrives in Task 3; until then, `openSpawnSheet` falls through to Project because `spHomeEmpty()` is true.

**Files:**
- Modify: `index.html` (CSS, markup, spawn block)
- Modify: `new_chat_ui_test.go`, `ui_test.go:1745-1760`, `index_test.mjs:956-970`

**Interfaces:**
- Consumes: everything Task 1 produces
- Produces:
  - `spView` values: `'home' | 'project' | 'preset' | 'prompt' | 'agent' | 'model' | 'mode' | 'effort' | 'path' | 'save'` (initial `'home'`)
  - `openSpawnView(view)`, `renderSpawnStep()` (replaces `renderSpawnMain`), `spBackOf(view) -> view | ''`, `spHomeEmpty() -> bool`
  - `spCustomizeOpen: bool`, `spNameOpen: bool`
  - `spDraft.step: string` (persisted with the draft)
  - `SP_PIN_SVG`, `spPinButton(label, pinned, toggle) -> HTMLButtonElement`
  - DOM ids: `sp-title-text`, `sp-step`, `sp-home`, `sp-home-list`, `sp-preset-step`, `sp-preset-list`, `sp-customize`, `sp-settings`, `sp-save` (now a row), `sp-prompt`, `sp-summary`, `sp-name-toggle`, `sp-next`
  - Removed ids: `sp-main`, `sp-project`, `sp-presets`, `sp-reset`, `sp-all-presets`, `sp-preset-label`, `sp-options`, `sp-options-panel`, `sp-bottom-tools`, `sp-draft-note`

- [ ] **Step 1: Write the failing Chrome tests**

In `new_chat_ui_test.go`:

1. Delete `TestNewChatPresetsSwipeHorizontallyWithoutSelecting` and `TestNewChatPresetOverrideResetAndExactLaunch` entirely.
2. Add:

```go
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
```

3. Update the remaining tests in `new_chat_ui_test.go`:

`TestNewChatSearchCannotBecomePathAndDraftSurvives`: replace its JS body with

```js
(()=>{
  spTask.value='Do not lose me';spTask.dispatchEvent(new Event('input'));
  const q=document.getElementById('sp-search');q.value='nonexistent';q.dispatchEvent(new Event('input'));
  const cwd=spDraft.cwd;
  document.getElementById('sp-close').click();openSpawnSheet();
  const task=spTask.value,view=spView;
  openSpawnView('preset');
  document.getElementById('sp-agent').click();
  [...document.querySelectorAll('#sp-picker-list button')].find(b=>b.textContent.includes('Claude Code')).click();
  return {cwd,task,view,model:spDraft.settings.model,mode:spDraft.settings.mode,disabled:spGo.disabled};
 })()
```

and add `|| state["view"] != "project"` to its first `if` condition.

`TestNewChatCenteredHeadersAndKeyboardHeight`: replace its JS expression with

```js
(()=>{const title=document.getElementById('sp-title').getBoundingClientRect(),sheet=spSheet.getBoundingClientRect(),foot=document.getElementById('sp-footer').getBoundingClientRect();openSpawnView('preset');const back=document.getElementById('sp-back');const out={center:Math.abs((title.left+title.right)/2-(sheet.left+sheet.right)/2),footer:foot.bottom<=innerHeight,overflow:document.documentElement.scrollWidth>innerWidth,backLeft:back.getBoundingClientRect().left<80,backText:back.textContent.trim(),icon:!!back.querySelector('img')};back.click();return out})()
```

`TestNewChatCustomPresetPinsAndPartialFailure`: replace the first four JS lines (the preset click through `spEl('back').click();`) with

```js
  openSpawnView('preset');document.querySelector('#sp-preset-list .sp-row').click();
  openSpawnView('preset');spEl('customize').click();
  spEl('save').click();spEl('save-name').value='My setup';spEl('picker-action').click();
  const custom=spCustom.find(p=>p.label==='My setup');
  openSpawnView('project');document.querySelector('#sp-picker-list button[aria-label="Pin dotfiles"]').click();
  const pinned=spPins.includes('/src/dotfiles');openSpawnView('prompt');
```

`TestNewChatMissingRecoveryCanReturnToDraft`: replace its JS body with

```js
(async()=>{
  openSpawnView('preset');document.querySelector('#sp-preset-list .sp-row').click();spTask.value='Unsent work';spTask.dispatchEvent(new Event('input'));
  spRecoveryBuffer='missing buffer';spRecoveryKeepsTask=true;persistSpawnDraft();renderSpawnStep();
  const locked=spEl('summary').disabled;
  spEl('back').click();const stayed=spView;
  spEl('release').click();
  return {locked,stayed,editable:!spEl('summary').disabled,task:spDraft.task,pending:spRecoveryBuffer,canLaunch:!spGo.disabled,stored:JSON.parse(localStorage.getItem('syzygy.launch.draft')).pendingBuffer};
 })()
```

and add `|| state["stayed"] != "prompt"` to its `if`.

`TestNewChatModelPickerGroupsByProvider`: insert `openSpawnView('preset');` as the first statement of its JS.

`TestNewChatVisualReview`: replace `page.eval(t, "document.querySelector('#sp-presets button').click()")` with `page.eval(t, "openSpawnView('preset');document.querySelector('#sp-preset-list .sp-row').click()")`, and change the view list to `[]string{"project", "preset", "model", "prompt"}`.

In `ui_test.go` (about line 1750), replace

```js
		openSpawnView('options');
		spName.focus();
```

with

```js
		openSpawnView('prompt');
		spEl('name-toggle').click();
		spName.focus();
```

In `index_test.mjs`, delete `const chipLabels = ...` and change the two assertions that use it:

```js
  assert.deepEqual(JSON.parse(JSON.stringify(get('spawnPresets.map(p => p.label)'))),['Sol','Astra']);
```

```js
  assert.deepEqual(JSON.parse(JSON.stringify(get('spawnPresets.map(p => p.label)'))),['Sol']);
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'NewChat|Keyboard|Viewport' ./... ; node --test index_test.mjs`
Expected: the new and edited Chrome tests FAIL (`spEl('title-text')` is null, `openSpawnView('preset')` renders nothing). The Node tests still pass.

- [ ] **Step 3: Replace the markup**

In `index.html`, replace the whole `<div id="spawn-sheet" ...>` element (from `<div id="spawn-sheet"` through its closing `</div>` after `#sp-footer`) with:

```html
<div id="spawn-sheet" role="dialog" aria-modal="true" aria-labelledby="sp-title" tabindex="-1">
  <div id="sp-head">
    <div id="sp-nav"><button id="sp-close" class="sp-icon" aria-label="Cancel new chat" title="Cancel new chat"></button><button id="sp-back" class="sp-icon" aria-label="Back" title="Back" hidden></button></div>
    <h2 id="sp-title"><span id="sp-title-text">New chat</span><span id="sp-step" hidden></span></h2>
    <button id="sp-release" class="sp-icon" aria-label="Return to draft" title="Return to draft" hidden></button>
  </div>
  <div id="sp-body">
    <p id="sp-message" role="status" hidden></p>
    <div id="sp-home" hidden><div id="sp-home-list"></div></div>
    <div id="sp-preset-step" hidden>
      <div class="sp-label">Presets</div>
      <div id="sp-preset-list"></div>
      <button id="sp-customize" class="sp-row" aria-expanded="false" aria-controls="sp-settings"></button>
      <div id="sp-settings" hidden><button id="sp-agent" class="sp-row" aria-label="Change agent"></button><button id="sp-model" class="sp-row" aria-label="Change model"></button><button id="sp-mode" class="sp-row" aria-label="Change permissions"></button><button id="sp-effort" class="sp-row" aria-label="Change effort"></button><button id="sp-save" class="sp-row" aria-label="Save as preset" hidden></button></div>
    </div>
    <div id="sp-prompt" hidden>
      <button id="sp-summary" class="sp-row" aria-label="Change agent and settings"></button>
      <textarea id="sp-task" aria-label="First message" placeholder="What are we working on?"></textarea>
      <button id="sp-name-toggle" class="sp-row" aria-expanded="false" aria-controls="sp-name"></button>
      <input id="sp-name" aria-label="Chat name" placeholder="e.g. New chat redesign" autocapitalize="off" hidden>
    </div>
    <div id="sp-picker" hidden><input id="sp-search" type="search" aria-label="Search choices" placeholder="Search" autocapitalize="off" autocomplete="off"><p id="sp-picker-note" class="sp-note"></p><div id="sp-picker-list"></div></div>
    <div id="sp-path" hidden><label class="sp-label" for="sp-dir">Project directory</label><input id="sp-dir" placeholder="/path/to/project" autocapitalize="off" autocorrect="off" spellcheck="false"><p class="sp-note">Enter an existing directory on the rig. Search text never changes your selected project.</p></div>
    <div id="sp-save-panel" hidden><label class="sp-label" for="sp-save-name">Preset name</label><input id="sp-save-name" placeholder="e.g. Focused coding" maxlength="60"><p class="sp-note">Saves the agent, model, permissions, and effort on this device.</p></div>
  </div>
  <div id="sp-footer"><button id="sp-go" aria-label="Start chat" title="Start chat" hidden>Start</button><button id="sp-next" hidden></button><button id="sp-picker-action" class="sp-icon" aria-label="Done" title="Done" hidden></button></div>
</div>
```

Before replacing it, open the current element and copy across any attribute that isn't shown above. The `#sp-picker`, `#sp-path` and `#sp-save-panel` children must stay byte-identical to today's.

- [ ] **Step 4: Replace the CSS**

In the `#spawn-sheet` CSS block, delete these rules: `#sp-project`, `#sp-project .sp-row-title`, `#sp-presets`, `.sp-chip`, the `/* Preset chips live in a horizontal strip... */` comment and its `#spawn-sheet .sp-chip` rule, `.sp-chip.sel`, `#sp-reset`, `#sp-preset-label`, `#sp-preset-label.modified`, `#sp-bottom-tools`, `#sp-bottom-tools .sp-note`, `#sp-go img`, `#sp-go[aria-busy=true] img`, and the reduced-motion rule that targets it. Then append:

```css
#sp-title { display:flex; flex-direction:column; align-items:center; line-height:1.25; }
#sp-step { font-size:11px; font-weight:400; color:var(--fg-mute); letter-spacing:.5px; }
.sp-group-label { margin-top:18px; }
.sp-group-label:first-child { margin-top:0; }
.sp-combo .provider-icon { width:18px; height:18px; flex:0 0 18px; }
.sp-mode-word { font-weight:600; margin-right:8px; }
#sp-customize img, #sp-name-toggle img { transition:transform .15s; }
#sp-customize[aria-expanded=true] img, #sp-name-toggle[aria-expanded=true] img { transform:rotate(90deg); }
#sp-prompt { display:flex; flex-direction:column; gap:12px; height:100%; }
#sp-summary { border:1px solid var(--bg2); border-radius:12px; padding:12px 14px; background:var(--bg1); min-height:64px; }
#sp-prompt #sp-task { flex:1; min-height:160px; resize:none; }
#sp-go { color:var(--bg0); font-size:15px; font-weight:700; }
#sp-go[aria-busy=true] { animation:sp-pulse 1s ease-in-out infinite alternate; }
@media(prefers-reduced-motion:reduce) { #sp-go[aria-busy=true] { animation:none; } #sp-customize img, #sp-name-toggle img { transition:none; } }
#sp-next { width:100%; height:52px; border:1px solid var(--bg2); border-radius:12px; background:var(--bg1); color:var(--fg); font-size:15px; }
```

The existing `#sp-task` rule stays. It still styles the textarea, and the `#sp-prompt #sp-task` rule overrides its height and resize behaviour.

- [ ] **Step 5: Rewrite the spawn block's view code**

All edits are inside `// --- Spawn sheet ---` ... `// --- Spawn sheet: end ---`.

5a. Remove `const spPresets = document.getElementById('sp-presets');`.

5b. Change the draft restore loop to include `step`:

```js
let spDraft = {cwd: '', name: '', task: '', preset: '', step: '', settings: spSettings(spStoredDraft.settings)};
for (const k of ['cwd','name','task','preset','step']) if (typeof spStoredDraft[k] === 'string') spDraft[k] = spStoredDraft[k];
```

5c. Change `let spView = 'main', ...` to `let spView = 'home', spBusy = false, spReturnFocus = null, spPickerOrigin = null, spCustomizeOpen = false, spNameOpen = false;`

5d. Replace the icon wiring line (`for (const [id, icon] of Object.entries({close:'close',...}))`) with:

```js
for (const [id, icon] of Object.entries({close:'close',back:'back',release:'reset','picker-action':'confirm'})) spButtonIcon(spEl(id), icon);
```

5e. Delete `renderSpawnPresets` and `renderSpawnMain`. Add in their place:

```js
const SP_STEPS = {project:'1 / 3', preset:'2 / 3', prompt:'3 / 3'};
const SP_TITLES = {home:'New chat', project:'Project', preset:'Agent', prompt:'Prompt', agent:'Agent', model:'Model', mode:'Permissions', effort:'Effort', path:'Project directory', save:'Save preset'};
const SP_PICKERS = ['project','agent','model','mode','effort'];
const SP_SUBVIEWS = ['agent','model','mode','effort','save'];
const SP_PIN_SVG = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1z"/></svg>';
function spHomeEmpty() { return !spCombos.pins.length && !spCombos.recent.length; }
function spBackOf(view) { return {path:'project', save:'preset', agent:'preset', model:'preset', mode:'preset', effort:'preset', prompt:'preset', preset:'project', project:spHomeEmpty() ? '' : 'home'}[view] || ''; }
function spPinButton(label, pinned, toggle) {
  const pin=document.createElement('button'); pin.type='button'; pin.className='sp-icon';
  pin.setAttribute('aria-label',(pinned ? 'Unpin ' : 'Pin ')+label); pin.title=pin.getAttribute('aria-label'); pin.setAttribute('aria-pressed',String(pinned));
  pin.innerHTML=SP_PIN_SVG; onTapPick(pin,toggle); return pin;
}
function renderSpawnHome() {}
function renderSpawnPresetStep() {
  const locked=spBusy || !!spRecoveryBuffer, match=spMatchingPreset(spDraft.settings), list=spEl('preset-list');
  list.replaceChildren();
  for (const p of spAllPresets()) {
    const item=document.createElement('div'); item.className='sp-project-item';
    const b=document.createElement('button'); b.type='button'; const sel=match?.key === p.key;
    b.className='sp-row'+(sel ? ' sel' : ''); b.setAttribute('aria-pressed',String(sel)); b.setAttribute('data-preset',p.key); b.disabled=locked;
    spRowContent(b,p.label,spPresetDetail(p),null);
    onTapPick(b,()=>{applySpawnPreset(p.key);openSpawnView('prompt');}); item.appendChild(b);
    if (spCustom.some(c => c.key === p.key)) {
      const del=document.createElement('button'); del.type='button'; del.className='sp-icon'; del.setAttribute('aria-label','Delete preset '+p.label); del.title=del.getAttribute('aria-label'); del.disabled=locked; spButtonIcon(del,'close');
      onTapPick(del,()=>{spCustom=spCustom.filter(c => c.key !== p.key);spWrite('presets',spCustom);renderSpawnStep();}); item.appendChild(del);
    }
    list.appendChild(item);
  }
  spRowContent(spEl('customize'),'Customize','','chevron');
  spEl('customize').setAttribute('aria-expanded',String(spCustomizeOpen)); spEl('customize').disabled=locked;
  spEl('settings').hidden=!spCustomizeOpen;
  for (const [field,label] of [['agent','Agent'],['model','Model'],['mode','Permissions'],['effort','Effort']]) {
    spRowContent(spEl(field),spChoiceName(field,spDraft.settings[field]),'','chevron',label);
    spEl(field).disabled=locked || (field !== 'agent' && !spAgent());
  }
  spEl('effort').hidden=!spChoices('effort').length && !spDraft.settings.effort;
  spRowContent(spEl('save'),'Save as preset','','save'); spEl('save').hidden=!spComplete(); spEl('save').disabled=locked;
}
function renderSpawnPrompt() {
  const locked=spBusy || !!spRecoveryBuffer;
  // A stale combo reads Custom even when its ids still match a preset.
  const label=spComplete() ? spSummaryLabel(spDraft.settings) : (spAgentFor(spDraft.settings)?.name || spDraft.settings.agent || 'Agent')+' / Custom';
  spRowContent(spEl('summary'),label,spDraft.cwd ? spProjectName(spDraft.cwd)+'  '+shortPath(spDraft.cwd) : 'Choose a project','chevron');
  spEl('summary').disabled=locked; spTask.disabled=locked;
  const open=spNameOpen || !!spDraft.name;
  spRowContent(spEl('name-toggle'),'Chat name','optional','chevron');
  spEl('name-toggle').setAttribute('aria-expanded',String(open)); spEl('name-toggle').disabled=locked;
  spName.hidden=!open; spName.disabled=locked;
}
function renderSpawnStep() {
  const locked=spBusy || !!spRecoveryBuffer;
  if (spView === 'home') renderSpawnHome();
  if (spView === 'preset') renderSpawnPresetStep();
  if (spView === 'prompt') renderSpawnPrompt();
  spEl('next').disabled=locked || (spView === 'preset' && !spComplete());
  spGo.disabled=spBusy || (!spRecoveryBuffer && (!spDraft.cwd || !spComplete()));
  spEl('release').hidden=!spRecoveryBuffer; spEl('release').disabled=spBusy;
  spGo.setAttribute('aria-busy',String(spBusy));
  spGo.setAttribute('aria-label',spRecoveryBuffer ? 'Open created chat' : 'Start chat');
  spGo.title=spGo.getAttribute('aria-label');
  spGo.textContent=spRecoveryBuffer ? 'Open chat' : 'Start';
}
```

`renderSpawnHome` is deliberately empty here. Task 3 fills it in.

5f. Change `applySpawnPreset` to end with `renderSpawnStep();` instead of `renderSpawnMain();`. In `loadSpawnPresets`, `loadSpawnProjects` and `loadSpawnCatalog`, replace every `renderSpawnMain()` with `renderSpawnStep()`. In `loadSpawnPresets`, delete `if(spView === 'presets') renderSpawnPicker();`.

5g. `pickSpawnProject` becomes:

```js
function pickSpawnProject(path) { spDraft.cwd=path; spDir.value=path; persistSpawnDraft(); openSpawnView('preset'); }
```

5h. In `renderSpawnPicker`:
- Replace the inline pin creation in the `project` branch (the four lines from `const pin=document.createElement('button');` through `item.appendChild(pin);list.appendChild(item);`) with:

```js
        item.appendChild(spPinButton(p.name,spPins.includes(p.path),()=>{spPins=spPins.includes(p.path)?spPins.filter(x=>x!==p.path):[...spPins,p.path];spWrite('pins',spPins);renderSpawnPicker();}));list.appendChild(item);
```

- Delete the whole `else if (spView === 'presets') {...}` branch.
- In the field-choice pick callback, replace `persistSpawnDraft();openSpawnView('main');` with:

```js
      spDraft.preset=spMatchingPreset(spDraft.settings)?.key || '';
      persistSpawnDraft();openSpawnView('preset');
```

5i. Replace `openSpawnView` with:

```js
function openSpawnView(view) {
  // A chat that already exists pins the sheet to Prompt until released.
  if ((spBusy || spRecoveryBuffer) && view !== 'prompt') return;
  if (SP_SUBVIEWS.includes(view) && spView === 'preset') spPickerOrigin=document.activeElement;
  if (view === 'preset' && !SP_SUBVIEWS.includes(spView)) spCustomizeOpen=!spMatchingPreset(spDraft.settings);
  if (view === 'model') spModelOpen=new Set([modelGroupKey(spDraft.settings.model)]);
  spView=view;
  if (view === 'home' || SP_STEPS[view]) { spDraft.step=view; persistSpawnDraft(); }
  spEl('title-text').textContent=SP_TITLES[view];
  spEl('step').textContent=SP_STEPS[view] || ''; spEl('step').hidden=!SP_STEPS[view];
  const back=spBackOf(view);
  spEl('close').hidden=!!back; spEl('back').hidden=!back;
  spEl('home').hidden=view !== 'home'; spEl('preset-step').hidden=view !== 'preset'; spEl('prompt').hidden=view !== 'prompt';
  spEl('picker').hidden=!SP_PICKERS.includes(view);
  spEl('path').hidden=view !== 'path'; spEl('save-panel').hidden=view !== 'save';
  spGo.hidden=view !== 'prompt';
  spEl('next').hidden=view !== 'home' && view !== 'preset';
  spEl('next').textContent=view === 'home' ? 'Start from scratch' : 'Next';
  spEl('picker-action').hidden=!SP_PICKERS.includes(view) && view !== 'path' && view !== 'save';
  spButtonIcon(spEl('picker-action'),view === 'project' ? 'folder' : view === 'save' ? 'save' : 'confirm');
  const actionLabel=view === 'project' ? 'Enter project directory' : view === 'save' ? 'Save preset' : 'Done';
  spEl('picker-action').setAttribute('aria-label',actionLabel); spEl('picker-action').title=actionLabel;
  spEl('search').value=''; spEl('search').placeholder='Search '+(view === 'mode' ? 'permissions' : view === 'project' ? 'projects' : view+'s');
  spEl('picker-note').textContent=view === 'mode' ? 'Permissions control which actions require your approval.' : view === 'model' ? 'Models available for '+(spAgent()?.name || 'this agent')+'.' : '';
  spEl('body').scrollTop=0;
  if (view === 'path') spDir.value=spDraft.cwd;
  renderSpawnStep();
  if (SP_PICKERS.includes(view)) renderSpawnPicker();
  if (view === 'preset' && spPickerOrigin?.isConnected && spEl('preset-step').contains(spPickerOrigin)) { spPickerOrigin.focus({preventScroll:true}); spPickerOrigin=null; }
  else (back ? spEl('back') : spEl('close')).focus({preventScroll:true});
}
```

5j. In `openSpawnSheet`:
- Change the signature to `function openSpawnSheet(cwd, step='') {`.
- After `spName.value=spDraft.name;spTask.value=spDraft.task;spDir.value=spDraft.cwd;`, add `spNameOpen=false;`.
- Replace `openSpawnView('main');spEl('close').focus({preventScroll:true});` with:

```js
  openSpawnView(spRecoveryBuffer ? 'prompt' : step || (spHomeEmpty() ? 'project' : 'home'));
```

Task 4 replaces this line with draft-step resume.

5k. Replace the listener wiring between `spScrim.addEventListener('click',hideSpawnSheet);...` and the `spSheet.addEventListener('keydown',...)` block with:

```js
spScrim.addEventListener('click',hideSpawnSheet);spEl('close').addEventListener('click',hideSpawnSheet);
spEl('back').addEventListener('click',()=>{const back=spBackOf(spView);if(back)openSpawnView(back);});
for (const field of spFields) spEl(field).addEventListener('click',()=>openSpawnView(field));
spEl('save').addEventListener('click',()=>{spEl('save-name').value='';openSpawnView('save');});
spEl('customize').addEventListener('click',()=>{spCustomizeOpen=!spCustomizeOpen;renderSpawnStep();});
spEl('summary').addEventListener('click',()=>openSpawnView('preset'));
spEl('name-toggle').addEventListener('click',()=>{spNameOpen=!(spNameOpen || !!spDraft.name);renderSpawnStep();if(spNameOpen)spName.focus({preventScroll:true});});
spEl('next').addEventListener('click',()=>{if(spEl('next').disabled)return;openSpawnView(spView === 'home' ? 'project' : 'prompt');});
spEl('release').addEventListener('click',()=>{if(spBusy)return;spRecoveryBuffer='';spRecoveryKeepsTask=false;persistSpawnDraft();spMessage('Draft restored. The previously created chat may still be in your chat list.');renderSpawnStep();});
spEl('search').addEventListener('input',renderSpawnPicker);
spTask.addEventListener('input',()=>{spDraft.task=spTask.value;persistSpawnDraft();});
spName.addEventListener('input',()=>{spDraft.name=spName.value;persistSpawnDraft();});
spEl('picker-action').addEventListener('click',()=>{
  if(spView==='project'){openSpawnView('path');return;}
  if(spView==='path') { const path=spDir.value.trim();if(!path || !(path.startsWith('/') || path.startsWith('~/'))){spMessage('Enter an absolute path or a path starting with ~/.');spDir.focus();return;}spMessage('');pickSpawnProject(path);return; }
  if(spView==='save') { const label=spEl('save-name').value.trim();if(!label){spEl('save-name').focus();return;}const key='local:'+Date.now().toString(36)+Math.random().toString(36).slice(2,6);spCustom.push({key,label,...spDraft.settings});spWrite('presets',spCustom);spDraft.preset=key;persistSpawnDraft(); }
  openSpawnView('preset');
});
```

The name toggle stays open while the draft has a name. It closes only when the name is empty.

5l. In the `keydown` handler, replace the Escape branch with:

```js
  if(e.key==='Escape'){e.preventDefault();e.stopPropagation();const back=spBackOf(spView);if(back && !spRecoveryBuffer && !spBusy)openSpawnView(back);else hideSpawnSheet();}
```

5m. In the `spGo` click handler, replace both `renderSpawnMain()` calls with `renderSpawnStep()`.

5n. Search the whole file for `renderSpawnMain`, `sp-presets`, `'main'`, `'options'`, `'presets'`, `sp-project` and `all-presets`. Nothing should remain in the spawn block or anywhere else. `grep -n "renderSpawnMain\|sp-presets\|openSpawnView('main')\|openSpawnView('options')\|all-presets" index.html` must print nothing.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./... && node --test index_test.mjs`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add index.html index_test.mjs new_chat_ui_test.go ui_test.go
git commit -m "feat(new-chat): Project, Agent, Prompt stepper replaces the one-screen sheet"
```

---

### Task 3: Home, remembered combos and pins

**Files:**
- Modify: `index.html` (spawn block: `renderSpawnHome`, `pickSpawnCombo`, and the `spGo` success path)
- Test: `new_chat_ui_test.go`

**Interfaces:**
- Consumes: `spCombos`, `spRecordCombo`, `spToggleComboPin`, `spComboTitle`, `spModeWord`, `spMatchingPreset` (Task 1); `spPinButton`, `openSpawnView`, `renderSpawnStep` (Task 2); global `providerIcon`, `modeColor`, `shortPath`
- Produces: `pickSpawnCombo(c)`. On success, `spGo` records the combo and clears `spDraft.step`.

- [ ] **Step 1: Write the failing tests**

Append to `new_chat_ui_test.go`:

```go
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
	page.waitFor(t, `typeof openSpawnSheet === 'function' && spawnProjects !== undefined`)
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'NewChat(LaunchRemembers|PinnedCombo|StaleCombo)' ./...`
Expected: FAIL. Home has no rows because `renderSpawnHome` is empty, and a launch doesn't record anything yet.

- [ ] **Step 3: Implement**

Replace the empty `function renderSpawnHome() {}` with:

```js
function renderSpawnHome() {
  const list=spEl('home-list'); list.replaceChildren();
  for (const [name,combos] of [['Pinned',spCombos.pins],['Recent',spCombos.recent]]) {
    if (!combos.length) continue;
    const head=document.createElement('div'); head.className='sp-label sp-group-label'; head.textContent=name; list.appendChild(head);
    for (const c of combos) {
      const item=document.createElement('div'); item.className='sp-project-item';
      const row=document.createElement('button'); row.type='button'; row.className='sp-row sp-combo';
      const title=spComboTitle(c), word=spModeWord(c.settings);
      // Agents without a provider mark keep an empty slot so titles align.
      row.innerHTML=providerIcon(c.settings.agent) || '<span class="provider-icon" aria-hidden="true"></span>';
      const copy=document.createElement('span'); copy.className='sp-row-copy';
      const t=document.createElement('span'); t.className='sp-row-title'; t.textContent=title; copy.appendChild(t);
      const d=document.createElement('span'); d.className='sp-row-detail';
      const mode=document.createElement('span'); mode.className='sp-mode-word'; mode.textContent=word; mode.style.color=modeColor(c.settings.mode); d.appendChild(mode);
      const path=document.createElement('span'); path.textContent=shortPath(c.cwd); d.appendChild(path);
      copy.appendChild(d); row.appendChild(copy);
      row.setAttribute('aria-label',title+', '+word+', '+shortPath(c.cwd));
      onTapPick(row,()=>pickSpawnCombo(c)); item.appendChild(row);
      item.appendChild(spPinButton(title,name === 'Pinned',()=>{spToggleComboPin(c);renderSpawnHome();}));
      list.appendChild(item);
    }
  }
}
function pickSpawnCombo(c) {
  spDraft.cwd=c.cwd; spDraft.settings=spSettings(c.settings); spDraft.preset=spMatchingPreset(spDraft.settings)?.key || '';
  spDir.value=c.cwd; persistSpawnDraft(); spMessage(''); openSpawnView('prompt');
}
```

`modeColor` returns a hex colour string. It lives outside the spawn block, so the Node loader stubs it (Task 1).

In the `spGo` click handler's success branch (`if(opened) { ... }`), insert these two statements right after `spWrite('recent',spRecent);`:

```js
spRecordCombo({cwd:spDraft.cwd,preset:spDraft.preset,settings:spDraft.settings});spDraft.step='';
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... && node --test index_test.mjs`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add index.html new_chat_ui_test.go
git commit -m "feat(new-chat): Home lists pinned and recent launch combos"
```

---

### Task 4: Entry points and resuming the saved step

**Files:**
- Modify: `index.html` (`openSpawnSheet`, `loadSpawnCatalog`, `spawnInProject`)
- Test: `new_chat_ui_test.go`

**Interfaces:**
- Consumes: `spDraft.step`, `spHomeEmpty`, `openSpawnView` (Task 2)
- Produces: `spResumeStep() -> view`, `spResumeCheck: bool`; `spawnInProject(cwd)` opens on Agent

- [ ] **Step 1: Write the failing tests**

Append to `new_chat_ui_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'NewChat(EntryPoints|ResumeFallsBack)' ./...`
Expected: FAIL. `spawnInProject` opens Project, and reopening doesn't resume the step.

- [ ] **Step 3: Implement**

Add above `openSpawnSheet`:

```js
// Reopening resumes the saved step; a Prompt draft is checked again once the
// catalog arrives, because validity cannot be judged against an empty catalog.
let spResumeCheck = false;
function spResumeStep() {
  const s=spDraft.step;
  if (s === 'prompt' || s === 'preset') return spDraft.cwd ? s : 'project';
  if (s === 'project') return 'project';
  return spHomeEmpty() ? 'project' : 'home';
}
```

In `openSpawnSheet`, replace the line added in Task 2 step 5j with:

```js
  const view=spRecoveryBuffer ? 'prompt' : step || spResumeStep();
  spResumeCheck=view === 'prompt' && !spRecoveryBuffer && !step;
  openSpawnView(view);
```

In `loadSpawnCatalog`, directly after the success-path `renderSpawnStep();`, add:

```js
    if (spResumeCheck && spView === 'prompt' && !spComplete()) openSpawnView('preset');
    spResumeCheck=false;
```

In `pickSpawnCombo`, add `spResumeCheck=false;` as the first statement. A tapped stale combo stays on Prompt, as the spec requires.

Change `function spawnInProject(cwd) { openSpawnSheet(cwd); }` to:

```js
function spawnInProject(cwd) { openSpawnSheet(cwd, 'preset'); }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... && node --test index_test.mjs`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add index.html new_chat_ui_test.go
git commit -m "feat(new-chat): per-project entry opens Agent and reopening resumes the step"
```

---

### Task 5: Visual review against Figma

**Files:**
- Modify: `new_chat_ui_test.go` (`TestNewChatVisualReview`)

- [ ] **Step 1: Extend the screenshot test to all four steps**

Replace the body of `TestNewChatVisualReview` after the `waitFor` with:

```go
	page.eval(t, `spCombos={pins:[{cwd:'/src/acp-mobile',preset:'s',settings:{agent:'codex',model:'sol',mode:'full',effort:'high'}}],recent:[{cwd:'/src/dotfiles',preset:'',settings:{agent:'claude',model:'sonnet',mode:'manual',effort:''}}]};spDraft.cwd='/src/acp-mobile';spDraft.settings={agent:'codex',model:'sol',mode:'full',effort:'high'}`)
	for _, view := range []string{"home", "project", "preset", "prompt"} {
		page.eval(t, fmt.Sprintf(`openSpawnView(%q)`, view))
		saveUIShot(t, page, "new-chat-"+view)
	}
```

Check what `saveUIShot` (in `diff_ui_test.go:100`) expects for the directory variable and file naming. If it reads `SYZYGY_UI_SHOTS` itself, remove this test's own `os.Getenv` skip and its manual capture code. Then drop any imports (`encoding/base64`, `os`, `path/filepath`) that are no longer used.

- [ ] **Step 2: Capture and compare**

Run: `mkdir -p /tmp/syzygy-shots && SYZYGY_UI_SHOTS=/tmp/syzygy-shots go test -run TestNewChatVisualReview ./...`

Open each PNG with the Read tool and compare it with the matching Figma frame: `New Chat stepper / Home`, `/ Project`, `/ Agent`, `/ Prompt` (node ids 140:1285, 140:1325, 140:1380, 140:1490). Check the step subtitle under the title, the 18px provider icons, the coloured mode word, the text footer buttons (`Start from scratch`, `Next`, `Start`), and that the Prompt textarea fills the height. Fix spacing with CSS only.

- [ ] **Step 3: Run the full suite and commit**

Run: `go test ./... && node --test index_test.mjs`
Expected: all PASS.

```bash
git add index.html new_chat_ui_test.go
git commit -m "test(new-chat): screenshot all four stepper steps for review"
```

---

## After the plan

- Update the spec status line to `Status: implemented` in the same commit as the final task.
- Deployment is out of scope here. It follows the usual acp-mobile path (push the fork, bump the pin in `build-acp-tools.sh`, kick the launchd job) and needs a separate go-ahead.
- Once this ships, delete this plan in a follow-up commit, after moving any lasting notes into the spec.
