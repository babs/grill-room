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

function render({ status: s, logs }) {
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
  mark($('can-host'), s.can_host, s.can_host ? 'available' : 'unavailable: ' + s.reason);
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
$('recheck').addEventListener('click', () => {
  api('POST', '/api/recheck').then(() => api('GET', '/api/status').then(render)).catch((err) => fail(err.message));
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
}).catch((err) => fail(err.message));
