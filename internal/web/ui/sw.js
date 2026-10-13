'use strict';
// The Web Push service worker (005-AC24). A push names a room, the kind of event and how many
// gathered; one notification per room stands, its count adding up until it is closed or clicked.

const what = { mention: 'You were mentioned', turn: 'Your agent turn ended', synthesis: 'A persona answered', message: 'New lines' };

// sameOrigin is url resolved on this origin, or the home page when it names another origin or
// does not parse.
function sameOrigin(url) {
  const home = new URL('/', self.location.origin);
  try {
    const u = new URL(url || '/', home);
    return u.origin === home.origin ? u : home;
  } catch {
    return home;
  }
}

// notify shows push p, its count added to the notification of p's room if one stands.
async function notify(p) {
  // The url ends in #m<line>, a new one each batch: the tag keys on the room's path alone.
  const tag = 'grill-room:' + sameOrigin(p.url).pathname;
  const shown = await self.registration.getNotifications({ tag });
  const count = shown.reduce((n, x) => n + ((x.data && x.data.count) || 0), 0) + (p.count || 1);
  const body = (what[p.kind] || 'Something happened') + (count > 1 ? ' · ' + count + ' events' : '');
  return self.registration.showNotification(p.room || 'grill-room', {
    body, tag, renotify: true, icon: '/static/icons/icon-192.png', badge: '/static/icons/badge-96.png', data: { url: p.url, count },
  });
}
// payload is a push's JSON, {} when it has none or it does not parse: a generic notification
// still shows, as Chrome warns of a site whose push shows nothing.
function payload(data) {
  try {
    return data ? data.json() : {};
  } catch {
    return {};
  }
}
self.addEventListener('push', (e) => e.waitUntil(notify(payload(e.data))));

// A click opens the chat at the line that triggered the push: a tab of the room already open
// scrolls there, otherwise a new one opens on the deep link.
self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const url = sameOrigin(e.notification.data && e.notification.data.url);
  const jump = (url.hash.match(/^#m(\d+)$/) || [])[1];
  e.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((tabs) => {
    const open = tabs.find((t) => new URL(t.url).pathname === url.pathname);
    if (!open) return self.clients.openWindow(url.href);
    if (jump) open.postMessage({ jump });
    return open.focus();
  }));
});

// resubscribe hands the server the subscription the browser renewed, for every room the page
// turned notifications on in: the page keeps them in Cache Storage, as a worker has no
// localStorage. The old one goes once its push service answers it is gone.
async function resubscribe(old, sub) {
  sub = sub || (old && await self.registration.pushManager.subscribe(old.options));
  if (!sub) return;
  const saved = await (await caches.open('grill-room-push')).match('/rooms');
  const rooms = saved ? await saved.json() : {};
  await Promise.all(Object.entries(rooms).map(([room, all]) => fetch('/api/rooms/' + encodeURIComponent(room) + '/push', {
    method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ subscription: sub.toJSON(), all_messages: all }),
  })));
}
self.addEventListener('pushsubscriptionchange', (e) => e.waitUntil(resubscribe(e.oldSubscription, e.newSubscription)));
// A page that subscribed again, as after a VAPID key change, hands the new subscription over.
self.addEventListener('message', (e) => {
  if (e.data && e.data.resubscribe) e.waitUntil(resubscribe(null, { toJSON: () => e.data.resubscribe }));
});
