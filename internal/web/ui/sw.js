'use strict';
// The Web Push service worker (005-AC24). A push names a room, the kind of event and how many
// gathered; one notification per room stands, its count adding up until it is closed or clicked.

const what = { mention: 'You were mentioned', turn: 'Your agent turn ended', synthesis: 'A persona answered', message: 'New lines' };

self.addEventListener('push', (e) => {
  const p = e.data ? e.data.json() : {};
  const tag = 'grill-room:' + p.url;
  e.waitUntil(self.registration.getNotifications({ tag }).then((shown) => {
    const before = shown.reduce((n, x) => n + ((x.data && x.data.count) || 0), 0);
    const count = before + (p.count || 1);
    const body = (what[p.kind] || 'Something happened') + (count > 1 ? ' · ' + count + ' events' : '');
    return self.registration.showNotification(p.room || 'grill-room', {
      body, tag, renotify: true, icon: '/static/icons/icon-192.png', badge: '/static/icons/icon-192.png', data: { url: p.url, count },
    });
  }));
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const url = new URL((e.notification.data && e.notification.data.url) || '/', self.location.origin).href;
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((tabs) => {
    const open = tabs.find((t) => t.url === url);
    return open ? open.focus() : self.clients.openWindow(url);
  }));
});
