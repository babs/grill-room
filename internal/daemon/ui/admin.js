'use strict';

const $ = (id) => document.getElementById(id);

// The link's one-time code is traded for a session token kept in sessionStorage: it survives a
// reload of this tab and is never sent by the browser on its own, so other sites cannot use it.
async function session() {
  const m = location.pathname.match(/^\/s\/([^/]+)$/);
  if (m) {
    history.replaceState(null, '', '/');
    const res = await fetch('/api/session', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code: m[1] }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'HTTP ' + res.status);
    sessionStorage.setItem('token', data.token);
  }
  const token = sessionStorage.getItem('token');
  if (!token) throw new Error('Open the latest link the daemon printed in its terminal.');
  return token;
}

let token;
let stopped = false;

async function api(method, path, body) {
  const opts = { method, headers: { Authorization: 'Bearer ' + token } };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const data = res.status === 204 ? {} : await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || 'HTTP ' + res.status);
  return data;
}

function fail(text) {
  $('banner').textContent = text;
  $('banner').hidden = false;
}

function mark(el, ok, text) {
  el.className = ok ? 'ok' : 'bad';
  el.textContent = text;
}

const pairingText = {
  '': 'Not paired.',
  requesting: 'Asking the room to pair…',
  pending: 'Waiting for your approval in the room.',
  paired: 'Paired.',
};

// renderSettings fills the cap form, unless the user is changing it.
function renderSettings(set, models, efforts) {
  const form = $('settings-form');
  if (form.dataset.dirty) return;
  for (const [id, values] of [['max-model', models], ['max-effort', efforts]]) {
    const sel = $(id);
    if (sel.options.length === 1) {
      for (const v of values) sel.add(new Option(v, v));
    }
  }
  $('max-model').value = set.max_model;
  $('max-effort').value = set.max_effort;
  $('approve-all').checked = set.approve_all;
}

// renderAllowlist lists the domains, each with its remove button; unchanged lists keep their DOM.
let shownAllowlist = '';
function renderAllowlist(list) {
  const key = JSON.stringify(list);
  if (key === shownAllowlist) return;
  shownAllowlist = key;
  const ul = $('allowlist');
  ul.replaceChildren();
  if (!list.length) {
    const li = document.createElement('li');
    li.textContent = 'No domain: the sandbox reaches nothing.';
    ul.append(li);
  }
  for (const domain of list) {
    const li = document.createElement('li');
    const name = document.createElement('code');
    name.textContent = domain;
    const rm = document.createElement('button');
    rm.type = 'button';
    rm.textContent = 'Remove';
    rm.setAttribute('aria-label', 'Remove ' + domain);
    rm.addEventListener('click', () => saveAllowlist(list.filter((d) => d !== domain)));
    li.append(name, ' ', rm);
    ul.append(li);
  }
}

let currentAllowlist = [];
function saveAllowlist(list) {
  $('allowlist-status').textContent = '';
  return api('POST', '/api/allowlist', { allowlist: list })
    .then(() => api('GET', '/api/status').then(render))
    .then(() => { $('allowlist-status').textContent = 'Saved.'; return true; })
    .catch((err) => { $('allowlist-status').textContent = err.message; return false; });
}

function renderDenied(list) {
  const body = $('denied').tBodies[0];
  body.replaceChildren();
  for (const d of list) {
    const tr = body.insertRow();
    tr.insertCell().textContent = new Date(d.time).toLocaleString();
    tr.insertCell().textContent = d.host;
  }
  $('denied').hidden = !list.length;
  $('denied-empty').hidden = !!list.length;
}

async function refreshDenied() {
  try {
    renderDenied((await api('GET', '/api/denied')).denied);
  } catch (err) {
    $('denied-empty').hidden = false;
    $('denied-empty').textContent = err.message;
  }
  if (!stopped) setTimeout(refreshDenied, 5000);
}

function badge(text, cls) {
  const b = document.createElement('span');
  b.className = 'badge ' + cls;
  b.textContent = text;
  return b;
}

// renderInstances lists the supervisor's instances; a daemon serving one room has none.
function renderInstances(instances, current, autostart) {
  $('instances-section').hidden = !instances;
  if (!instances) return;
  const ul = $('instances');
  ul.replaceChildren();
  for (const i of instances) {
    const li = document.createElement('li');
    const url = document.createElement('code');
    url.textContent = i.url;
    let text = ' · ' + i.state;
    if (i.version) text += ', ' + i.version;
    if (i.connection) text += ', ' + i.connection;
    if (i.restarts) text += ', ' + i.restarts + ' restart' + (i.restarts > 1 ? 's' : '');
    if (i.error) text += ': ' + i.error;
    li.append(url, text);
    if (i.trust) {
      const trust = document.createElement('div');
      trust.className = 'hint';
      trust.textContent = 'Worker binary: ' + i.trust;
      li.append(trust);
    }
    if (i.accept_unsigned) {
      const row = document.createElement('div');
      row.className = 'relax';
      const off = document.createElement('button');
      off.type = 'button';
      off.textContent = 'Stop accepting unsigned binaries';
      off.addEventListener('click', () => setRelax(i.id, false, ''));
      row.append(badge('accepts unsigned binaries', 'danger'), off);
      li.append(row);
    }
    const rooms = document.createElement('ul');
    for (const r of i.rooms) {
      const room = document.createElement('li');
      room.textContent = r + (r === i.room ? ' (served)' : '');
      rooms.append(room);
    }
    li.append(rooms);
    if (i.id === current) li.className = 'current';
    ul.append(li);
  }
  // Rooms set on the page go to an instance: these sections follow it.
  for (const el of document.querySelectorAll('.per-instance')) el.hidden = !current;
  const box = $('autostart');
  box.parentElement.hidden = autostart === undefined;
  if (autostart !== undefined && !box.dataset.busy) box.checked = autostart;
}

const selfText = {
  project: ['project-signed', 'ok'],
  'dev-key': ['signed with a dev key', 'ok'],
  'dev build': ['dev build', ''],
  unsigned: ['NOT project-signed', 'bad'],
  unknown: ['signature not checked', ''],
};

function setRelax(instance, enabled, host) {
  $('relax-status').textContent = '';
  return api('POST', '/api/relax', { instance, enabled, host })
    .then(() => api('GET', '/api/status').then(render))
    .then(() => { $('relax-status').textContent = enabled ? 'Unsigned binaries accepted.' : ''; return true; })
    .catch((err) => { $('relax-status').textContent = err.message; return false; });
}

// fillSelect replaces a select's options when the instances change, keeping the selection.
function fillSelect(sel, instances) {
  const key = instances.map((i) => i.id).join();
  if (sel.dataset.key === key) return;
  sel.dataset.key = key;
  const was = sel.value;
  sel.replaceChildren(...instances.map((i) => new Option(i.url, i.id)));
  if (was) sel.value = was;
}

// renderTrust shows the supervisor's signature, its trust settings and the relax toggles
// (003-AC4, 003-AC5).
let shownKeys = '';
function renderTrust(t, instances) {
  $('trust-section').hidden = !t;
  $('self-sig').hidden = !t;
  if (!t) return;
  const [text, cls] = selfText[t.self.state] || [t.self.state, ''];
  const sig = $('self-sig');
  sig.textContent = text;
  sig.className = 'badge ' + cls;
  mark($('self-detail'), t.self.state !== 'unsigned', text + (t.self.detail ? ': ' + t.self.detail : ''));
  const r = t.root;
  let root = { tuf: 'refreshed through TUF', disk: 'last good copy on disk', embedded: 'embedded in this binary' }[r.source] || 'not loaded yet';
  if (r.source && r.at && !r.at.startsWith('0001')) root += ', ' + new Date(r.at).toLocaleString();
  if (r.error) root += ' (TUF unreachable: ' + r.error + ')';
  $('root').textContent = root;
  $('checked').textContent = (t.checked && !t.checked.startsWith('0001') ? new Date(t.checked).toLocaleString() : 'not yet') + (t.check_error ? ': ' + t.check_error : '');
  const list = instances || [];
  fillSelect($('channel-instance'), list);
  fillSelect($('relax-instance'), list.filter((i) => !i.accept_unsigned));
  $('relax-form').hidden = !$('relax-instance').options.length;
  const form = $('channel-form');
  if (!form.dataset.dirty) {
    $('channel').value = t.channel;
    if (t.follow) $('channel-instance').value = t.follow;
  }
  $('channel-instance').hidden = $('channel').value !== 'instance';
  const relaxed = list.filter((i) => i.accept_unsigned);
  $('relax-banner').hidden = !relaxed.length;
  $('relax-list').textContent = relaxed.map((i) => i.url).join(', ');
  const key = JSON.stringify(t.dev_keys);
  if (key === shownKeys) return;
  shownKeys = key;
  const ul = $('dev-keys');
  ul.replaceChildren();
  if (!t.dev_keys.length) {
    const li = document.createElement('li');
    li.textContent = 'None.';
    ul.append(li);
  }
  for (const k of t.dev_keys) {
    const li = document.createElement('li');
    const name = document.createElement('code');
    name.textContent = k.name;
    const rm = document.createElement('button');
    rm.type = 'button';
    rm.textContent = 'Remove';
    rm.setAttribute('aria-label', 'Remove ' + k.name);
    rm.addEventListener('click', () => api('POST', '/api/dev-keys/remove', { name: k.name })
      .then(() => api('GET', '/api/status').then(render))
      .catch((err) => { $('key-status').textContent = err.message; }));
    li.append(name, ' · ' + k.fingerprint + ' ', rm);
    ul.append(li);
  }
}

function render({ status: s, logs, models, efforts, instances, instance, autostart, trust }) {
  renderInstances(instances, instance, autostart);
  renderTrust(trust, instances);
  $('host').textContent = s.host + ' · ' + s.version;
  $('room').textContent = s.room || 'No room yet: paste the room link below.';
  let text = pairingText[s.pairing] || s.pairing;
  if (s.pairing === 'paired') text += ' ' + { connected: 'Connected.', connecting: 'Connecting…', offline: 'Offline.' }[s.connection];
  $('pairing').textContent = text + (s.error ? ' ' + s.error : '');
  $('approve').hidden = s.pairing !== 'pending';
  if (s.pairing === 'pending') {
    $('code').textContent = s.code;
    $('approve-link').href = s.approve_url;
  }
  $('room-form').hidden = s.pairing === 'paired' && s.connection === 'connected';
  mark($('docker'), s.docker.ok, s.docker.ok ? 'running (' + s.docker.version + ')' : s.docker.reason);
  const c = s.credentials;
  if (!c.ok) mark($('creds'), false, c.error);
  else if (c.expired) mark($('creds'), false, 'expired: run Claude Code on this machine to refresh them');
  else mark($('creds'), true, 'found in ' + c.origin + (c.expires_at ? ', valid until ' + new Date(c.expires_at).toLocaleString() : ''));
  const img = s.image;
  const imageText = {
    '': 'waiting for the room server',
    pending: 'waiting for Docker',
    checking: 'checking…',
    pulling: 'pulling from the room server\'s registry: ' + (img.progress || ''),
    building: 'building on this machine: ' + (img.progress || ''),
    ready: 'ready (' + img.ref + ')',
    failed: img.error,
  }[img.state];
  mark($('image'), img.state === 'ready', imageText || img.state);
  const u = s.update;
  $('update').hidden = !u.available;
  if (u.available) {
    $('update-version').textContent = u.available;
    $('update-from').textContent = u.from || 'the room server';
    $('update-apply').disabled = u.running;
    $('update-status').textContent = u.running ? 'Downloading…' : u.error || '';
  }
  mark($('can-host'), s.can_host, s.can_host ? 'available' : 'unavailable: ' + s.reason);
  renderSettings(s.settings, models, efforts);
  currentAllowlist = s.allowlist;
  renderAllowlist(s.allowlist);
  const logs$ = $('logs');
  const atBottom = logs$.scrollTop + logs$.clientHeight >= logs$.scrollHeight - 20;
  logs$.textContent = logs.join('\n');
  if (atBottom) logs$.scrollTop = logs$.scrollHeight;
}

async function refresh() {
  try {
    render(await api('GET', '/api/status'));
  } catch (err) {
    if (!stopped) fail(err.message);
    return;
  }
  if (!stopped) setTimeout(refresh, 2000);
}

$('room-form').addEventListener('submit', (e) => {
  e.preventDefault();
  $('room-error').textContent = '';
  api('POST', '/api/room', { url: $('room-url').value })
    .then(() => api('GET', '/api/status').then(render))
    .catch((err) => { $('room-error').textContent = err.message; });
});
$('settings-form').addEventListener('input', () => { $('settings-form').dataset.dirty = '1'; });
$('settings-form').addEventListener('submit', (e) => {
  e.preventDefault();
  $('settings-status').textContent = '';
  api('POST', '/api/settings', { max_model: $('max-model').value, max_effort: $('max-effort').value, approve_all: $('approve-all').checked })
    .then(() => {
      delete $('settings-form').dataset.dirty;
      $('settings-status').textContent = 'Saved.';
      return api('GET', '/api/status').then(render);
    })
    .catch((err) => { $('settings-status').textContent = err.message; });
});
$('allowlist-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const domain = $('allowlist-add').value.trim();
  saveAllowlist([...currentAllowlist, domain]).then((ok) => { if (ok) $('allowlist-add').value = ''; });
});
$('channel').addEventListener('change', () => {
  $('channel-form').dataset.dirty = '1';
  $('channel-instance').hidden = $('channel').value !== 'instance';
});
$('channel-instance').addEventListener('change', () => { $('channel-form').dataset.dirty = '1'; });
$('channel-form').addEventListener('submit', (e) => {
  e.preventDefault();
  $('channel-status').textContent = '';
  const channel = $('channel').value;
  api('POST', '/api/update-channel', { channel, instance: channel === 'instance' ? $('channel-instance').value : '' })
    .then(() => {
      delete $('channel-form').dataset.dirty;
      $('channel-status').textContent = 'Saved. Checking for an update…';
      return api('GET', '/api/status').then(render);
    })
    .catch((err) => { $('channel-status').textContent = err.message; });
});
$('relax-form').addEventListener('submit', (e) => {
  e.preventDefault();
  setRelax($('relax-instance').value, true, $('relax-host').value).then((ok) => { if (ok) $('relax-host').value = ''; });
});
$('key-form').addEventListener('submit', (e) => {
  e.preventDefault();
  $('key-status').textContent = '';
  api('POST', '/api/dev-keys', { name: $('key-name').value.trim(), pem: $('key-pem').value })
    .then(() => {
      $('key-name').value = '';
      $('key-pem').value = '';
      $('key-status').textContent = 'Trusted.';
      return api('GET', '/api/status').then(render);
    })
    .catch((err) => { $('key-status').textContent = err.message; });
});
$('autostart').addEventListener('change', () => {
  const box = $('autostart');
  box.dataset.busy = '1';
  $('autostart-status').textContent = '';
  api('POST', '/api/autostart', { enabled: box.checked })
    .then(() => { $('autostart-status').textContent = box.checked ? 'Starts at your next login.' : 'Removed.'; })
    .catch((err) => { box.checked = !box.checked; $('autostart-status').textContent = err.message; })
    .finally(() => { delete box.dataset.busy; });
});
$('recheck').addEventListener('click', () => {
  api('POST', '/api/recheck').then(() => api('GET', '/api/status').then(render)).catch((err) => fail(err.message));
});
$('update-apply').addEventListener('click', () => {
  $('update-apply').disabled = true;
  $('update-status').textContent = 'Downloading…';
  api('POST', '/api/update').then(() => {
    stopped = true;
    $('update').hidden = true;
    $('main').hidden = true;
    fail('Updated. The daemon restarts and opens a new page; you can close this one.');
  }).catch((err) => {
    $('update-apply').disabled = false;
    $('update-status').textContent = err.message;
  });
});
$('stop').addEventListener('click', () => {
  api('POST', '/api/stop').then(() => {
    stopped = true;
    fail('The daemon stopped. You can close this tab.');
    $('main').hidden = true;
    $('stop').disabled = true;
  }).catch((err) => fail(err.message));
});

session().then((t) => {
  token = t;
  $('main').hidden = false;
  refresh();
  refreshDenied();
}).catch((err) => fail(err.message));
