import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

class FakeClassList {
  constructor() { this.values = new Set(); }
  add(...names) { names.forEach(name => this.values.add(name)); }
  remove(...names) { names.forEach(name => this.values.delete(name)); }
  contains(name) { return this.values.has(name); }
}

class FakeElement {
  constructor(id = '') {
    this.id = id;
    this.classList = new FakeClassList();
    this.style = {};
    this.children = [];
    this.disabled = false;
    this.textContent = '';
    this.title = '';
    this.scrollTop = 0;
    this.listeners = new Map();
    this._innerHTML = '';
  }
  set innerHTML(value) { this._innerHTML = value; this.children = []; }
  get innerHTML() { return this._innerHTML; }
  appendChild(child) { this.children.push(child); return child; }
  addEventListener(name, fn) { this.listeners.set(name, fn); }
  blur() {}
  focus() {}
  querySelector() { return null; }
  querySelectorAll() { return []; }
}

function loadHistoryClient(overrides = {}) {
  const html = fs.readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const start = html.indexOf('// --- History: browse agent-recall transcripts ---');
  const end = html.indexOf("document.getElementById('nav-pins-btn')", start);
  assert.notEqual(start, -1, 'history client block should exist');
  assert.notEqual(end, -1, 'history client block should have an end marker');
  const source = html.slice(start, end);

  const elements = new Map();
  const element = id => {
    if (!elements.has(id)) elements.set(id, new FakeElement(id));
    return elements.get(id);
  };
  const context = {
    console,
    document: {
      getElementById: element,
      createElement: () => new FakeElement(),
    },
    basePath: '',
    orreryEl: new FakeElement('orrery'),
    pinsEl: new FakeElement('pins'),
    spSheet: new FakeElement('sp-sheet'),
    pvSheet: new FakeElement('pv-sheet'),
    requestAnimationFrame: fn => { fn(); return 1; },
    escHtml: value => String(value),
    renderAgentMarkdown: value => String(value),
    providerIcon: () => '',
    renderMarkdown: value => String(value),
    lastSessions: [],
    loadSessions: async () => {},
    selectSession: () => {},
    fetch: async () => { throw new Error('unexpected fetch'); },
    alert: () => {},
    setTimeout: fn => { fn(); return 1; },
    clearTimeout: () => {},
    Date,
    JSON,
    Promise,
    ...overrides,
  };
  vm.createContext(context);
  vm.runInContext(source, context, {filename: 'index.html#history'});
  return {context, elements};
}

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return {promise, resolve};
}

test('resume waits for and selects the exact restored buffer', async () => {
  const target = {
    pid: 42,
    bufferName: '*Claude Agent @ demo*',
    sessionId: 'resumed-live-id',
  };
  let selected = null;
  let polls = 0;
  const requestBodies = [];
  const {context, elements} = loadHistoryClient({
    fetch: async (url, options) => {
      assert.equal(url, '/api/resume-transcript');
      requestBodies.push(JSON.parse(options.body));
      return {
        ok: true,
        text: async () => JSON.stringify({
          ok: true,
          bufferName: target.bufferName,
          sessionId: 'archived-session-id',
          existing: false,
        }),
      };
    },
    loadSessions: async () => {
      polls += 1;
      context.lastSessions = polls === 1
        ? [{pid: 7, bufferName: '*Unrelated fresh session*'}]
        : [target, {pid: 7, bufferName: '*Unrelated fresh session*'}];
    },
    selectSession: session => { selected = session; },
  });
  const transcript = {
    file: '/Users/demo/.agent-shell/transcripts/old.md',
    resumable: true,
    resumeReason: '',
  };
  const history = elements.get('history');
  history.classList.add('visible');

  context.setHistoryView('Claude · demo', () => {}, true, transcript);
  await context.resumeTranscript(transcript);

  assert.deepEqual(requestBodies, [{file: transcript.file}]);
  assert.equal(polls, 2);
  assert.equal(selected?.bufferName, target.bufferName);
  assert.equal(history.classList.contains('visible'), false);
});

test('transcript view disables resume with the server-provided reason', () => {
  const {context, elements} = loadHistoryClient();
  const transcript = {
    file: '/Users/demo/.agent-shell/transcripts/unsupported.md',
    resumable: false,
    resumeReason: 'No matching agent config is available.',
  };

  context.setHistoryView('DeepSeek · demo', () => {}, true, transcript);

  const button = elements.get('history-resume');
  assert.equal(button.style.display, '');
  assert.equal(button.disabled, true);
  assert.equal(button.title, transcript.resumeReason);
});

test('closing History during the resume request prevents stale selection', async () => {
  const response = deferred();
  const target = {pid: 42, bufferName: '*Claude Agent @ demo*'};
  let selected = null;
  const {context, elements} = loadHistoryClient({
    lastSessions: [target],
    fetch: async () => ({ok: true, text: () => response.promise}),
    selectSession: session => { selected = session; },
  });
  const transcript = {
    file: '/Users/demo/.agent-shell/transcripts/old.md',
    resumable: true,
  };
  elements.get('history').classList.add('visible');
  context.setHistoryView('Claude · demo', () => {}, true, transcript);

  const resume = context.resumeTranscript(transcript);
  await Promise.resolve();
  context.closeHistory();
  response.resolve(JSON.stringify({
    ok: true,
    bufferName: target.bufferName,
    sessionId: 'archived-session-id',
  }));
  await resume;

  assert.equal(selected, null);
  assert.equal(elements.get('history').classList.contains('visible'), false);
});

test('a stale history-list response cannot overwrite a newer transcript view', async () => {
  const oldList = deferred();
  const transcript = {
    file: '/Users/demo/.agent-shell/transcripts/current.md',
    agent: 'Claude',
    project: 'demo',
    timestamp: '2026-08-27-12-00-00',
    resumable: true,
  };
  const {context, elements} = loadHistoryClient({
    fetch: async url => {
      if (url === '/api/transcripts') return {ok: true, json: () => oldList.promise};
      return {
        ok: true,
        json: async () => ({content: '\n---\n## User (now)\n\ncurrent transcript'}),
      };
    },
  });

  const listLoad = context.loadHistory();
  await context.openTranscript(transcript);
  oldList.resolve([{
    file: '/Users/demo/.agent-shell/transcripts/stale.md',
    agent: 'Codex', project: 'stale', preview: 'stale list item', timestamp: '',
  }]);
  await listLoad;

  const rendered = elements.get('history-body').children;
  assert.equal(elements.get('history-title').textContent.startsWith('Claude · demo'), true);
  assert.equal(rendered.some(child => child.className === 'pv-msg user' &&
    child.textContent === 'current transcript'), true);
  assert.equal(rendered.some(child => child.className === 'hist-card'), false);
});

test('an older transcript response cannot overwrite a newer transcript', async () => {
  const firstResponse = deferred();
  const first = {
    file: '/Users/demo/.agent-shell/transcripts/first.md',
    agent: 'Claude', project: 'first', timestamp: '', resumable: true,
  };
  const second = {
    file: '/Users/demo/.agent-shell/transcripts/second.md',
    agent: 'Codex', project: 'second', timestamp: '', resumable: true,
  };
  const {context, elements} = loadHistoryClient({
    fetch: async url => ({
      ok: true,
      json: () => url.includes('first.md')
        ? firstResponse.promise
        : Promise.resolve({content: '\n---\n## User (now)\n\nsecond transcript'}),
    }),
  });

  const olderLoad = context.openTranscript(first);
  await Promise.resolve();
  await context.openTranscript(second);
  firstResponse.resolve({content: '\n---\n## User (now)\n\nfirst transcript'});
  await olderLoad;

  const rendered = elements.get('history-body').children;
  assert.equal(elements.get('history-title').textContent.startsWith('Codex · second'), true);
  assert.equal(rendered.some(child => child.className === 'pv-msg user' &&
    child.textContent === 'second transcript'), true);
  assert.equal(rendered.some(child => child.textContent === 'first transcript'), false);
});
