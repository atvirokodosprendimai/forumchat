// sw.js — forumchat service worker.
//
// Job: receive Web Push notifications and surface them via
// self.registration.showNotification, and re-focus / open the right
// page when the user taps a notification.
//
// We keep state out of here: the server includes a `url` on every
// payload, and we just open or focus that. No cache plumbing — this
// SW is push-only, intentionally narrow.

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  // Parse JSON; tolerate empty pushes (Chrome can wake the SW with no payload).
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch { data = {}; }
  const title = data.title || 'forumchat';
  const body  = data.body  || '';
  const url   = data.url   || '/';
  const tag   = data.tag   || undefined;
  const icon  = data.icon  || '/static/icon-192.png';

  event.waitUntil(
    self.registration.showNotification(title, {
      body, tag, icon,
      badge: '/static/icon-192.png',
      data: { url },
      renotify: !!tag,
    })
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = event.notification.data?.url || '/';
  event.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    // Prefer focusing an already-open tab on the same origin.
    for (const c of all) {
      try {
        if (new URL(c.url).origin === self.location.origin) {
          await c.focus();
          if ('navigate' in c) { try { await c.navigate(url); } catch (_) {} }
          return;
        }
      } catch (_) {}
    }
    if (self.clients.openWindow) {
      await self.clients.openWindow(url);
    }
  })());
});
