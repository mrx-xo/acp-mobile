// acp-mobile service worker: Web Push only.  No fetch handler and no
// caching on purpose -- index.html reloads itself when the server's build
// id changes, and an offline cache would defeat that.
'use strict';

function trace(event, detail) {
  try {
    return fetch('/api/push-trace', { method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ event: 'sw:' + event, detail: String(detail || '') }) }).catch(() => {});
  } catch (e) { return Promise.resolve(); }
}

const SW_VERSION = 'trace-2';
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil((async () => {
  await self.clients.claim();
  await trace('activate', SW_VERSION);
})()));

self.addEventListener('push', (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (e) { data = { body: event.data && event.data.text() }; }
  const title = data.title || data.bufferName || 'agent-shell';
  event.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    await trace('push', SW_VERSION + ' ' + (data.bufferName || '') + ' windows=' + wins.length + ' ' +
      wins.map((w) => (w.visibilityState || '?') + (w.focused ? ' focused' : '')).join(','));
    await self.registration.showNotification(title, {
      body: data.body || '',
      tag: data.tag || data.bufferName || undefined,
      data: { bufferName: data.bufferName || '' },
    });
  })());
});

// Tap: record the target durably first, then hand it to an open window or
// open one.  iOS freezes/evicts a backgrounded home-screen app and reloads
// it on focus, so a postMessage to the old page is lost and openWindow's
// URL is not always honored.  The page reads and clears the pending
// target on every load and resume (see consumePendingSession).
const PENDING_CACHE = 'acp-pending';
const PENDING_KEY = '/pending-session';
async function rememberPendingSession(bufferName) {
  if (!bufferName) return;
  try {
    const c = await caches.open(PENDING_CACHE);
    await c.put(PENDING_KEY, new Response(bufferName, { headers: { 'Content-Type': 'text/plain' } }));
  } catch (e) {}
}
self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const bufferName = (event.notification.data && event.notification.data.bufferName) || '';
  event.waitUntil((async () => {
    await trace('notificationclick', bufferName);
    await rememberPendingSession(bufferName);
    // Second channel to an open page that does not depend on matchAll
    // finding the window (iOS is not reliable there).
    let bc = 'ok';
    try { new BroadcastChannel('acp-push').postMessage({ type: 'open-session', bufferName }); } catch (e) { bc = 'error ' + e; }
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    await trace('clients', wins.length + ' window(s), broadcast ' + bc + ' ' +
      wins.map((w) => (w.visibilityState || '?') + (w.focused ? ' focused' : '')).join(','));
    if (wins.length) {
      const win = wins[0];
      let focus = 'ok';
      if ('focus' in win) { try { await win.focus(); } catch (e) { focus = 'error ' + e; } }
      win.postMessage({ type: 'open-session', bufferName });
      await trace('posted', 'focus ' + focus);
      return;
    }
    const url = bufferName ? '/?session=' + encodeURIComponent(bufferName) : '/';
    let open = 'ok';
    try { await self.clients.openWindow(url); } catch (e) { open = 'error ' + e; }
    await trace('openWindow', open);
  })());
});
