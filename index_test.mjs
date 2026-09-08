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

function loadSocketClient(overrides = {}) {
  const html = fs.readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const start = html.indexOf('// --- Socket: connect, reconnect, keepalive ---');
  const end = html.indexOf('// --- Socket: end ---', start);
  assert.notEqual(start, -1, 'socket block should exist');
  assert.notEqual(end, -1, 'socket block should have an end marker');

  let now = 1000;
  let nextID = 1;
  const timers = new Map();
  const nextTimer = () => [...timers.values()].sort((a, b) =>
    a.at - b.at || a.id - b.id)[0];
  const clock = {
    setTimeout(fn, delay = 0) {
      const id = nextID++;
      timers.set(id, {id, at: now + delay, fn});
      return id;
    },
    clearTimeout(id) { timers.delete(id); },
    nextDelay() {
      const next = nextTimer();
      return next ? next.at - now : null;
    },
    pending() {
      return [...timers.values()].map(({id, at}) => ({id, at}));
    },
    async advance(ms) {
      const target = now + ms;
      for (let timer = nextTimer(); timer && timer.at <= target; timer = nextTimer()) {
        now = timer.at;
        timers.delete(timer.id);
        await timer.fn();
      }
      now = target;
    },
  };
  class FakeDate extends Date {
    static now() { return now; }
  }
  const sockets = [];
  class FakeWebSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSING = 2;
    static CLOSED = 3;
    constructor(url) {
      this.url = url;
      this.readyState = FakeWebSocket.CONNECTING;
      this.closeCalls = 0;
      sockets.push(this);
    }
    open() {
      this.readyState = FakeWebSocket.OPEN;
      this.onopen();
    }
    close() {
      this.closeCalls++;
      this.readyState = FakeWebSocket.CLOSING;
      // Tests decide when the browser delivers the close event.
    }
    emitClose() {
      this.readyState = FakeWebSocket.CLOSED;
      this.onclose();
    }
  }
  const statusClasses = [];
  const statusText = {
    textContent: '',
    classList: new FakeClassList(),
    get className() { return statusClasses.at(-1) || ''; },
    set className(value) { statusClasses.push(value); },
  };
  const documentListeners = new Map();
  const windowListeners = new Map();
  const calls = {resetReplayBuffer: 0, buffered: [], handled: []};
  const chatViewEl = new FakeElement('chat');
  chatViewEl.classList.add('visible');
  const context = {
    ws: null,
    messagesEl: new FakeElement('messages'),
    statusText,
    sendBtn: new FakeElement('send'),
    chatViewEl,
    allReplayTurns: [],
    renderedTurnStart: 0,
    reconnectAttempts: 0,
    waitForSessionAttempts: 0,
    reconnectTimer: null,
    disconnectedAt: null,
    silenceTimer: null,
    currentAgentMsg: null,
    currentUserMsg: null,
    lastSentMsg: null,
    replayMode: false,
    replayTimer: null,
    sessionId: null,
    pendingPermissions: [],
    currentSessionKey: null,
    currentSockPid: '1',
    basePath: '',
    location: {protocol: 'http:', host: 'x'},
    document: {
      visibilityState: 'visible',
      addEventListener: (name, fn) => documentListeners.set(name, fn),
    },
    window: {
      addEventListener: (name, fn) => windowListeners.set(name, fn),
    },
    WebSocket: FakeWebSocket,
    __sockets: sockets,
    setTimeout: clock.setTimeout,
    clearTimeout: clock.clearTimeout,
    Date: FakeDate,
    setProcessing: () => {},
    resetThoughtState: () => {},
    resetReplayBuffer: () => { calls.resetReplayBuffer++; },
    closeMsgMenu: () => {},
    closeReader: () => {},
    bufferReplayMessage: msg => calls.buffered.push(msg),
    handleMessage: msg => calls.handled.push(msg),
    flushReplay: () => {},
    showOrrery: () => {},
    sKey: session => session.sessionId,
    fetch: async () => ({ok: true, json: async () => ({sessions: []})}),
    console,
    Math,
    JSON,
    ...overrides,
  };
  vm.createContext(context);
  vm.runInContext(html.slice(start, end), context, {filename: 'index.html#socket'});
  return {context, clock, sockets, statusClasses, calls, documentListeners, windowListeners};
}

test('reconnectDelay starts fast and caps exponential backoff at 60 seconds', () => {
  const {context} = loadSocketClient();
  assert.equal(context.reconnectDelay(0), 400);
  assert.equal(context.reconnectDelay(1), 2000);
  assert.equal(context.reconnectDelay(2), 4000);
  assert.equal(context.reconnectDelay(30), 60000);
});

test('a close followed by an open within two seconds never shows an error', async () => {
  const {context, clock, sockets, statusClasses} = loadSocketClient();
  context.connect('1');
  sockets[0].open();
  sockets[0].emitClose();
  assert.equal(context.statusText.className, 'header-status reconnecting');
  assert.equal(context.statusText.textContent, 'Reconnecting...');

  await clock.advance(400);
  assert.equal(sockets.length, 2);
  sockets[1].open();
  assert.equal(context.statusText.className, 'header-status connected');
  assert.equal(context.statusText.textContent, 'Connected');
  assert.equal(statusClasses.some(value => value.split(/\s+/).includes('error')), false);
});

test('three failed retries promote the status to Disconnected', async () => {
  const {context, clock, sockets} = loadSocketClient();
  context.connect('1');
  sockets[0].open();
  sockets[0].emitClose();

  for (let retry = 1; retry <= 3; retry++) {
    assert.equal(context.statusText.className, 'header-status reconnecting');
    const delay = clock.nextDelay();
    assert.notEqual(delay, null, 'a retry must be scheduled');
    await clock.advance(delay);
    assert.equal(sockets.length, retry + 1);
    sockets[retry].emitClose();
  }
  assert.equal(context.statusText.className, 'header-status error');
  assert.equal(context.statusText.textContent, 'Disconnected');
});

test('reconnect resets replay state without blanking messages before flushReplay', async () => {
  const {context, clock, sockets, calls} = loadSocketClient();
  context.connect('1');
  sockets[0].open();
  context.messagesEl.children.push(new FakeElement(), new FakeElement());
  context.sessionId = 'old-session';
  const resetsBeforeRetry = calls.resetReplayBuffer;
  sockets[0].emitClose();
  assert.equal(context.messagesEl.children.length, 2);

  await clock.advance(400);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(sockets.length, 2);
  assert.equal(context.messagesEl.children.length, 2);
  assert.ok(calls.resetReplayBuffer > resetsBeforeRetry);
  assert.equal(context.sessionId, null);

  context.clearMessages();
  assert.equal(context.messagesEl.children.length, 0);
});

test('keepalive pings bypass both replay buffering and live message handling', () => {
  const {context, sockets, calls} = loadSocketClient();
  context.connect('1');
  sockets[0].open();
  const ping = {data: JSON.stringify({jsonrpc: '2.0', method: 'acp-mobile/ping'})};
  context.replayMode = true;
  sockets[0].onmessage(ping);
  assert.equal(calls.buffered.length, 0);
  assert.equal(calls.handled.length, 0);

  context.replayMode = false;
  sockets[0].onmessage(ping);
  assert.equal(calls.buffered.length, 0);
  assert.equal(calls.handled.length, 0);
  sockets[0].onmessage({data: JSON.stringify({method: 'session/update'})});
  assert.equal(calls.handled.length, 1);
  assert.equal(calls.handled[0].method, 'session/update');
});

test('60 seconds of silence closes an open socket and pings renew the deadline', async () => {
  const silent = loadSocketClient();
  silent.context.connect('1');
  silent.sockets[0].open();
  await silent.clock.advance(59999);
  assert.equal(silent.sockets[0].closeCalls, 0);
  await silent.clock.advance(1);
  assert.equal(silent.sockets[0].closeCalls, 1);

  const active = loadSocketClient();
  active.context.connect('1');
  active.sockets[0].open();
  await active.clock.advance(59000);
  active.sockets[0].onmessage({
    data: JSON.stringify({jsonrpc: '2.0', method: 'acp-mobile/ping'}),
  });
  await active.clock.advance(59000);
  assert.equal(active.sockets[0].closeCalls, 0);
  await active.clock.advance(1001);
  assert.equal(active.sockets[0].closeCalls, 1);
});

test('visibility wake resets closed-socket backoff but leaves open sockets alone', async () => {
  const {context, clock, sockets, documentListeners, windowListeners} = loadSocketClient();
  assert.equal(typeof windowListeners.get('pageshow'), 'function');
  assert.equal(typeof windowListeners.get('online'), 'function');
  const visibilityChanged = documentListeners.get('visibilitychange');
  assert.equal(typeof visibilityChanged, 'function');
  context.connect('1');
  sockets[0].open();
  sockets[0].emitClose();
  assert.equal(context.reconnectAttempts, 1);
  context.reconnectAttempts = 5;
  context.document.visibilityState = 'visible';
  visibilityChanged();
  assert.equal(clock.nextDelay(), 400);
  assert.equal(clock.pending().length, 1);
  assert.equal(context.reconnectAttempts, 1);

  await clock.advance(400);
  assert.equal(sockets.length, 2);
  sockets[1].open();
  const pendingBeforeWake = clock.pending();
  visibilityChanged();
  context.wakeSocket();
  assert.deepEqual(clock.pending(), pendingBeforeWake);
  assert.equal(context.reconnectTimer, null);
  assert.equal(context.reconnectAttempts, 0);
  assert.equal(sockets.length, 2);
});

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

// --- Turn nav: prompt/response jumping ---

function loadTurnNav() {
  const html = fs.readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const start = html.indexOf('// --- Turn nav: jump between prompts and responses ---');
  const end = html.indexOf('// --- Turn nav: end ---', start);
  assert.notEqual(start, -1, 'turn nav block should exist');
  assert.notEqual(end, -1, 'turn nav block should have an end marker');
  const context = { console, Math };
  vm.createContext(context);
  vm.runInContext(html.slice(start, end), context, {filename: 'index.html#turnnav'});
  // Objects built inside the vm have a foreign Object prototype, which
  // deepStrictEqual rejects; round-trip through JSON to normalise.
  const plain = fn => (...args) => JSON.parse(JSON.stringify(fn(...args)));
  return {
    turnNavStops: plain(context.turnNavStops),
    turnNavTarget: plain(context.turnNavTarget),
    turnNavNextMode: context.turnNavNextMode,
    turnNavShouldShow: context.turnNavShouldShow,
    turnNavLandmarks: context.turnNavLandmarks,
    turnNavPrefs: plain(context.turnNavPrefs),
    turnNavPlacement: plain(context.turnNavPlacement),
    turnNavDefaultEnabled: context.turnNavDefaultEnabled,
    turnNavKind: context.turnNavKind,
  };
}

const convo = [
  {kind: 'user', top: 0},
  {kind: 'thought', top: 100},
  {kind: 'tool', top: 150},
  {kind: 'agent', top: 200},
  {kind: 'agent', top: 300},
  {kind: 'user', top: 400},
  {kind: 'agent', top: 500},
  {kind: 'system', top: 600},
];

test('prompt mode stops on every user message', () => {
  const {turnNavStops} = loadTurnNav();
  assert.deepEqual(turnNavStops(convo, 'prompt'), [
    {kind: 'prompt', top: 0}, {kind: 'prompt', top: 400},
  ]);
});

test('response mode stops once per agent turn, at its first agent-side block', () => {
  const {turnNavStops} = loadTurnNav();
  assert.deepEqual(turnNavStops(convo, 'response'), [
    {kind: 'response', top: 100}, {kind: 'response', top: 500},
  ]);
});

test('a conversation that opens with the agent counts that as a response', () => {
  const {turnNavStops} = loadTurnNav();
  assert.deepEqual(turnNavStops([{kind: 'agent', top: 0}, {kind: 'user', top: 50}], 'response'),
    [{kind: 'response', top: 0}]);
});

test('alternate mode interleaves prompts and responses in document order', () => {
  const {turnNavStops} = loadTurnNav();
  assert.deepEqual(turnNavStops(convo, 'alternate').map(s => s.kind + '@' + s.top),
    ['prompt@0', 'response@100', 'prompt@400', 'response@500']);
});

test('down picks the nearest stop below the viewport top, ignoring the one under it', () => {
  const {turnNavStops, turnNavTarget} = loadTurnNav();
  const stops = turnNavStops(convo, 'alternate');
  assert.deepEqual(turnNavTarget(stops, 100, 1), {kind: 'prompt', top: 400});
  assert.deepEqual(turnNavTarget(stops, 104, 1), {kind: 'prompt', top: 400});
  assert.deepEqual(turnNavTarget(stops, 500, 1), null);
});

test('up picks the nearest stop above the viewport top, ignoring the one under it', () => {
  const {turnNavStops, turnNavTarget} = loadTurnNav();
  const stops = turnNavStops(convo, 'alternate');
  assert.deepEqual(turnNavTarget(stops, 400, -1), {kind: 'response', top: 100});
  assert.deepEqual(turnNavTarget(stops, 396, -1), {kind: 'response', top: 100});
  assert.deepEqual(turnNavTarget(stops, 0, -1), null);
});

test('mode cycles prompt, response, alternate, prompt', () => {
  const {turnNavNextMode} = loadTurnNav();
  assert.equal(turnNavNextMode('prompt'), 'response');
  assert.equal(turnNavNextMode('response'), 'alternate');
  assert.equal(turnNavNextMode('alternate'), 'prompt');
  assert.equal(turnNavNextMode('garbage'), 'prompt');
});

test('turn nav only shows when enabled for this chat and there is somewhere to go', () => {
  const {turnNavShouldShow} = loadTurnNav();
  assert.equal(turnNavShouldShow(false, 5), false);
  assert.equal(turnNavShouldShow(true, 1), false);
  assert.equal(turnNavShouldShow(true, 2), true);
});

test('turn nav landmark count ignores the current mode: one prompt plus one response is somewhere to go', () => {
  const {turnNavLandmarks} = loadTurnNav();
  const onePrompt = [{kind: 'user', top: 0}, {kind: 'agent', top: 40}, {kind: 'agent', top: 90}];
  assert.equal(turnNavLandmarks(onePrompt), 2);
  assert.equal(turnNavLandmarks([{kind: 'user', top: 0}]), 1);
  assert.equal(turnNavLandmarks([{kind: 'agent', top: 0}]), 1);
  assert.equal(turnNavLandmarks([]), 0);
});

test('turn nav prefs default to bottom-right and fall back per axis on garbage', () => {
  const {turnNavPrefs} = loadTurnNav();
  assert.deepEqual(turnNavPrefs(null, null), {vertical: 'bottom', horizontal: 'right'});
  assert.deepEqual(turnNavPrefs('top', 'left'), {vertical: 'top', horizontal: 'left'});
  assert.deepEqual(turnNavPrefs('sideways', 'left'), {vertical: 'bottom', horizontal: 'left'});
  assert.deepEqual(turnNavPrefs('top', 'middle'), {vertical: 'top', horizontal: 'right'});
});

test('turn nav placement: bottom floats above the scroll button, top hugs the chat header', () => {
  const {turnNavPlacement} = loadTurnNav();
  // clear = px of chrome to stay above at the bottom (composer + scroll
  // button in a chat, the search dock in History); header = px at the top.
  const layout = {clear: 148, header: 60};
  assert.deepEqual(turnNavPlacement({vertical: 'bottom', horizontal: 'right'}, layout),
    {top: 'auto', bottom: '148px', left: 'auto', right: '12px'});
  assert.deepEqual(turnNavPlacement({vertical: 'top', horizontal: 'left'}, layout),
    {top: '72px', bottom: 'auto', left: '12px', right: 'auto'});
});

test('turn nav is on by default in a History transcript, off in a live chat', () => {
  const {turnNavDefaultEnabled} = loadTurnNav();
  assert.equal(turnNavDefaultEnabled('history'), true);
  assert.equal(turnNavDefaultEnabled('chat'), false);
});

test('transcript bubbles classify like chat bubbles', () => {
  const {turnNavKind} = loadTurnNav();
  assert.equal(turnNavKind(['pv-msg', 'user']), 'user');
  assert.equal(turnNavKind(['pv-msg', 'agent']), 'agent');
  assert.equal(turnNavKind(['msg', 'system']), 'system');
  assert.equal(turnNavKind(['msg', 'tool']), 'agent');
});
