'use strict';
// Web Push (005-AC24) for the room page and /profile (038-AC14): one browser subscription,
// registered for each room where the participant turns notifications on. A room's state in this
// browser is its localStorage key, which both pages read.
window.grillPush = (() => {
  const pushable = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
  const ios = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
  const homeScreen = matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
  // hint says why this browser cannot turn notifications on, '' when it can.
  const hint = ios && !homeScreen
    ? 'On iPhone and iPad, notifications work once the room is on the home screen: Share, Add to Home Screen, then open it from there.'
    : pushable ? '' : 'This browser does not support notifications.';
  const denied = 'The browser did not allow notifications: allow them in its site settings, then try again.';
  const key = (room) => 'grill-room-push:' + room;
  const url = (room) => '/api/rooms/' + encodeURIComponent(room) + '/push';
  // The explainer's dismissal snoozes the room's unprompted offer for 14 days in this browser (008-AC11).
  const snoozeKey = 'grill-room-push-snooze';
  const snooze = () => localStorage.setItem(snoozeKey, String(Date.now() + 14 * 24 * 3600e3));

  // on: turned on here, and the browser still allows notifications; revoked in its settings, the
  // toggle offers to turn them on again.
  const on = (room) => localStorage.getItem(key(room)) === '1' && pushable && Notification.permission === 'granted';

  // plainSend stands in for the room page's sendJSON on /profile: it throws on an error answer.
  async function plainSend(method, to, body) {
    const res = await fetch(to, { method, headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'HTTP ' + res.status);
    return data;
  }

  // rooms records in Cache Storage the rooms with notifications on and their "every line" choice:
  // the service worker renews the subscription for each (sw.js resubscribe).
  async function rooms(edit) {
    const cache = await caches.open('grill-room-push');
    const saved = await cache.match('/rooms');
    const list = saved ? await saved.json() : {};
    edit(list);
    await cache.put('/rooms', Response.json(list));
  }

  // subscription is this browser's, made with the instance's key when create; null when send
  // answered null, as the room page's does once its access ended.
  async function subscription(room, create, send = plainSend) {
    const reg = await navigator.serviceWorker.register('/sw.js');
    await navigator.serviceWorker.ready;
    const sub = await reg.pushManager.getSubscription();
    if (!create) return sub;
    const got = await send('GET', url(room));
    if (!got) return null;
    const vapid = Uint8Array.from(atob(got.key.replaceAll('-', '+').replaceAll('_', '/')), (c) => c.charCodeAt(0));
    const held = sub && sub.options.applicationServerKey;
    if (sub && held && vapid.join() !== new Uint8Array(held).join()) {
      // Made with another VAPID key, as after the instance's key changed: push services refuse it.
      await send('DELETE', url(room), { subscription: { endpoint: sub.endpoint } });
      await sub.unsubscribe();
      const fresh = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: vapid });
      // The other rooms turned on in this browser held the old one too.
      reg.active.postMessage({ resubscribe: fresh.toJSON() });
      return fresh;
    }
    return sub || reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: vapid });
  }

  // save hands the server this browser's subscription for room, with its "every line" choice.
  async function save(room, send = plainSend) {
    const sub = await subscription(room, true, send);
    if (!sub) return false;
    const all = localStorage.getItem(key(room) + ':all') === '1';
    if (!(await send('PUT', url(room), { subscription: sub.toJSON(), all_messages: all }))) return false;
    localStorage.setItem(key(room), '1');
    await rooms((list) => { list[room] = all; });
    return true;
  }

  async function off(room, send = plainSend) {
    const sub = await subscription(room, false, send);
    if (sub) await send('DELETE', url(room), { subscription: { endpoint: sub.endpoint } });
    localStorage.removeItem(key(room));
    await rooms((list) => { delete list[room]; });
  }

  return { pushable, hint, denied, key, snoozeKey, snooze, on, save, off };
})();
