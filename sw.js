// acp-mobile service worker: Web Push only.  No fetch handler and no
// caching on purpose -- index.html reloads itself when the server's build
// id changes, and an offline cache would defeat that.
'use strict';

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (e) { data = { body: event.data && event.data.text() }; }
  const title = data.title || data.bufferName || 'agent-shell';
  event.waitUntil(self.registration.showNotification(title, {
    body: data.body || '',
    tag: data.tag || data.bufferName || undefined,
    data: { bufferName: data.bufferName || '' },
  }));
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
    await rememberPendingSession(bufferName);
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    if (wins.length) {
      const win = wins[0];
      if ('focus' in win) { try { await win.focus(); } catch (e) {} }
      win.postMessage({ type: 'open-session', bufferName });
      return;
    }
    const url = bufferName ? '/?session=' + encodeURIComponent(bufferName) : '/';
    await self.clients.openWindow(url);
  })());
});
