import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

class FakeClassList {
  constructor() { this.values = new Set(); }
  add(...names) { names.forEach(name => this.values.add(name)); }
  remove(...names) { names.forEach(name => this.values.delete(name)); }
  contains(name) { return this.values.has(name); }
  toggle(name, force) {
    const on = force === undefined ? !this.values.has(name) : !!force;
    if (on) this.values.add(name); else this.values.delete(name);
    return on;
  }
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
  get firstChild() { return this.children[0] || null; }
  set className(value) {
    this.classList.values = new Set(String(value).split(/\s+/).filter(Boolean));
  }
  get className() { return [...this.classList.values].join(' '); }
  addEventListener(name, fn) { this.listeners.set(name, fn); }
  setAttribute(name, value) { this.attributes = {...(this.attributes || {}), [name]: value}; }
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
    reconnectInFlight: false,
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

test('a wake during an in-flight retry does not dial a second socket', async () => {
  // The retry callback awaits the session lookup before dialing. Safari
  // fires visibilitychange, pageshow and online together on resume,
  // right inside that window; none of them may stack another dial.
  let releaseLookup;
  const lookup = new Promise(resolve => { releaseLookup = resolve; });
  const {context, clock, sockets, documentListeners, windowListeners} = loadSocketClient({
    currentSessionKey: 'buffer-a',
    sKey: session => session.bufferName,
    fetch: async () => { await lookup; return {ok: true, json: async () => ({sessions: [{bufferName: 'buffer-a', pid: 1}]})}; },
  });
  context.connect('1');
  sockets[0].open();
  sockets[0].emitClose();
  // The clock awaits the retry callback, which is parked on the lookup;
  // do not await it yet, just let the callback run up to the fetch.
  const advancing = clock.advance(400);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(context.reconnectInFlight, true);
  assert.equal(sockets.length, 1);

  documentListeners.get('visibilitychange')();
  windowListeners.get('pageshow')();
  windowListeners.get('online')();
  assert.equal(clock.pending().length, 0, 'no second retry timer while a redial is in flight');

  releaseLookup();
  await advancing;
  assert.equal(sockets.length, 2, 'exactly one replacement socket');
  assert.equal(context.reconnectInFlight, false);
});

test('connect drops a still-connecting socket before dialing a new one', () => {
  const {context, sockets} = loadSocketClient();
  context.connect('1');
  context.connect('1');
  assert.equal(sockets.length, 2);
  assert.equal(sockets[0].closeCalls, 1);
  assert.equal(sockets[0].onclose, null);
  assert.equal(sockets[1].closeCalls, 0);
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

// --- Model picker ---

function loadModelPicker(overrides = {}) {
  const html = fs.readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const start = html.indexOf('// --- Model picker ---');
  const end = html.indexOf('// --- Model picker: end ---', start);
  assert.notEqual(start, -1, 'model picker block should exist');
  assert.notEqual(end, -1, 'model picker block should have an end marker');
  const elements = new Map();
  const element = id => {
    if (!elements.has(id)) elements.set(id, new FakeElement(id));
    return elements.get(id);
  };
  const timers = [];
  const context = {
    document: {
      getElementById: element,
      createElement: () => new FakeElement(),
    },
    basePath: '/phone',
    currentBufferName: '*Agent @ demo*',
    closeChatMenu: () => {},
    fetch: async () => { throw new Error('unexpected fetch'); },
    setTimeout: (fn, delay) => { timers.push({fn, delay}); return timers.length; },
    Array, Error, JSON, Promise,
    ...overrides,
  };
  vm.createContext(context);
  vm.runInContext(html.slice(start, end), context, {filename: 'index.html#models'});
  return {context, elements, timers};
}

const modelData = () => ({current: 'first', models: [
  {id: 'first', name: 'First', description: 'Fast model'},
  {id: 'second', name: 'Second', description: ''},
]});
const modelReply = data => ({ok: true, json: async () => data});

test('model picker lists models in rig order with the current model marked', async () => {
  let menuClosed = false;
  const {context, elements} = loadModelPicker({
    closeChatMenu: () => { menuClosed = true; },
    fetch: async (url, options) => {
      assert.equal(url, '/phone/api/models');
      assert.equal(options.method, 'POST');
      assert.equal(options.headers['Content-Type'], 'application/json');
      assert.deepEqual(JSON.parse(options.body), {bufferName: '*Agent @ demo*'});
      return modelReply(modelData());
    },
  });
  await context.openModelPicker();
  const rows = elements.get('md-list').children;
  assert.equal(menuClosed, true);
  assert.deepEqual(rows.map(row => row.textContent), ['First (current)', 'Second']);
  assert.equal(rows[0].children[0].textContent, 'Fast model');
  assert.equal(rows[0].classList.contains('md-row'), true);
  assert.equal(rows[0].classList.contains('sel'), true);
  assert.equal(rows[1].classList.contains('sel'), false);
  assert.equal(elements.get('model-sheet').classList.contains('visible'), true);
});

test('choosing a model posts the exact body, re-marks, and closes after a delay', async () => {
  const requests = [];
  const {context, elements, timers} = loadModelPicker({
    fetch: async (url, options) => {
      requests.push({url, options});
      return modelReply(url.endsWith('/api/models') ? modelData() : {ok: true, current: 'second'});
    },
  });
  await context.openModelPicker();
  await elements.get('md-list').children[1].onclick();
  assert.equal(requests.length, 2);
  assert.equal(requests[1].url, '/phone/api/model');
  assert.equal(requests[1].options.method, 'POST');
  assert.equal(requests[1].options.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(requests[1].options.body), {
    bufferName: '*Agent @ demo*', modelId: 'second',
  });
  const rows = elements.get('md-list').children;
  assert.equal(rows[0].classList.contains('sel'), false);
  assert.equal(rows[1].classList.contains('sel'), true);
  assert.equal(elements.get('model-sheet').classList.contains('visible'), true);
  assert.equal(timers.length, 1);
  assert.equal(timers[0].delay, 300);
  timers[0].fn();
  assert.equal(elements.get('model-sheet').classList.contains('visible'), false);
  assert.equal(elements.get('model-scrim').classList.contains('visible'), false);
});

test('a stale model list cannot overwrite a newer open', async () => {
  const first = deferred();
  let calls = 0;
  const {context, elements} = loadModelPicker({
    fetch: async () => ++calls === 1 ? first.promise : modelReply({
      current: 'new', models: [{id: 'new', name: 'New', description: ''}],
    }),
  });
  const stale = context.openModelPicker();
  await context.openModelPicker();
  first.resolve(modelReply(modelData()));
  await stale;
  assert.deepEqual(elements.get('md-list').children.map(row => row.textContent), ['New (current)']);
});

test('model switch errors stay inline, preserve selection, and allow retry', async () => {
  for (const httpOK of [true, false]) {
    const {context, elements, timers} = loadModelPicker({
      fetch: async url => url.endsWith('/api/models') ? modelReply(modelData()) : {
        ok: httpOK, status: httpOK ? 200 : 409,
        json: async () => ({ok: false, error: 'Agent refused'}),
      },
    });
    await context.openModelPicker();
    await context.chooseModel('second');
    const rows = elements.get('md-list').children;
    assert.equal(rows[0].classList.contains('sel'), true);
    assert.equal(rows[1].classList.contains('sel'), false);
    assert.equal(rows[1].disabled, false);
    assert.equal(rows[2].textContent, 'Model switch failed: Agent refused');
    assert.equal(elements.get('model-sheet').classList.contains('visible'), true);
    assert.equal(timers.length, 0);
  }
});

test('model list failures show a message in the open sheet', async () => {
  for (const fetch of [
    async () => ({ok: false, status: 404}),
    async () => { throw new Error('offline'); },
  ]) {
    const {context, elements} = loadModelPicker({fetch});
    await context.openModelPicker();
    const messages = elements.get('md-list').children;
    assert.equal(messages.length, 1);
    assert.match(messages[0].textContent, /^Models unavailable: (No such session\.|offline)$/);
    assert.equal(elements.get('model-sheet').classList.contains('visible'), true);
  }
});

test('a stale switch reply cannot re-mark or close a newer picker', async () => {
  const pending = deferred();
  let writes = 0;
  const {context, elements, timers} = loadModelPicker({
    fetch: async url => {
      if (url.endsWith('/api/models')) return modelReply(modelData());
      writes++;
      return pending.promise;
    },
  });
  await context.openModelPicker();
  const stale = context.chooseModel('second');
  await context.chooseModel('first');
  assert.equal(writes, 1, 'duplicate choices are blocked while switching');
  await context.openModelPicker();
  pending.resolve(modelReply({ok: true, current: 'second'}));
  await stale;
  assert.equal(elements.get('md-list').children[0].classList.contains('sel'), true);
  assert.equal(elements.get('model-sheet').classList.contains('visible'), true);
  assert.equal(timers.length, 0);
});

test('old close timers and replies cannot affect a reopened or closed picker', async () => {
  const {context, elements, timers} = loadModelPicker({
    fetch: async url => modelReply(url.endsWith('/api/models') ? modelData() : {ok: true, current: 'second'}),
  });
  await context.openModelPicker();
  await context.chooseModel('second');
  await context.openModelPicker();
  timers[0].fn();
  assert.equal(elements.get('model-sheet').classList.contains('visible'), true);
  for (const control of ['md-close', 'model-scrim']) {
    const pending = deferred();
    context.fetch = () => pending.promise;
    const loading = context.openModelPicker();
    elements.get(control).listeners.get('click')();
    pending.resolve(modelReply(modelData()));
    await loading;
    assert.equal(elements.get('model-sheet').classList.contains('visible'), false);
    assert.equal(elements.get('model-scrim').classList.contains('visible'), false);
    assert.equal(elements.get('md-list').children[0].textContent, 'Loading models...');
  }
});

// --- Spawn sheet ---

function loadSpawnSheet(overrides = {}) {
  const html = fs.readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const start = html.indexOf('// --- Spawn sheet ---');
  const end = html.indexOf('// --- Spawn sheet: end ---', start);
  assert.notEqual(start, -1, 'spawn sheet block should exist');
  assert.notEqual(end, -1, 'spawn sheet block should have an end marker');
  const source = html.slice(start, end);

  const elements = new Map();
  const element = id => {
    if (!elements.has(id)) elements.set(id, new FakeElement(id));
    return elements.get(id);
  };
  const context = {
    console: {warn: () => {}},
    document: {
      getElementById: element,
      createElement: () => new FakeElement(),
    },
    basePath: '',
    lastSessions: [],
    shortPath: value => String(value),
    sKey: session => session.bufferName,
    syncHistoryDock: () => {},
    loadSessions: async () => {},
    selectSession: () => {},
    closeChatMenu: () => {},
    currentBufferName: null,
    fetch: async () => { throw new Error('unexpected fetch'); },
    alert: () => {},
    setTimeout: fn => { fn(); return 1; },
    clearTimeout: () => {},
    setInterval: () => 1,
    clearInterval: () => {},
    Array,
    Set,
    Error,
    JSON,
    Promise,
    ...overrides,
  };
  vm.createContext(context);
  vm.runInContext(source, context, {filename: 'index.html#spawn'});
  // Top-level let/const stay in the script scope, not on the context.
  const get = name => vm.runInContext(name, context);
  const set = (name, value) => vm.runInContext(`${name} = ${JSON.stringify(value)}`, context);
  return {context, elements, get, set};
}

const presetsReply = presets => async (url, options) => {
  assert.equal(url, '/api/presets');
  assert.equal(options.method, 'POST');
  return {ok: true, json: async () => ({presets})};
};

const chipLabels = el => el.children.map(chip => chip.textContent);

test('spawn sheet shows the rig presets after default, in rig order', async () => {
  const {context, elements, get} = loadSpawnSheet({
    fetch: presetsReply([
      {key: 'f', label: 'Fable 5.1 \u00b7 Bypass', model: 'fable[1m]', mode: 'bypassPermissions'},
      {key: 'F', label: 'Fable 5 \u00b7 Bypass', model: 'claude-fable-5[1m]', mode: 'bypassPermissions'},
      {key: 'a', label: 'Astra \u00b7 Full', model: 'gpt-6-astra', mode: 'agent-full-access'},
    ]),
  });
  context.openSpawnSheet(null);
  const presets = elements.get('sp-presets');
  assert.deepEqual(chipLabels(presets), ['default']);
  await context.loadSpawnPresets();
  assert.deepEqual(chipLabels(presets), [
    'default', 'Fable 5.1 \u00b7 Bypass', 'Fable 5 \u00b7 Bypass', 'Astra \u00b7 Full',
  ]);
  assert.equal(presets.children[0].classList.contains('sel'), true);
  presets.children[2].listeners.get('click')();
  assert.equal(get('spPreset'), 'F');
  assert.equal(presets.children[2].classList.contains('sel'), true);
  assert.equal(presets.children[0].classList.contains('sel'), false);
});

test('spawn sheet keeps the last good presets when the rig is unreachable', async () => {
  let fail = false;
  const {context, elements} = loadSpawnSheet({
    fetch: async (url, options) => {
      if (fail) throw new Error('offline');
      return presetsReply([{key: 'o', label: 'Opus \u00b7 Bypass', model: 'opus', mode: 'bypassPermissions'}])(url, options);
    },
  });
  context.openSpawnSheet(null);
  await context.loadSpawnPresets();
  const presets = elements.get('sp-presets');
  assert.deepEqual(chipLabels(presets), ['default', 'Opus \u00b7 Bypass']);
  fail = true;
  context.openSpawnSheet(null);
  await context.loadSpawnPresets();
  assert.deepEqual(chipLabels(presets), ['default', 'Opus \u00b7 Bypass']);
});

test('a preset reply from an older open cannot overwrite a newer one', async () => {
  const first = deferred();
  const second = deferred();
  let calls = 0;
  const {context, elements} = loadSpawnSheet({
    fetch: async url => {
      if (url !== '/api/presets') return {ok: true, json: async () => ({projects: []})};
      calls += 1;
      const presets = await (calls === 1 ? first.promise : second.promise);
      return {ok: true, json: async () => ({presets})};
    },
  });
  // openSpawnSheet itself starts a load: the first open's fetch is the
  // stale one, the second open's fetch is the fresh one.
  context.openSpawnSheet(null);
  context.openSpawnSheet(null);
  assert.equal(calls, 2);
  second.resolve([{key: 'h', label: 'Haiku \u00b7 Auto', model: 'haiku', mode: 'auto'}]);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(chipLabels(elements.get('sp-presets')), ['default', 'Haiku \u00b7 Auto']);
  first.resolve([{key: 'x', label: 'stale', model: 'x', mode: 'x'}]);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(chipLabels(elements.get('sp-presets')), ['default', 'Haiku \u00b7 Auto']);
});

test('spawn sheet drops malformed preset entries and sends only the key', async () => {
  const bodies = [];
  const {context, get, set} = loadSpawnSheet({
    fetch: async (url, options) => {
      if (url === '/api/presets') {
        return {ok: true, json: async () => ({presets: [
          {key: 'ff', label: 'two chars'},
          null,
          {label: 'no key'},
          {key: 's', label: 'Sonnet \u00b7 Accept', model: 'sonnet', mode: 'acceptEdits'},
        ]})};
      }
      bodies.push(JSON.parse(options.body));
      return {json: async () => ({ok: true})};
    },
    loadSessions: async () => {},
  });
  context.openSpawnSheet(null);
  await context.loadSpawnPresets();
  assert.deepEqual(get('spawnPresets').map(p => p.key), ['s']);
  set('spPreset', 's');
  set('SPAWN_POLL_MS', 0);
  await context.spawnAndOpen({cwd: '/tmp/x', name: '', task: '', preset: get('spPreset')});
  assert.deepEqual(bodies, [{cwd: '/tmp/x', name: '', task: '', preset: 's'}]);
});

const rowPaths = el => el.children.map(row =>
  (row.children.find(c => c.className === 'sp-row-path') || row).textContent);
const rowFlags = el => el.children.map(row =>
  ['live', 'sel'].filter(flag => row.classList.contains(flag)).join(' '));
const tap = (row, dx = 0, dy = 0) => {
  row.listeners.get('pointerdown')({pointerType: 'touch', clientX: 10, clientY: 10, preventDefault() {}});
  row.listeners.get('pointerup')({pointerType: 'touch', clientX: 10 + dx, clientY: 10 + dy});
};

const projectsReply = async url => {
  if (url === '/api/projects') {
    return {ok: true, json: async () => ({projects: [
      {name: 'dotfiles', path: '/home/.dotfiles', live: false},
      {name: 'sandbox', path: '/home/.emacs-sandbox', live: false},
      {name: 'mobile', path: '/work/mobile', live: true},
    ]})};
  }
  return {ok: true, json: async () => ({presets: []})};
};

test('project combobox lists live first and the typed path filters by name, path, or ~ form', async () => {
  const {context, elements} = loadSpawnSheet({
    fetch: projectsReply,
    shortPath: value => String(value).replace('/home', '~'),
  });
  context.openSpawnSheet('/work/mobile');
  await context.loadSpawnProjects();
  const projects = elements.get('sp-projects');
  const dir = elements.get('sp-dir');
  assert.deepEqual(rowPaths(projects), ['/work/mobile', '~/.dotfiles', '~/.emacs-sandbox']);
  assert.deepEqual(rowFlags(projects), ['live sel', '', '']);
  // The name shows only when it is not already the path's base name.
  assert.equal(projects.children[2].children.some(c => c.className === 'sp-row-name' && c.textContent === 'sandbox'), true);
  assert.equal(projects.children[1].children.some(c => c.className === 'sp-row-name'), false);
  dir.value = 'sandbox';
  dir.listeners.get('input')();
  assert.deepEqual(rowPaths(projects), ['~/.emacs-sandbox']);
  dir.value = '~/.dot';
  dir.listeners.get('input')();
  assert.deepEqual(rowPaths(projects), ['~/.dotfiles']);
  dir.value = 'nothing-here';
  dir.listeners.get('input')();
  assert.deepEqual(rowPaths(projects), ['no match; spawns in the path as typed']);
  assert.equal(dir.value, 'nothing-here');
});

test('picking a row fills the path box, collapses the list, and reopening shows the whole list with that row marked', async () => {
  const {context, elements} = loadSpawnSheet({fetch: projectsReply});
  context.openSpawnSheet(null);
  await context.loadSpawnProjects();
  const projects = elements.get('sp-projects');
  const dir = elements.get('sp-dir');
  let blurred = 0;
  dir.blur = () => { blurred += 1; };
  assert.equal(projects.classList.contains('open'), false, 'closed until the box is focused');
  dir.listeners.get('focus')();
  assert.equal(projects.classList.contains('open'), true);
  dir.value = 'dot';
  dir.listeners.get('input')();
  assert.deepEqual(rowPaths(projects), ['/home/.dotfiles']);
  projects.children[0].listeners.get('click')();
  assert.equal(dir.value, '/home/.dotfiles');
  assert.equal(projects.classList.contains('open'), false, 'a pick collapses the list');
  assert.equal(blurred, 1, 'a pick drops the keyboard');
  dir.listeners.get('focus')();
  assert.equal(projects.classList.contains('open'), true);
  dir.listeners.get('blur')();
  assert.equal(projects.classList.contains('open'), false, 'leaving the box closes the list');
  assert.deepEqual(rowPaths(projects), ['/work/mobile', '/home/.dotfiles', '/home/.emacs-sandbox']);
  assert.deepEqual(rowFlags(projects), ['live', 'sel', '']);
});

test('a touch tap picks on pointerup without a click, a drag does not pick, the synthetic click is ignored', async () => {
  let now = 1000;
  class FakeDate extends Date { static now() { return now; } }
  const {context, elements} = loadSpawnSheet({fetch: projectsReply, Date: FakeDate});
  context.openSpawnSheet(null);
  await context.loadSpawnProjects();
  const projects = elements.get('sp-projects');
  const dir = elements.get('sp-dir');
  let prevented = 0;
  const row = projects.children[1];
  row.listeners.get('pointerdown')({pointerType: 'touch', clientX: 0, clientY: 0, preventDefault() { prevented += 1; }});
  row.listeners.get('pointerup')({pointerType: 'touch', clientX: 0, clientY: 40});
  assert.equal(prevented, 1, 'touch pointerdown must cancel the focus change');
  assert.equal(dir.value, '', 'a 40px drag is a scroll, not a pick');
  tap(projects.children[1]);
  assert.equal(dir.value, '/home/.dotfiles');
  // The browser follows a touch with a click. The pick re-rendered the
  // list, so that click lands on a fresh element, maybe another row: it
  // must not pick anything.
  dir.value = 'x';
  projects.children[0].listeners.get('click')();
  assert.equal(dir.value, 'x');
  // A mouse pointerdown only guards focus (the list closes on blur);
  // the click does the pick once the touch suppression window is over.
  now += 1000;
  const mouseRow = projects.children[2];
  mouseRow.listeners.get('pointerdown')({pointerType: 'mouse', preventDefault() { prevented += 1; }});
  assert.equal(prevented, 2);
  assert.equal(dir.value, 'x', 'a mouse pointerdown must not pick');
  mouseRow.listeners.get('click')();
  assert.equal(dir.value, '/home/.emacs-sandbox');
});

test('a stale project reply cannot overwrite a newer spawn-sheet open', async () => {
  const replies = [deferred(), deferred()];
  let projectCalls = 0;
  const {context, elements} = loadSpawnSheet({
    fetch: async url => {
      if (url === '/api/projects') return {ok: true, json: async () => ({projects: await replies[projectCalls++].promise})};
      return {ok: true, json: async () => ({presets: []})};
    },
  });
  context.openSpawnSheet(null);
  context.openSpawnSheet(null);
  replies[1].resolve([{name: 'fresh', path: '/fresh', live: false}]);
  await new Promise(resolve => setImmediate(resolve));
  replies[0].resolve([{name: 'stale', path: '/stale', live: false}]);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(rowPaths(elements.get('sp-projects')), ['/fresh']);
});

// --- Catalogue: #tag grammar, the Catalogued chip, the transcript-view button ---
function loadCatalogueHistoryClient(overrides = {}) {
  const client = loadHistoryClient(overrides);
  const {context, elements} = client;
  Object.defineProperty(context, 'historyCataloguedOnly', {
    get: () => vm.runInContext('historyCataloguedOnly', context),
    set: value => vm.runInContext(
      `historyCataloguedOnly = ${Boolean(value)}`, context),
  });
  const classes = elements.get('history-catalogue').classList;
  classes.toggle = (name, force = !classes.contains(name)) => {
    if (force) classes.add(name);
    else classes.remove(name);
    return force;
  };
  return client;
}

test('parseHistoryQuery separates text, normalizes tags and recognizes bare hashes', () => {
  const {context} = loadHistoryClient();
  const cases = [
    ['fix #Syzygy resume #phone', {
      text: 'fix resume', tags: ['syzygy', 'phone'], catalogued: false,
    }],
    ['#', {text: '', tags: [], catalogued: true}],
    ['#syzygy #syzygy', {
      text: '', tags: ['syzygy'], catalogued: false,
    }],
    ['  ', {text: '', tags: [], catalogued: false}],
    ['##x', {text: '', tags: ['x'], catalogued: false}],
  ];

  for (const [raw, expected] of cases) {
    assert.deepEqual(
      JSON.parse(JSON.stringify(context.parseHistoryQuery(raw))),
      expected,
      JSON.stringify(raw),
    );
  }
});

test('splitTags normalizes comma-separated and whitespace-separated tags and dedupes', () => {
  const {context} = loadHistoryClient();

  assert.deepEqual(
    Array.from(context.splitTags('syzygy, Resume,#phone  #resume')),
    ['syzygy', 'resume', 'phone'],
  );
});

test('historySearchBody merges the chip with the bare-hash filter', () => {
  const {context} = loadCatalogueHistoryClient();

  context.historyCataloguedOnly = true;
  assert.deepEqual(
    JSON.parse(JSON.stringify(context.historySearchBody('hello'))),
    {query: 'hello', catalogued: true, tags: []},
  );

  context.historyCataloguedOnly = false;
  assert.deepEqual(
    JSON.parse(JSON.stringify(context.historySearchBody('# hello'))),
    {query: 'hello', catalogued: true, tags: []},
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(context.historySearchBody('hello'))),
    {query: 'hello', catalogued: false, tags: []},
  );
});

test('catalogued chip toggles state and aria-pressed and switches empty-box requests', async () => {
  const requests = [];
  const {context, elements} = loadCatalogueHistoryClient({
    fetch: async (url, options) => {
      requests.push({
        url,
        method: options.method,
        body: options.body === undefined ? null : JSON.parse(options.body),
      });
      return {
        ok: true,
        json: async () => url === '/api/transcripts'
          ? []
          : {results: [], truncated: false},
      };
    },
  });
  const chip = elements.get('history-catalogued-chip');
  elements.get('history-search-input').value = '';
  assert.equal(context.historyCataloguedOnly, false);

  chip.listeners.get('click')();
  await new Promise(resolve => setImmediate(resolve));

  assert.equal(context.historyCataloguedOnly, true);
  assert.equal(chip.attributes['aria-pressed'], 'true');
  assert.deepEqual(requests, [{
    url: '/api/transcript-search',
    method: 'POST',
    body: {query: '', catalogued: true, tags: []},
  }]);

  chip.listeners.get('click')();
  await new Promise(resolve => setImmediate(resolve));

  assert.equal(context.historyCataloguedOnly, false);
  assert.equal(chip.attributes['aria-pressed'], 'false');
  assert.deepEqual(requests, [{
    url: '/api/transcript-search',
    method: 'POST',
    body: {query: '', catalogued: true, tags: []},
  }, {
    url: '/api/transcripts',
    method: 'POST',
    body: null,
  }]);
});

test('runHistoryQuery searches a tag without requiring text or the chip', async () => {
  const requests = [];
  const {context, elements} = loadCatalogueHistoryClient({
    fetch: async (url, options) => {
      requests.push({
        url,
        method: options.method,
        body: JSON.parse(options.body),
      });
      return {
        ok: true,
        json: async () => ({results: [], truncated: false}),
      };
    },
  });
  context.historyCataloguedOnly = false;
  elements.get('history-search-input').value = '#syzygy';

  context.runHistoryQuery();
  await new Promise(resolve => setImmediate(resolve));

  assert.deepEqual(requests, [{
    url: '/api/transcript-search',
    method: 'POST',
    body: {query: '', catalogued: false, tags: ['syzygy']},
  }]);
});

test('runHistoryQuery rejects a one-character text query without fetching', async () => {
  const requests = [];
  const {context, elements} = loadCatalogueHistoryClient({
    fetch: async (url, options) => {
      requests.push({url, options});
      return {
        ok: true,
        json: async () => ({results: [], truncated: false}),
      };
    },
  });
  context.historyCataloguedOnly = false;
  elements.get('history-search-input').value = 'a';

  context.runHistoryQuery();
  await new Promise(resolve => setImmediate(resolve));

  assert.deepEqual(requests, []);
  assert.match(elements.get('history-body').innerHTML, /at least 2/);
});

test('setHistoryView paints the catalogue button from the resume target', () => {
  const {context, elements} = loadCatalogueHistoryClient();
  const button = elements.get('history-catalogue');

  context.setHistoryView('Saved transcript', null, true, {
    sessionId: 's1',
    catalogued: '2026-09-08T12:00:00Z',
  });
  assert.equal(button.style.display, '');
  assert.equal(button.textContent, 'uncatalogue');
  assert.equal(button.classList.contains('kept'), true);

  context.setHistoryView('Unsaved transcript', null, true, {
    sessionId: 's1',
    catalogued: '',
  });
  assert.equal(button.style.display, '');
  assert.equal(button.textContent, 'catalogue');
  assert.equal(button.classList.contains('kept'), false);

  context.setHistoryView('History', null);
  assert.equal(button.style.display, 'none');
});

test('catalogueFlow loads existing tags and saves the prompted note and normalized tags', async () => {
  const requests = [];
  const prompts = [];
  const answers = ['why', 'a, B'];
  const initial = {
    sessionId: 's1',
    catalogued: '',
    note: '',
    tags: [],
    allTags: ['old'],
  };
  const saved = {
    sessionId: 's1',
    catalogued: '2026-09-08T12:00:00Z',
    note: 'why',
    tags: ['a', 'b'],
    allTags: ['old', 'a', 'b'],
  };
  const {context} = loadHistoryClient({
    window: {
      prompt: (message, value) => {
        prompts.push({message, value});
        return answers.shift();
      },
    },
    fetch: async (url, options) => {
      const method = options.method || 'GET';
      requests.push({
        url,
        method,
        body: options.body === undefined ? null : JSON.parse(options.body),
      });
      return {
        ok: true,
        text: async () => JSON.stringify(method === 'GET' ? initial : saved),
      };
    },
  });

  const result = await context.catalogueFlow('s1');

  assert.deepEqual(requests, [{
    url: '/api/catalogue?sessionId=s1',
    method: 'GET',
    body: null,
  }, {
    url: '/api/catalogue',
    method: 'POST',
    body: {sessionId: 's1', note: 'why', tags: ['a', 'b']},
  }]);
  assert.equal(prompts.length, 2);
  assert.match(prompts[1].message, /#old/);
  assert.deepEqual(result, saved);
});

test('catalogueFlow returns null without posting when the first prompt is cancelled', async () => {
  const requests = [];
  let promptCalls = 0;
  const {context} = loadHistoryClient({
    window: {
      prompt: () => {
        promptCalls++;
        return null;
      },
    },
    fetch: async (url, options) => {
      requests.push({url, method: options.method || 'GET'});
      return {
        ok: true,
        text: async () => JSON.stringify({
          sessionId: 's1',
          catalogued: '',
          note: '',
          tags: [],
          allTags: ['old'],
        }),
      };
    },
  });

  assert.equal(await context.catalogueFlow('s1'), null);
  assert.equal(promptCalls, 1);
  assert.deepEqual(requests, [{
    url: '/api/catalogue?sessionId=s1',
    method: 'GET',
  }]);
});

// --- Catalogue and pin: response ordering (a slow read must not undo a write) ---
test('a catalogue GET that was in flight before a save cannot overwrite the saved state', async () => {
  const slowGet = deferred();
  const {context} = loadHistoryClient({
    fetch: async (url, options) => {
      if (!options || !options.method) return slowGet.promise;
      return {ok: true, text: async () => JSON.stringify({sessionId: 's1', catalogued: '2026-09-08T10:00:00Z', note: 'saved', tags: ['a'], allTags: ['a']})};
    },
  });
  const read = context.fetchCatalogueState('s1');
  await context.saveCatalogue('s1', 'saved', ['a']);
  slowGet.resolve({ok: true, text: async () => JSON.stringify({sessionId: 's1', catalogued: '', note: '', tags: [], allTags: []})});
  await read;
  assert.equal(vm.runInContext("catalogueStates.get('s1').note", context), 'saved');
});

test('catalogueFlow neither prompts nor saves once the caller is no longer current', async () => {
  let prompts = 0;
  let posts = 0;
  let current = true;
  const {context} = loadHistoryClient({
    window: {prompt: () => { prompts += 1; return 'x'; }},
    fetch: async (url, options) => {
      if (options && options.method === 'POST') posts += 1;
      current = false; // the user navigated away while the read was in flight
      return {ok: true, text: async () => JSON.stringify({sessionId: 's1', catalogued: '', note: '', tags: [], allTags: []})};
    },
  });
  assert.equal(await context.catalogueFlow('s1', () => current), null);
  assert.equal(prompts, 0);
  assert.equal(posts, 0);
});

test('going back to a narrowed History list after a catalogue change reruns the query', async () => {
  const searches = [];
  const {context, elements} = loadHistoryClient({
    fetch: async (url, options) => {
      if (url === '/api/transcript-search') {
        searches.push(JSON.parse(options.body));
        return {ok: true, json: async () => ({results: [{file: '/t.md', sessionId: 's1', catalogued: '2026-09-08T10:00:00Z', project: 'p', timestamp: '2026-09-08-10-00-00'}], truncated: false})};
      }
      if (url.indexOf('/api/transcript?') === 0) return {ok: true, json: async () => ({content: ''})};
      throw new Error('unexpected ' + url);
    },
  });
  await context.searchHistory('#');
  assert.equal(searches.length, 1);
  const row = vm.runInContext('historyRootState.list[0]', context);
  await context.openTranscript(row, '');
  context.rememberCatalogueState({sessionId: 's1', catalogued: '', note: '', tags: []});
  elements.get('history-back').listeners.get('click')();
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(searches.length, 2);
  assert.deepEqual(searches[1], {query: '', catalogued: true, tags: []});
});

function loadOrreryPins(overrides = {}) {
  const html = fs.readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const start = html.indexOf('// --- Orrery pins: chats the phone keeps at the top ---');
  const end = html.indexOf('function renderSessions(', start);
  assert.notEqual(start, -1, 'orrery pins block should exist');
  const context = {
    basePath: '', lastSessions: [], orreryEl: new FakeElement('orrery'), navListEl: new FakeElement('nav-list'),
    renderSessions: () => {}, JSON, Promise, Array, Set, Number,
    fetch: async () => { throw new Error('unexpected fetch'); },
    ...overrides,
  };
  vm.createContext(context);
  vm.runInContext(html.slice(start, end), context, {filename: 'index.html#orrery-pins'});
  return context;
}

test('a pin report that was in flight before a toggle cannot undo the toggle', async () => {
  const slowGet = deferred();
  const context = loadOrreryPins({
    fetch: async (url, options) => options && options.method === 'POST'
      ? {ok: true, json: async () => ({bufferName: 'chat', pinned: true, pins: ['chat']})}
      : slowGet.promise,
  });
  const report = context.loadOrreryPins();
  assert.equal(await context.toggleChatPin('chat'), true);
  slowGet.resolve({ok: true, json: async () => ({pins: []})});
  await report;
  assert.equal(context.isChatPinned('chat'), true);
});
