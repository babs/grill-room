'use strict';

const $ = (id) => document.getElementById(id);
const roomID = decodeURIComponent(location.pathname.split('/')[2] || '');
const api = '/api/rooms/' + encodeURIComponent(roomID);
const messages = $('messages');
const sendForm = $('send');
const bodyInput = $('body');
let events;
let refreshTimer;
let wasIn = false;

function end(text) {
  if (events) events.close();
  $('banner').textContent = text;
  $('banner').hidden = false;
  sendForm.hidden = true;
  document.querySelector('.upload').hidden = true;
}

// loadRoom fetches the room header and participant list; false when access is gone.
async function loadRoom() {
  const res = await fetch(api);
  if (res.status === 401) {
    // A refresh can race the stream's "revoked" event: once in, both must tell the same thing.
    end(wasIn ? 'Your access to this room has ended.'
      : 'You are not in this room, or your access was revoked. Open your invite link again.');
    return false;
  }
  if (!res.ok) throw new Error('room: HTTP ' + res.status);
  wasIn = true;
  const data = await res.json();
  $('room-name').textContent = data.room.name;
  document.title = data.room.name + ' · grill-room';
  $('me').textContent = data.me.name;
  $('participants').replaceChildren(...data.participants.map((p) => {
    const li = document.createElement('li');
    li.textContent = p.name;
    return li;
  }));
  return true;
}

function addMessage(m) {
  const li = document.createElement('li');
  li.className = 'msg ' + m.kind;
  li.dataset.id = m.id;
  if (m.kind === 'system') {
    li.textContent = m.body;
    // A join changes the participant list; history replay sends many, so refresh once.
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => loadRoom().catch(console.error), 200);
  } else {
    const meta = document.createElement('div');
    meta.className = 'meta';
    const sender = document.createElement('strong');
    sender.textContent = m.sender;
    const time = document.createElement('time');
    time.dateTime = m.ts;
    time.textContent = new Date(m.ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    meta.append(sender, ' ', time);
    const body = document.createElement('div');
    body.className = 'body';
    body.textContent = m.body;
    li.append(meta, body);
  }
  const atBottom = messages.scrollTop + messages.clientHeight >= messages.scrollHeight - 40;
  messages.append(li);
  if (atBottom) messages.scrollTop = messages.scrollHeight;
}

// Documents. Text documents render in an iframe sandboxed without allow-same-origin: their
// scripts get an opaque origin, so no room cookie and no same-origin call to the API.
let openDoc = null;

function docURL(id, suffix) {
  return api + '/documents/' + encodeURIComponent(id) + suffix;
}

function describe(d) {
  if (d.kind === 'agent') return 'agent document, ' + d.revisions + (d.revisions === 1 ? ' revision' : ' revisions');
  return 'uploaded by ' + d.author;
}

async function loadDocuments() {
  const res = await fetch(api + '/documents');
  if (res.status === 401) return;
  if (!res.ok) throw new Error('documents: HTTP ' + res.status);
  const docs = await res.json();
  $('documents').replaceChildren(...docs.map((d) => {
    const li = document.createElement('li');
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'doc';
    b.textContent = d.name;
    b.title = describe(d);
    b.addEventListener('click', () => showDocument(d.id).catch(console.error));
    li.append(b);
    return li;
  }));
  if (openDoc) {
    const fresh = docs.find((d) => d.id === openDoc.id);
    if (fresh && fresh.revisions !== openDoc.revisions) await showDocument(fresh.id, true);
  }
}

function option(value, text) {
  const o = document.createElement('option');
  o.value = value;
  o.textContent = text;
  return o;
}

// showDocument opens a document; refresh keeps the revisions picked unless the latest was shown.
async function showDocument(id, refresh) {
  const res = await fetch(docURL(id, ''));
  if (!res.ok) throw new Error('document: HTTP ' + res.status);
  const { document: d, revisions } = await res.json();
  // A refresh that lands after the viewer closed or moved to another document is stale.
  if (refresh && (!openDoc || openDoc.id !== id)) return;
  const followLatest = !refresh || $('rev').value === String(openDoc.revisions);
  const keepFrom = refresh ? $('rev-from').value : '';
  openDoc = d;
  $('viewer').hidden = false;
  $('doc-name').textContent = d.name;
  $('doc-meta').textContent = d.kind === 'upload' && d.format === 'md' && !/\.(md|markdown)$/i.test(d.name)
    ? describe(d) + ', shown as its Markdown copy'
    : describe(d);
  const original = $('doc-original');
  original.hidden = d.kind !== 'upload';
  original.href = docURL(d.id, '/original');
  original.download = d.name;

  const agent = d.kind === 'agent';
  $('revisions').hidden = !agent;
  if (agent) {
    const prev = $('rev').value;
    $('rev').replaceChildren(...revisions.map((r) => option(r.n, 'r' + r.n + ' · ' + new Date(r.created_at).toLocaleString())));
    $('rev-from').replaceChildren(option('', 'nothing'), ...revisions.map((r) => option(r.n, 'r' + r.n)));
    $('rev').value = followLatest ? String(d.revisions) : prev;
    $('rev-from').value = keepFrom;
  }
  if (!refresh || followLatest) renderDocument();
}

function renderDocument() {
  const d = openDoc;
  let el;
  if (d.format === 'image') {
    el = document.createElement('img');
    el.src = docURL(d.id, '/original');
    el.alt = d.name;
  } else {
    el = document.createElement('iframe');
    el.title = d.name;
    if (d.format === 'pdf') {
      // Chrome will not run its PDF viewer in a sandboxed frame; a PDF runs no page script.
      el.src = docURL(d.id, '/original');
    } else {
      el.setAttribute('sandbox', 'allow-scripts');
      const q = new URLSearchParams();
      if (d.kind === 'agent') {
        q.set('rev', $('rev').value);
        if ($('rev-from').value) q.set('from', $('rev-from').value);
      }
      el.src = docURL(d.id, '/view' + (q.size ? '?' + q : ''));
    }
  }
  $('doc-frame').replaceChildren(el);
}

$('rev').addEventListener('change', renderDocument);
$('rev-from').addEventListener('change', renderDocument);
$('doc-close').addEventListener('click', () => {
  openDoc = null;
  $('viewer').hidden = true;
  $('doc-frame').replaceChildren();
});

async function upload(file) {
  const status = $('upload-status');
  status.textContent = 'Uploading ' + file.name + '…';
  const form = new FormData();
  form.append('file', file);
  const res = await fetch(api + '/documents', { method: 'POST', body: form });
  if (res.status === 401) {
    end('Your access to this room has ended.');
    return;
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    status.textContent = data.error || 'Upload failed: HTTP ' + res.status;
    return;
  }
  status.textContent = '';
  await showDocument(data.id);
}

$('upload-input').addEventListener('change', (e) => {
  const file = e.target.files[0];
  e.target.value = '';
  if (file) upload(file).catch((err) => { $('upload-status').textContent = 'Upload failed.'; console.error(err); });
});

function connect() {
  events = new EventSource(api + '/events');
  events.onmessage = (e) => addMessage(JSON.parse(e.data));
  // Listing on open, not before: the stream announces only changes made after it opened.
  events.onopen = () => loadDocuments().catch(console.error);
  events.addEventListener('documents', () => loadDocuments().catch(console.error));
  events.addEventListener('revoked', () => end('Your access to this room has ended.'));
  events.onerror = () => {
    // CLOSED means the browser gave up (a 401 on reconnect); otherwise it retries by itself.
    if (events.readyState === EventSource.CLOSED) loadRoom().catch(console.error);
  };
}

async function send() {
  const body = bodyInput.value.trim();
  if (!body) return;
  const res = await fetch(api + '/messages', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ body }),
  });
  if (res.status === 401) {
    end('Your access to this room has ended.');
    return;
  }
  if (!res.ok) throw new Error('send: HTTP ' + res.status);
  bodyInput.value = '';
}

sendForm.addEventListener('submit', (e) => {
  e.preventDefault();
  send().catch(console.error);
});
bodyInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    send().catch(console.error);
  }
});

loadRoom().then((ok) => { if (ok) connect(); }).catch(console.error);
