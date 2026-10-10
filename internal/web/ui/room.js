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
let participants = [];
let myID = '';

function end(text) {
  if (events) events.close();
  leaveVoice();
  $('voice').hidden = true;
  $('banner').textContent = text;
  $('banner').hidden = false;
  sendForm.hidden = true;
  document.querySelector('.upload').hidden = true;
  $('annotations').hidden = true;
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
  participants = data.participants;
  myID = String(data.me.id);
  $('voice').hidden = !data.voice;
  renderParticipants();
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
  if (!refresh) closeNew();
  await loadAnnotations();
}

function renderDocument() {
  const d = openDoc;
  let el;
  frameReady = false;
  setPick(false);
  $('annot-pick').hidden = d.format !== 'html';
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
  closeNew();
  annotations = [];
  renderAnnotations();
});

// Annotations. The document frame runs untrusted scripts, so it only ever proposes: an anchor to
// annotate, a mark clicked. The thread UI and every API call live here, in the room page.
let annotations = [];
let frameReady = false;
// newAnchor is the anchor of the annotation being written: undefined when none is, null for a
// comment on the whole document.
let newAnchor;

const docFrame = () => $('doc-frame').querySelector('iframe[sandbox]');

// canCreate: anchors are taken on the latest revision, shown without a diff.
function canCreate() {
  const d = openDoc;
  if (!d || d.revisions === 0) return false;
  return d.kind !== 'agent' || ($('rev').value === String(d.revisions) && !$('rev-from').value);
}

// The frame's origin is opaque, so it cannot be named: only anchors and statuses go there, never
// thread text, names or credentials.
function postToFrame(msg) {
  const frame = docFrame();
  if (frame && frameReady) frame.contentWindow.postMessage(msg, '*');
}

function sendAnnotations() {
  postToFrame({
    type: 'gr:annotations',
    items: annotations.map((a) => ({ id: a.id, status: a.status, anchor: a.anchor })),
    canCreate: canCreate(),
  });
}

function rejectFrameMessage(why) {
  console.warn('bridge: rejected frame message: ' + why);
}

window.addEventListener('message', (ev) => {
  const frame = docFrame();
  // Anything not from the document frame on show is none of this bridge's business.
  if (!frame || ev.source !== frame.contentWindow) return;
  if (ev.origin !== 'null') return rejectFrameMessage('origin ' + ev.origin);
  const msg = GrillBridge.validate(ev.data);
  if (!msg) return rejectFrameMessage('malformed or oversized');
  switch (msg.type) {
    case 'gr:ready':
      frameReady = true;
      sendAnnotations();
      break;
    case 'gr:anchor':
      setPick(false);
      // A frame script can post anchors at will: never replace what the participant is typing.
      if (canCreate() && !(newAnchor !== undefined && $('annot-body').value.trim())) openNew(msg.anchor);
      break;
    case 'gr:open':
      focusItem(msg.id, false);
      break;
    case 'gr:pick':
      setPick(false);
      break;
  }
});

function setPick(on) {
  $('annot-pick').setAttribute('aria-pressed', String(on));
  postToFrame({ type: 'gr:pick', on });
}

$('annot-pick').addEventListener('click', () => setPick($('annot-pick').getAttribute('aria-pressed') !== 'true'));
$('annot-doc').addEventListener('click', () => openNew(null));
$('annot-cancel').addEventListener('click', closeNew);

function clip(s, n) {
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}

function describeAnchor(a) {
  if (!a) return 'Whole document';
  if (a.kind === 'text') return '"' + clip(a.exact, 160) + '"';
  return '<' + a.tag + '> element' + (a.fingerprint ? ': ' + clip(a.fingerprint, 80) : '');
}

function openNew(anchor) {
  newAnchor = anchor;
  $('annot-target').textContent = describeAnchor(anchor);
  $('annot-new').hidden = false;
  $('annot-status').textContent = '';
  $('annot-body').focus();
}

function closeNew() {
  newAnchor = undefined;
  $('annot-new').hidden = true;
  $('annot-body').value = '';
  $('annot-files').value = '';
}

// readImages returns the chosen files as base64, the shape the API takes.
function readImages(input) {
  return Promise.all([...input.files].map((f) => new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve({ data: r.result.slice(r.result.indexOf(',') + 1) });
    r.onerror = () => reject(r.error);
    r.readAsDataURL(f);
  })));
}

async function sendJSON(method, url, body) {
  const res = await fetch(url, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  if (res.status === 401) {
    end('Your access to this room has ended.');
    return null;
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || 'HTTP ' + res.status);
  return data;
}

$('annot-new').addEventListener('submit', (e) => {
  e.preventDefault();
  (async () => {
    const d = openDoc;
    const attachments = await readImages($('annot-files'));
    const body = $('annot-body').value;
    const saved = await sendJSON('POST', docURL(d.id, '/annotations'), { revision: d.revisions, anchor: newAnchor, body, attachments });
    if (!saved) return;
    closeNew();
    await loadAnnotations();
    focusItem(saved.id, true);
  })().catch((err) => { $('annot-status').textContent = 'Not saved: ' + err.message; });
});

async function loadAnnotations() {
  const d = openDoc;
  if (!d) return;
  const res = await fetch(docURL(d.id, '/annotations'));
  if (res.status === 401) return;
  if (!res.ok) throw new Error('annotations: HTTP ' + res.status);
  const list = await res.json();
  // The viewer moved on while this was in flight.
  if (!openDoc || openDoc.id !== d.id) return;
  annotations = list;
  renderAnnotations();
  sendAnnotations();
}

// rank orders the panel: whole-document comments, then marks, then orphans.
const rank = (a) => (a.anchor ? (a.orphaned ? 2 : 1) : 0);

function renderAnnotations() {
  const list = $('annot-list');
  const kept = new Map([...list.children].map((li) => [li.dataset.id, li]));
  const items = [...annotations].sort((x, y) => rank(x) - rank(y) || x.id - y.id)
    .map((a) => fillItem(kept.get(String(a.id)) || newItem(a.id), a));
  // Re-inserting blurs: only reorder when the order changed, and give focus back.
  if (items.length !== list.children.length || items.some((li, i) => list.children[i] !== li)) {
    const focused = document.activeElement;
    list.replaceChildren(...items);
    if (focused && list.contains(focused)) focused.focus();
  }
}

function focusItem(id, quiet) {
  const li = $('annot-list').querySelector(`li[data-id="${id}"]`);
  if (!li) return;
  $('annot-list').querySelectorAll('li.active').forEach((x) => x.classList.remove('active'));
  li.classList.add('active');
  li.scrollIntoView({ block: 'nearest' });
  if (!quiet) li.querySelector('textarea').focus();
  postToFrame({ type: 'gr:focus', id });
}

function newItem(id) {
  const li = document.createElement('li');
  li.className = 'annot';
  li.dataset.id = id;
  li.innerHTML = '<button type="button" class="annot-head"><span class="annot-badge"></span> <span class="annot-label"></span></button>'
    + '<p class="annot-note"></p><ol class="annot-thread"></ol>'
    + '<form class="annot-reply"><textarea rows="2" maxlength="4000" aria-label="Reply" placeholder="Reply"></textarea>'
    + '<label class="annot-file">Images <input type="file" accept="image/png,image/jpeg,image/gif,image/webp" multiple></label>'
    + '<div class="annot-actions"><button type="button" class="annot-state"></button><button type="submit">Reply</button></div>'
    + '<p class="annot-error" role="status"></p></form>';
  li.querySelector('.annot-head').addEventListener('click', () => focusItem(id, true));
  const form = li.querySelector('form');
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    (async () => {
      const attachments = await readImages(form.querySelector('input[type=file]'));
      const body = form.querySelector('textarea').value;
      if (!(await sendJSON('POST', api + '/annotations/' + id + '/messages', { body, attachments }))) return;
      form.reset();
      form.querySelector('.annot-error').textContent = '';
      await loadAnnotations();
    })().catch((err) => { form.querySelector('.annot-error').textContent = 'Not sent: ' + err.message; });
  });
  li.querySelector('.annot-state').addEventListener('click', () => {
    const status = li.dataset.status === 'done' ? 'reopened' : 'done';
    sendJSON('PATCH', api + '/annotations/' + id, { status })
      .then(loadAnnotations)
      .catch((err) => { form.querySelector('.annot-error').textContent = err.message; });
  });
  return li;
}

function fillItem(li, a) {
  li.dataset.status = a.status;
  li.classList.toggle('orphaned', a.orphaned);
  const badge = li.querySelector('.annot-badge');
  badge.className = 'annot-badge ' + a.status;
  badge.textContent = a.status;
  li.querySelector('.annot-label').textContent = describeAnchor(a.anchor);
  const note = li.querySelector('.annot-note');
  note.hidden = !a.orphaned;
  note.textContent = a.orphaned ? 'Gone from the current revision, last seen in r' + a.revision + '.' : '';
  li.querySelector('.annot-state').textContent = a.status === 'done' ? 'Reopen' : 'Mark done';
  li.querySelector('.annot-thread').replaceChildren(...a.messages.map((m) => {
    const item = document.createElement('li');
    item.className = 'annot-msg ' + m.by;
    const meta = document.createElement('div');
    meta.className = 'meta';
    const who = document.createElement('strong');
    who.textContent = m.author;
    const time = document.createElement('time');
    time.dateTime = m.created_at;
    time.textContent = new Date(m.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    meta.append(who, ' ', time);
    item.append(meta);
    if (m.body) {
      const body = document.createElement('div');
      body.className = 'body';
      body.textContent = m.body;
      item.append(body);
    }
    for (const at of m.attachments) {
      const img = document.createElement('img');
      img.src = api + '/attachments/' + encodeURIComponent(at.id);
      img.alt = 'Image from ' + m.author;
      img.loading = 'lazy';
      item.append(img);
    }
    return item;
  }));
  return li;
}

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
  events.addEventListener('annotations', () => loadAnnotations().catch(console.error));
  events.addEventListener('revoked', () => end('Your access to this room has ended.'));
  events.addEventListener('stt', (e) => showSTT(JSON.parse(e.data)));
  events.addEventListener('agent', (e) => {
    agent = JSON.parse(e.data);
    renderAgent();
    renderParticipants();
  });
  events.onerror = () => {
    // CLOSED means the browser gave up (a 401 on reconnect); otherwise it retries by itself.
    if (events.readyState === EventSource.CLOSED) loadRoom().catch(console.error);
  };
}

// Agent. The host's daemon runs it: the LED is green only while that daemon is connected and can
// host (D8). The host, and the participants the host allows, can press the button (D18).
let agent = { online: false, can_trigger: [], daemons: [], turn: null };

const isHost = () => String(agent.host_participant_id || '') === myID;

function renderAgent() {
  const running = !!agent.turn && agent.turn.status === 'running';
  const led = $('agent-led');
  led.classList.toggle('on', agent.online && !running);
  led.classList.toggle('busy', agent.online && running);
  led.setAttribute('aria-label', !agent.online ? 'Agent offline' : running ? 'Agent working' : 'Agent ready');
  $('agent-status').textContent = !agent.online ? (agent.reason || 'Agent offline') : running ? 'Agent working…' : '';
  const allowed = isHost() || agent.can_trigger.includes(Number(myID));
  const press = $('agent-press');
  press.disabled = !agent.online || running || !allowed;
  press.title = allowed ? '' : 'The host has not given you the agent button';
  $('agent-fresh').hidden = !isHost();
  $('agent-fresh').disabled = press.disabled;
}

async function pressAgent(fresh) {
  $('agent-press').disabled = true;
  $('agent-fresh').disabled = true;
  try {
    await sendJSON('POST', api + '/turns', { fresh });
  } catch (err) {
    renderAgent();
    $('agent-status').textContent = err.message;
  }
}

$('agent-press').addEventListener('click', () => pressAgent(false));
$('agent-fresh').addEventListener('click', () => {
  if (confirm('Start a new agent session? The agent forgets its earlier turns and rereads the room history.')) pressAgent(true);
});

// agentControls shows who hosts and, to the host, the button right and the hand-over per row.
function agentControls(li, p) {
  const id = p.id;
  const host = agent.host_participant_id === id;
  let badge = li.querySelector('.host-badge');
  if (host && !badge) {
    badge = document.createElement('span');
    badge.className = 'host-badge';
    badge.textContent = 'host';
    li.querySelector('.name').after(badge);
  } else if (!host && badge) {
    badge.remove();
  }
  const manage = isHost() && !host;
  let trigger = li.querySelector('label.trigger');
  if (manage && !trigger) {
    trigger = document.createElement('label');
    trigger.className = 'trigger';
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.setAttribute('aria-label', 'Let ' + p.name + ' press the agent button');
    box.addEventListener('change', () => {
      sendJSON('PATCH', api + '/participants/' + id, { can_trigger: box.checked })
        .catch((err) => { box.checked = !box.checked; $('agent-status').textContent = err.message; });
    });
    trigger.append(box, 'can ask');
    li.append(trigger);
  } else if (!manage && trigger) {
    trigger.remove();
  }
  if (trigger) trigger.querySelector('input').checked = agent.can_trigger.includes(id);
  const daemon = agent.daemons.find((d) => d.participant_id === id && d.can_host);
  let handOver = li.querySelector('button.make-host');
  if (manage && daemon && !handOver) {
    handOver = document.createElement('button');
    handOver.type = 'button';
    handOver.className = 'make-host';
    handOver.textContent = 'Make host';
    handOver.setAttribute('aria-label', 'Make ' + p.name + ' the agent host');
    handOver.addEventListener('click', () => {
      sendJSON('PUT', api + '/host', { participant_id: id }).catch((err) => { $('agent-status').textContent = err.message; });
    });
    li.append(handOver);
  } else if (!(manage && daemon) && handOver) {
    handOver.remove();
  }
}

// Voice. One LiveKit room per grill room; a participant's LiveKit identity is its participant id,
// so each audio track, speaking flag and transcript line maps back to a name in the list.
let voice = null;
let speaking = new Set();
const volumes = new Map();

function voiceParticipant(id) {
  if (!voice) return null;
  return id === myID ? voice.localParticipant : voice.remoteParticipants.get(id);
}

// renderParticipants updates rows in place: speaker changes fire many times a second, and a
// rebuilt row would drop a volume slider mid-drag.
function renderParticipants() {
  const list = $('participants');
  const kept = new Map([...list.children].map((li) => [li.dataset.pid, li]));
  const rows = participants.map((p) => {
    const id = String(p.id);
    let li = kept.get(id);
    if (!li) {
      li = document.createElement('li');
      li.dataset.pid = id;
      const dot = document.createElement('span');
      dot.className = 'voice-dot';
      const name = document.createElement('span');
      name.className = 'name';
      li.append(dot, name);
    }
    li.querySelector('.name').textContent = p.name;
    const vp = voiceParticipant(id);
    const mic = !!vp && vp.isMicrophoneEnabled;
    li.classList.toggle('in-voice', !!vp);
    li.classList.toggle('speaking', mic && speaking.has(id));
    li.classList.toggle('muted', !!vp && !mic);
    li.querySelector('.voice-dot').title = !vp ? '' : !mic ? 'Muted' : speaking.has(id) ? 'Speaking' : 'In the audio';
    let vol = li.querySelector('input[type=range]');
    if (vp && id !== myID && !vol) {
      vol = document.createElement('input');
      vol.type = 'range';
      vol.min = '0';
      vol.max = '1';
      vol.step = '0.05';
      vol.value = String(volumes.get(id) ?? 1);
      vol.setAttribute('aria-label', 'Volume of ' + p.name);
      vol.addEventListener('input', () => {
        volumes.set(id, Number(vol.value));
        const rp = voiceParticipant(id);
        if (rp) rp.setVolume(Number(vol.value));
      });
      li.append(vol);
    } else if (!vp && vol) {
      vol.remove();
    }
    agentControls(li, p);
    return li;
  });
  if (rows.length !== list.children.length || rows.some((li, i) => list.children[i] !== li)) list.replaceChildren(...rows);
}

function showSTT(st) {
  const el = $('stt-status');
  el.classList.toggle('down', !st.available);
  el.textContent = !st.available ? 'Transcription unavailable' : st.listening ? 'Transcribing' : '';
}

function voiceError(text) {
  $('voice-error').textContent = text;
  $('voice-error').hidden = !text;
}

function voiceButtons() {
  const on = !!voice;
  $('voice-join').hidden = on;
  $('voice-leave').hidden = !on;
  $('voice-mute').hidden = !on;
  const muted = on && !voice.localParticipant.isMicrophoneEnabled;
  $('voice-mute').setAttribute('aria-pressed', String(muted));
  $('voice-mute').textContent = muted ? 'Unmute' : 'Mute';
  $('voice-unlock').hidden = !on || voice.canPlaybackAudio;
}

function voiceChanged() {
  voiceButtons();
  renderParticipants();
}

async function joinVoice() {
  if (voice) return;
  voiceError('');
  $('voice-join').disabled = true;
  let room;
  try {
    const res = await fetch(api + '/voice');
    if (res.status === 401) return end('Your access to this room has ended.');
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || 'HTTP ' + res.status);
    const { url, token } = await res.json();
    const LK = LivekitClient;
    room = new LK.Room({ audioCaptureDefaults: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
    room.on(LK.RoomEvent.TrackSubscribed, (track, _pub, p) => {
      if (track.kind !== 'audio') return;
      const el = track.attach();
      el.dataset.pid = p.identity;
      $('voice-audio').append(el);
      if (volumes.has(p.identity)) p.setVolume(volumes.get(p.identity));
    });
    room.on(LK.RoomEvent.TrackUnsubscribed, (track) => track.detach().forEach((el) => el.remove()));
    room.on(LK.RoomEvent.ActiveSpeakersChanged, (list) => {
      speaking = new Set(list.map((p) => p.identity));
      renderParticipants();
    });
    for (const ev of ['ParticipantConnected', 'ParticipantDisconnected', 'TrackMuted', 'TrackUnmuted',
      'LocalTrackPublished', 'LocalTrackUnpublished', 'AudioPlaybackStatusChanged']) {
      room.on(LK.RoomEvent[ev], voiceChanged);
    }
    room.on(LK.RoomEvent.Disconnected, (reason) => {
      if (voice !== room) return;
      cleanupVoice();
      // Leaving on purpose is CLIENT_INITIATED; anything else, a kick included, is worth saying.
      if (reason !== LK.DisconnectReason.CLIENT_INITIATED) voiceError('You were disconnected from the audio.');
    });
    voice = room;
    await room.connect(url, token);
    if (voice !== room) return;
    try {
      await room.localParticipant.setMicrophoneEnabled(true);
    } catch (err) {
      // Without a microphone the participant still hears the others.
      voiceError('Microphone unavailable: ' + err.message + '. You can still listen.');
    }
    voiceChanged();
  } catch (err) {
    // A Leave pressed while connecting rejects connect(): nothing to report then.
    if (room && voice !== room) return;
    leaveVoice();
    voiceError('Could not join the audio: ' + err.message);
  } finally {
    $('voice-join').disabled = false;
  }
}

function cleanupVoice() {
  voice = null;
  speaking = new Set();
  $('voice-audio').replaceChildren();
  voiceChanged();
}

function leaveVoice() {
  const room = voice;
  if (!room) return;
  cleanupVoice();
  room.disconnect().catch(console.error);
}

$('voice-join').addEventListener('click', () => joinVoice());
$('voice-leave').addEventListener('click', leaveVoice);
$('voice-mute').addEventListener('click', () => {
  if (!voice) return;
  voice.localParticipant.setMicrophoneEnabled(!voice.localParticipant.isMicrophoneEnabled)
    .then(voiceChanged)
    .catch((err) => voiceError('Microphone: ' + err.message));
});
// Browsers block playback without a gesture; LiveKit flags it and this button lifts it.
$('voice-unlock').addEventListener('click', () => voice && voice.startAudio().then(voiceChanged).catch(console.error));

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

// Pairing: the daemon's local page opens this room with ?pair=<id>. Approving here, in the
// participant's own session, hands the daemon its token; nobody sees or types it.
async function showPairing(id) {
  history.replaceState(null, '', location.pathname);
  const url = api + '/pairings/' + encodeURIComponent(id);
  const text = $('pairing-text');
  $('pairing').hidden = false;
  const res = await fetch(url);
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    text.textContent = data.error || 'This pairing request could not be loaded.';
    $('pairing-actions').hidden = true;
    return;
  }
  const p = await res.json();
  text.textContent = 'The agent daemon on "' + p.name + '" asks to run the agent for you in this room, with your Claude subscription. ' +
    'Approve only if your daemon page shows the code ' + p.code + '.';
  const answer = async (approve) => {
    $('pairing-actions').hidden = true;
    const r = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ approve }),
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) text.textContent = data.error || 'Pairing failed: HTTP ' + r.status;
    else text.textContent = approve ? 'Daemon paired. Its page shows it connected within a few seconds.' : 'Pairing denied.';
  };
  $('pairing-approve').onclick = () => answer(true).catch(console.error);
  $('pairing-deny').onclick = () => answer(false).catch(console.error);
}

// Host the agent: the daemon download for the visitor's platform and the install one-liner (AC36).
// Chromium tells the CPU through userAgentData; Safari and Firefox hide it, so the guess is editable.
async function guessPlatform() {
  const ua = navigator.userAgent;
  let os = /Mac/.test(navigator.platform) || /Macintosh/.test(ua) ? 'darwin' : /Win/.test(navigator.platform) ? 'wsl' : 'linux';
  // Every Mac sold since 2023 is Apple silicon.
  let arch = os === 'darwin' || /aarch64|arm64/i.test(ua) ? 'arm64' : 'amd64';
  const uad = navigator.userAgentData;
  if (uad && uad.getHighEntropyValues) {
    try {
      const h = await uad.getHighEntropyValues(['architecture', 'platform']);
      os = { macOS: 'darwin', Windows: 'wsl', Linux: 'linux', 'Chrome OS': 'linux' }[h.platform] || os;
      arch = { arm: 'arm64', x86: 'amd64' }[h.architecture] || arch;
    } catch { /* keep the user-agent guess */ }
  }
  return { os, arch };
}

async function showHostAgent() {
  const res = await fetch('/api/daemon/version');
  if (!res.ok) return;
  const rel = await res.json();
  if (!rel.version) return;
  const { os, arch } = await guessPlatform();
  $('host-os').value = os;
  $('host-arch').value = arch;
  const render = () => {
    const target = ($('host-os').value === 'darwin' ? 'darwin' : 'linux') + '-' + $('host-arch').value;
    const link = $('host-download');
    link.href = '/download/grill-room-daemon-' + target;
    link.textContent = 'Download grill-room-daemon ' + rel.version + ' (' + target + ')';
    $('host-sha').textContent = 'sha256 ' + (rel.sha256[target] || '');
    $('host-wsl').hidden = $('host-os').value !== 'wsl';
  };
  $('host-os').addEventListener('change', render);
  $('host-arch').addEventListener('change', render);
  render();
  const oneLiner = 'curl -fsSL ' + location.origin + '/install.sh | sh -s -- ' + location.origin + location.pathname;
  $('host-oneliner').textContent = oneLiner;
  $('host-copy').addEventListener('click', () => {
    navigator.clipboard.writeText(oneLiner).then(() => { $('host-copy').textContent = 'Copied'; }, console.error);
  });
  $('host-agent').hidden = false;
}

loadRoom().then((ok) => {
  if (!ok) return;
  connect();
  showHostAgent().catch(console.error);
  const pair = new URLSearchParams(location.search).get('pair');
  if (pair) showPairing(pair).catch(console.error);
}).catch(console.error);
