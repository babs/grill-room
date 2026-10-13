'use strict';
// The Web Push service worker (005-AC24). A push names a room, the kind of event and how many
// gathered; one notification per room stands, its count adding up until it is closed or clicked.

const what = { mention: 'You were mentioned', turn: 'Your agent turn ended', synthesis: 'A persona answered', message: 'New lines' };

// notify shows push p, its count added to the notification of p's room if one stands.
async function notify(p) {
  // The url ends in #m<line>, a new one each batch: the tag keys on the room's path alone.
  const tag = 'grill-room:' + new URL(p.url || '/', self.location.origin).pathname;
  const shown = await self.registration.getNotifications({ tag });
  const count = shown.reduce((n, x) => n + ((x.data && x.data.count) || 0), 0) + (p.count || 1);
  const body = (what[p.kind] || 'Something happened') + (count > 1 ? ' · ' + count + ' events' : '');
  return self.registration.showNotification(p.room || 'grill-room', {
    body, tag, renotify: true, icon: '/static/icons/icon-192.png', badge: '/static/icons/icon-192.png', data: { url: p.url, count },
  });
}
self.addEventListener('push', (e) => e.waitUntil(notify(e.data ? e.data.json() : {})));

// A click opens the chat at the line that triggered the push: a tab of the room already open
// scrolls there, otherwise a new one opens on the deep link.
self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const url = new URL((e.notification.data && e.notification.data.url) || '/', self.location.origin);
  const jump = (url.hash.match(/^#m(\d+)$/) || [])[1];
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((tabs) => {
    const open = tabs.find((t) => new URL(t.url).pathname === url.pathname);
    if (!open) return self.clients.openWindow(url.href);
    if (jump) open.postMessage({ jump });
    return open.focus();
  }));
});
