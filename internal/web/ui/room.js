'use strict';

const $ = (id) => document.getElementById(id);
const roomID = decodeURIComponent(location.pathname.split('/')[2] || '');
const api = '/api/rooms/' + encodeURIComponent(roomID);
const messages = $('messages');
const sendForm = $('send');
const bodyInput = $('body');
let events;
// lastID is the newest message shown: a reopened stream resumes after it. editSeq is the last
// edit or delete applied: the stream replays the ones past it.
let lastID = 0;
let editSeq = 0;
let retryTimer;
let ended = false;
let refreshTimer;
let wasIn = false;
let participants = [];
let myID = '';
// tab names this page to the server's presence: its stream and heartbeats (005-AC6).
const tab = (crypto.randomUUID ? crypto.randomUUID() : String(Math.random()).slice(2)).replaceAll('-', '');
// presence holds the server's "presence" event by participant id; transcribed, the participants
// the STT worker hears.
let presence = new Map();
let transcribed = new Set();

function end(text) {
  ended = true;
  clearTimeout(retryTimer);
  if (events) events.close();
  $('reconnecting').hidden = true;
  leaveVoice();
  $('voice').hidden = true;
  $('banner').textContent = text;
  $('banner').hidden = false;
  sendForm.hidden = true;
  document.querySelector('.upload').hidden = true;
  $('manage-open').hidden = true;
  if ($('manage').open) $('manage').close();
  $('invites').hidden = true;
  $('agent').hidden = true;
  if ($('personas').open) $('personas').close();
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
  // Every member edits the transcription glossary there (005-AC23); the rest is for room admins.
  $('manage-open').hidden = false;
  renderParticipants();
  // The room admin panel (admin/room-admin.js) renders from the same answer.
  window.grillRoom = data;
  window.dispatchEvent(new CustomEvent('grill:room', { detail: data }));
  return true;
}

// The list shrinks under a keyboard, a toolbar or a wrapping header: keep the newest message in
// view when it was. Only a scroll up unpins, as scroll events land after the layout changed.
let pinned = true;
let lastTop = 0;
messages.addEventListener('scroll', () => {
  if (messages.scrollTop + messages.clientHeight >= messages.scrollHeight - 40) pinned = true;
  else if (messages.scrollTop < lastTop) pinned = false;
  lastTop = messages.scrollTop;
});
new ResizeObserver(() => { if (pinned) messages.scrollTop = messages.scrollHeight; }).observe(messages);

function addMessage(m) {
  if (m.kind === 'system') {
    // A join changes the participant list; history replay sends many, so refresh once.
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => loadRoom().catch(console.error), 200);
  }
  messages.append(renderMessage(m));
  if (pinned) messages.scrollTop = messages.scrollHeight;
}

// An edit or delete replaces the line in place; one not shown yet arrives later as it stands. A
// replay of a change already shown leaves the line alone, with any edit box open on it.
function replaceMessage(m) {
  const li = messages.querySelector('li[data-id="' + m.id + '"]');
  if (li && m.change_seq > Number(li.dataset.seq)) li.replaceWith(renderMessage(m));
}

function button(text, label, onClick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'link';
  b.textContent = text;
  if (label) b.setAttribute('aria-label', label);
  b.addEventListener('click', onClick);
  return b;
}

function timeOf(ts) {
  const time = document.createElement('time');
  time.dateTime = ts;
  time.textContent = new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  time.title = new Date(ts).toLocaleString();
  return time;
}

function renderMessage(m) {
  const li = document.createElement('li');
  li.className = 'msg ' + m.kind;
  li.dataset.id = m.id;
  li.dataset.seq = m.change_seq || 0;
  if (m.kind === 'system') {
    // Event lines show their time as messages do (005-AC19).
    const body = document.createElement('span');
    body.className = 'body';
    body.textContent = m.body;
    li.append(timeOf(m.ts), ' ', body);
    return li;
  }
  const meta = document.createElement('div');
  meta.className = 'meta';
  const sender = document.createElement('strong');
  // An agent shows its persona's badge and colour (002-AC42).
  sender.textContent = m.badge ? m.badge + ' ' + m.sender : m.sender;
  if (m.colour) sender.style.color = m.colour;
  meta.append(sender, ' ', timeOf(m.ts));
  if (m.kind === 'agent') {
    // The model and effort the turn ran with (002-AC18), the model checked against what claude
    // reported (005-AC10). Lines from before 005-AC9 may carry no effort.
    const model = document.createElement('span');
    model.className = 'model';
    model.textContent = m.model || 'model unknown';
    model.title = 'Claude model of this turn';
    const ran = [model];
    if (m.effort) {
      const effort = document.createElement('span');
      effort.className = 'effort';
      effort.textContent = 'effort ' + m.effort;
      effort.title = 'Effort of this turn';
      ran.push(' · ', effort);
    }
    if (m.model_note || m.model_mismatch) {
      const note = document.createElement('span');
      note.className = 'model-note' + (m.model_mismatch ? ' mismatch' : '');
      note.textContent = m.model_mismatch ? '⚠ claude reported ' + (m.model_note || 'no model') : m.model_note;
      note.title = m.model_mismatch ? 'Claude did not report the model this turn asked for' : 'Other models of this turn, with their share of its cost';
      ran.push(' · ', note);
    }
    // The host cap lowered what the persona asked for (002-AC19).
    if (m.requested_model || m.requested_effort) {
      const asked = document.createElement('span');
      asked.className = 'capped';
      asked.textContent = 'requested ' + level({ model: m.requested_model, effort: m.requested_effort }) + ' → ran';
      const why = document.createElement('span');
      why.className = 'capped';
      why.textContent = '(host cap)';
      meta.append(' ', asked, ' ', ...ran, ' ', why);
    } else {
      meta.append(' ', ...ran);
    }
  }
  const body = document.createElement('div');
  body.className = 'body';
  if (m.deleted_at) {
    li.classList.add('deleted');
    body.textContent = 'message deleted';
    li.append(meta, body);
    return li;
  }
  // Rendered and sanitised by the server (render.MessageHTML).
  body.innerHTML = m.html || '';
  if (m.edited_at) {
    meta.append(' ', button('(edited)', 'Show earlier versions', () => toggleVersions(li, m).catch(console.error)));
  }
  if (String(m.participant_id) === myID && (m.kind === 'text' || m.kind === 'voice')) {
    meta.append(' ', button('Edit', '', () => editMessage(li, m)),
      ' ', button('Delete', '', () => deleteMessage(m)));
  }
  li.append(meta, body);
  return li;
}

async function toggleVersions(li, m) {
  const shown = li.querySelector('.versions');
  if (shown) {
    shown.remove();
    return;
  }
  const res = await fetch(api + '/messages/' + m.id + '/versions');
  if (!res.ok) return;
  const list = document.createElement('ol');
  list.className = 'versions';
  list.setAttribute('aria-label', 'Earlier versions');
  list.append(...(await res.json()).map((v) => {
    const item = document.createElement('li');
    const when = document.createElement('time');
    when.dateTime = v.written_at;
    when.textContent = new Date(v.written_at).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' });
    const text = document.createElement('span');
    text.textContent = v.body;
    item.append(when, ' ', text);
    return item;
  }));
  li.append(list);
}

function editMessage(li, m) {
  const form = document.createElement('form');
  form.className = 'edit';
  const box = document.createElement('textarea');
  box.value = m.body;
  box.rows = 2;
  box.maxLength = 4000;
  // Not "message": the send box is labelled Message.
  box.setAttribute('aria-label', 'New text');
  const save = document.createElement('button');
  save.textContent = 'Save';
  const cancel = button('Cancel', '', () => li.replaceWith(renderMessage(m)));
  form.append(box, save, ' ', cancel);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    sendJSON('PATCH', api + '/messages/' + m.id, { body: box.value }).then((saved) => {
      if (saved && form.isConnected) li.replaceWith(renderMessage(saved));
    }).catch((err) => { box.setCustomValidity(err.message); box.reportValidity(); });
  });
  box.addEventListener('input', () => box.setCustomValidity(''));
  box.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      form.requestSubmit();
    } else if (e.key === 'Escape') {
      cancel.click();
    }
  });
  li.querySelector('.body').replaceWith(form);
  box.focus();
}

function deleteMessage(m) {
  if (!confirm('Delete this message? It shows as deleted for everyone.')) return;
  fetch(api + '/messages/' + m.id, { method: 'DELETE' }).then((res) => {
    if (res.status === 401) end('Your access to this room has ended.');
  }).catch(console.error);
}

// Manage: a room admin's panel, rendered by admin/room-admin.js (004-AC8).
$('manage-open').addEventListener('click', () => $('manage').showModal());
$('manage-close').addEventListener('click', () => $('manage').close());

// copyField copies a read-only field, and selects it for a manual copy when the clipboard refuses.
function copyField(input, button) {
  navigator.clipboard.writeText(input.value).then(() => { button.textContent = 'Copied'; }, () => input.select());
}

// The join page leaves the recovery link of a profile it just created: shown here once.
function showRecovery() {
  const link = sessionStorage.getItem('grill_room_recovery');
  if (!link) return;
  sessionStorage.removeItem('grill_room_recovery');
  $('recovery-link').value = link;
  $('recovery').hidden = false;
}
$('recovery-copy').addEventListener('click', () => copyField($('recovery-link'), $('recovery-copy')));
$('recovery-done').addEventListener('click', () => {
  $('recovery-link').value = '';
  $('recovery').hidden = true;
});

// Invites: a member creates links and revokes the ones they created; a room admin, who gets every
// invite, manages the others from Manage.
async function loadInvites() {
  const res = await fetch(api + '/invites');
  if (!res.ok) return;
  const invites = (await res.json()).filter((i) => !i.revoked_at && String(i.created_by) === myID);
  $('invite-list').replaceChildren(...invites.map((inv) => {
    const li = document.createElement('li');
    const when = new Date(inv.created_at).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' });
    const label = document.createElement('span');
    label.textContent = 'Created ' + when;
    const revoke = document.createElement('button');
    revoke.type = 'button';
    revoke.textContent = 'Revoke';
    revoke.setAttribute('aria-label', 'Revoke the invite created ' + when);
    revoke.addEventListener('click', () => {
      sendJSON('DELETE', api + '/invites/' + inv.id).then(loadInvites).catch(console.error);
    });
    li.append(label, revoke);
    return li;
  }));
}
$('invites').addEventListener('toggle', () => { if ($('invites').open) loadInvites().catch(console.error); });
$('invite-create').addEventListener('click', () => {
  sendJSON('POST', api + '/invites').then((inv) => {
    if (!inv) return;
    $('invite-link').value = inv.url;
    $('invite-copy').textContent = 'Copy invite';
    $('invite-new').hidden = false;
    return loadInvites();
  }).catch(console.error);
});
$('invite-copy').addEventListener('click', () => copyField($('invite-link'), $('invite-copy')));

// Documents. Text documents render in an iframe sandboxed without allow-same-origin: their
// scripts get an opaque origin, so no room cookie and no same-origin call to the API.
let openDoc = null;

function docURL(id, suffix) {
  return api + '/documents/' + encodeURIComponent(id) + suffix;
}

function describe(d) {
  if (d.kind === 'agent') return 'agent document by ' + personaLabel(d.author) + ', ' + d.revisions + (d.revisions === 1 ? ' revision' : ' revisions');
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
    $('rev').replaceChildren(...revisions.map((r) => option(r.n, 'r' + r.n + ' · ' + personaLabel(r.author) + ' · ' + new Date(r.created_at).toLocaleString())));
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
    who.textContent = m.badge ? m.badge + ' ' + m.author : m.author;
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

// connect opens the event stream, replacing any previous one. A new EventSource cannot send
// Last-Event-ID, so it resumes with ?since=.
function connect() {
  clearTimeout(retryTimer);
  if (events) events.close();
  if (ended) return;
  const es = new EventSource(api + '/events?' + (lastID ? 'since=' + lastID + (editSeq ? '&edits=' + editSeq : '') + '&' : '') + 'tab=' + tab);
  events = es;
  es.onmessage = (e) => {
    const m = JSON.parse(e.data);
    if (m.id <= lastID) return;
    lastID = m.id;
    addMessage(m);
  };
  // Listing on open, not before: the stream announces only changes made after it opened.
  es.onopen = () => {
    $('reconnecting').hidden = true;
    beat();
    loadDocuments().then(loadAnnotations).catch(console.error);
  };
  es.addEventListener('message_edited', (e) => {
    const m = JSON.parse(e.data);
    if (m.change_seq > editSeq) editSeq = m.change_seq;
    replaceMessage(m);
  });
  es.addEventListener('documents', () => loadDocuments().catch(console.error));
  es.addEventListener('annotations', () => loadAnnotations().catch(console.error));
  es.addEventListener('revoked', () => end('Your access to this room has ended.'));
  es.addEventListener('stt', (e) => showSTT(JSON.parse(e.data)));
  es.addEventListener('presence', (e) => {
    presence = new Map(JSON.parse(e.data).people.map((p) => [String(p.id), p]));
    renderParticipants();
  });
  es.addEventListener('agent', (e) => {
    agent = JSON.parse(e.data);
    renderAgent();
    renderParticipants();
    window.grillAgent = agent;
    window.dispatchEvent(new CustomEvent('grill:agent', { detail: agent }));
  });
  es.onerror = () => {
    if (es !== events) return;
    $('reconnecting').hidden = false;
    // CLOSED: the browser gave up, on a 401 or on the proxy's error page during a redeploy. It
    // retries by itself otherwise.
    if (es.readyState === EventSource.CLOSED) retryTimer = setTimeout(resume, 3000);
  };
}

// resume reloads the room, then reopens the stream from the last message held. A failed load
// (the server is still down) retries.
function resume() {
  clearTimeout(retryTimer);
  if (ended) return;
  loadRoom().then((ok) => { if (ok) connect(); }).catch((err) => {
    console.error(err);
    if (!ended) retryTimer = setTimeout(resume, 3000);
  });
}

// A phone suspends a background page's stream, and the page may come back holding one that looks
// open but delivers nothing: reopen it whenever the page returns.
document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible' && events) resume(); });
window.addEventListener('pageshow', (e) => { if (e.persisted && events) resume(); });
window.addEventListener('online', () => { if (events) resume(); });
window.addEventListener('focus', () => { if (events && events.readyState !== EventSource.OPEN) resume(); });

// Agents. The host's daemon runs every persona of the room: a persona's LED is green only while
// that daemon is connected and can host (D8). The host, and the participants the host allows,
// press a persona's button (D18); a press during a turn waits in the room's queue (002-AC17).
let agent = { online: false, can_trigger: [], daemons: [], turn: null, running: [], personas: [], queue: [], models: [], efforts: [], cap_requests: [],
  changes: [], room_skills: [], chain: { budget: 25, count: 0, suspended: false }, parallel_turns: false };

// level shows a model and effort as the room says them, "opus · effort max". Both are always set
// since 005-AC9; "unset" only shows a line written before.
const level = (l) => (l.model || 'unset') + ' · effort ' + (l.effort || 'unset');

const isHost = () => String(agent.host_participant_id || '') === myID;
const canPress = () => isHost() || agent.can_trigger.includes(Number(myID));
const personaByID = (id) => agent.personas.find((p) => p.id === id);
// contextUse is the share of its model's window that a persona's session holds, in percent; null
// while unknown (003-AC24).
const contextUse = (p) => (p.context_window ? Math.round((100 * (p.context_tokens || 0)) / p.context_window) : null);
// Above this use the room offers the host a new session with a handoff note (003-AC25).
const contextFull = 80;

function handOff(p) {
  if (!confirm('New session for ' + p.name + ' with a handoff? Its last turn on this session writes docs/_handoff-' + p.name
    + '.md, then a new session starts from that note and the transcripts.')) return Promise.resolve();
  return sendJSON('POST', api + '/turns', { persona_id: p.id, handoff: true });
}
// A persona signs documents and revisions by name: show its badge while one holds that name.
function personaLabel(name) {
  const p = agent.personas.find((x) => x.name === name);
  return p ? p.badge + ' ' + name : name;
}

// A turn the host's machine holds in its own queue, behind turns of other rooms, is "queued (host
// busy)" (003-AC18).
const runningTurn = (p) => (agent.running || []).find((t) => t.persona_id === p.id);
const turnText = (p, t) => p.name + (t.host_queued ? ' queued (host busy)' : ' working');

function personaState(p) {
  const running = runningTurn(p);
  const pos = agent.queue.findIndex((q) => q.persona_id === p.id);
  if (p.pending) return { cls: 'pending', label: p.name + ' waits for the host\'s approval' };
  if (!agent.online) return { cls: '', label: p.name + ' offline' };
  if (running) return { cls: running.host_queued ? 'queued' : 'busy', label: turnText(p, running) };
  if (pos >= 0) return { cls: 'queued', label: p.name + ' queued, position ' + (pos + 1) };
  return { cls: 'on', label: p.name + ' ready' };
}

function renderAgent() {
  const bar = $('persona-buttons');
  const kept = new Map([...bar.children].map((b) => [b.dataset.id, b]));
  const buttons = agent.personas.map((p) => {
    let b = kept.get(String(p.id));
    if (!b) {
      b = document.createElement('button');
      b.type = 'button';
      b.className = 'persona';
      b.dataset.id = p.id;
      const led = document.createElement('span');
      led.className = 'led';
      led.setAttribute('role', 'img');
      const badge = document.createElement('span');
      badge.className = 'badge';
      const name = document.createElement('span');
      name.className = 'pname';
      b.append(led, badge, name);
      b.addEventListener('click', () => pressPersona(Number(b.dataset.id), false));
    }
    const st = personaState(p);
    b.style.setProperty('--persona', p.colour);
    b.setAttribute('aria-label', 'Ask ' + p.name);
    b.dataset.name = p.name;
    b.querySelector('.badge').textContent = p.badge;
    b.querySelector('.pname').textContent = p.name;
    const led = b.querySelector('.led');
    led.className = 'led ' + st.cls;
    led.setAttribute('aria-label', st.label);
    b.disabled = !agent.online || !canPress() || st.cls === 'queued' || st.cls === 'pending';
    b.title = !canPress() ? 'The host has not given you the agent button' : st.label;
    return b;
  });
  if (buttons.length !== bar.children.length || buttons.some((b, i) => bar.children[i] !== b)) bar.replaceChildren(...buttons);
  const running = (agent.running || []).map((t) => [personaByID(t.persona_id), t]).filter(([p]) => p);
  $('agent-status').textContent = !agent.online ? (agent.reason || 'Agent offline')
    : running.length ? running.map(([p, t]) => turnText(p, t)).join(', ') + '…' : agent.personas.length === 0 ? 'No persona: add one.' : '';
  renderQueue();
  renderApprovals();
  renderChain();
  renderParallel();
  renderPersonas();
}

// renderParallel shows the room's parallel mode; only the host switches it (003-AC20).
function renderParallel() {
  const box = $('parallel-turns');
  if (!box.dataset.busy) box.checked = !!agent.parallel_turns;
  box.disabled = !isHost();
}
$('parallel-turns').addEventListener('change', () => {
  const box = $('parallel-turns');
  box.dataset.busy = '1';
  sendJSON('PATCH', api + '/agent', { parallel_turns: box.checked })
    .catch((err) => { box.checked = !box.checked; personasStatus(err.message); })
    .finally(() => { delete box.dataset.busy; });
});

// renderChain shows the agent chain: its count, a banner once it reached its budget (re-arm or
// suspend the limit), and a permanent warning while the limit is suspended (002-AC25, 002-AC27).
function renderChain() {
  const c = agent.chain;
  const paused = !c.suspended && c.count >= c.budget;
  $('chain-count').textContent = c.suspended ? 'Chain ' + c.count + ', no limit' : 'Chain ' + c.count + '/' + c.budget;
  $('chain-unlimited').hidden = !c.suspended;
  $('chain-paused').hidden = !paused;
  $('chain-paused-text').textContent = 'The agent chain reached its budget of ' + c.budget + ' turns: agents cannot call each other until it is re-armed.'
    + (canPress() ? '' : ' The host, or someone they allow to press, can re-arm it.');
  for (const id of ['chain-rearm', 'chain-suspend', 'chain-restore']) $(id).hidden = !canPress();
  if (document.activeElement !== $('chain-budget')) $('chain-budget').value = c.budget;
  $('chain-budget').disabled = !canPress();
}

function steerChain(body) {
  return sendJSON('PATCH', api + '/chain', body).catch((err) => { $('agent-status').textContent = err.message; });
}
$('chain-rearm').addEventListener('click', () => steerChain({ rearm: true }));
$('chain-suspend').addEventListener('click', () => {
  if (confirm('Suspend the chain limit? Agents may then call each other without end, on the host\'s subscription.')) steerChain({ suspended: true });
});
$('chain-restore').addEventListener('click', () => steerChain({ suspended: false }));
$('chain-form').addEventListener('submit', (e) => {
  e.preventDefault();
  sendJSON('PATCH', api + '/chain', { budget: Number($('chain-budget').value) })
    .then((c) => { if (c) personasStatus('Chain budget set to ' + c.budget + '.'); })
    .catch((err) => personasStatus(err.message));
});

// renderApprovals is the banner of what waits for the host: personas asking above the host cap
// (002-AC19) and persona changes held while the host approves everything (002-AC21). Everyone sees
// it; only the host answers.
function renderApprovals() {
  const items = [];
  const decide = (li, what, path, body) => {
    if (!isHost()) {
      li.append(' The host decides.');
      return;
    }
    for (const approve of [true, false]) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'small';
      b.textContent = approve ? 'Approve' : 'Refuse';
      b.setAttribute('aria-label', (approve ? 'Approve ' : 'Refuse ') + what);
      b.addEventListener('click', () => {
        sendJSON('POST', api + path, { ...body, approve }).catch((err) => { $('agent-status').textContent = err.message; });
      });
      li.append(' ', b);
    }
  };
  for (const r of agent.cap_requests || []) {
    const p = personaByID(r.persona_id);
    if (!p) continue;
    const li = document.createElement('li');
    li.textContent = p.badge + ' ' + p.name + ' asks for ' + level(r.asks) + ', above the host cap: it runs at ' + level(r.runs) + '.';
    decide(li, level(r.asks) + ' for ' + p.name, '/personas/' + p.id + '/cap', { model: r.asks.model || '', effort: r.asks.effort || '' });
    items.push(li);
  }
  for (const p of agent.personas) {
    const use = contextUse(p);
    if (use === null || use < contextFull) continue;
    const li = document.createElement('li');
    li.textContent = p.badge + ' ' + p.name + '\'s session holds ' + use + '% of its context window.';
    if (isHost()) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'small';
      b.textContent = 'New session with handoff';
      b.setAttribute('aria-label', 'New session with handoff for ' + p.name);
      b.addEventListener('click', () => handOff(p).catch((err) => { $('agent-status').textContent = err.message; }));
      li.append(' ', b);
    } else {
      li.append(' The host can start a new session with a handoff.');
    }
    items.push(li);
  }
  for (const c of agent.changes || []) {
    const p = personaByID(c.persona_id);
    if (!p) continue;
    const li = document.createElement('li');
    li.textContent = c.kind === 'add'
      ? p.badge + ' ' + p.name + ', added by ' + c.requested_by + ': ' + c.summary + '.'
      : c.requested_by + ' asks to change ' + p.badge + ' ' + p.name + ': ' + c.summary + '.';
    decide(li, (c.kind === 'add' ? 'the persona ' : 'the change to ') + p.name, '/changes/' + c.id, {});
    items.push(li);
  }
  for (const k of (agent.room_skills || []).filter((x) => x.pending)) {
    const li = document.createElement('li');
    li.textContent = k.author + ' copied the skill ' + k.name + ' v' + k.version + ' into the room.';
    decide(li, 'the room skill ' + k.name, '/skills/' + k.id + '/decision', {});
    items.push(li);
  }
  $('approvals').hidden = items.length === 0;
  $('approval-list').replaceChildren(...items);
}

function renderQueue() {
  $('queue').hidden = agent.queue.length === 0;
  $('queue-list').replaceChildren(...agent.queue.map((q) => {
    const li = document.createElement('li');
    const p = personaByID(q.persona_id);
    const who = participants.find((x) => x.id === q.participant_id);
    const by = { mention: ', mentioned by ', call: ', called by an agent', every_n: ', every ' + (p ? p.every_n : 'N') + ' rounds' }[q.source];
    const label = (p ? p.badge + ' ' + p.name : 'a persona') + (q.cause ? ', ' + q.cause
      : (by === undefined ? ', asked by ' : by) + (q.participant_id ? (who ? who.name : 'someone') : ''));
    li.append(label);
    if (String(q.participant_id) === myID || (!q.participant_id && canPress())) {
      const cancel = document.createElement('button');
      cancel.type = 'button';
      cancel.className = 'small';
      cancel.textContent = 'Cancel';
      cancel.setAttribute('aria-label', 'Cancel the queued turn of ' + (p ? p.name : 'this persona'));
      cancel.addEventListener('click', () => {
        fetch(api + '/queue/' + q.id, { method: 'DELETE' }).catch(console.error);
      });
      li.append(' ', cancel);
    }
    return li;
  }));
}

async function pressPersona(id, fresh) {
  try {
    await sendJSON('POST', api + '/turns', { persona_id: id, fresh });
  } catch (err) {
    renderAgent();
    $('agent-status').textContent = err.message;
  }
}

// Personas panel: any member adds, edits, updates and removes the room's personas; a change
// applies from that persona's next turn and shows in the chat (002-AC14, 002-AC15).
let templates = [];

function personasStatus(text) {
  $('personas-status').textContent = text;
}

// picker offers values only: a persona always names its model and effort (005-AC9).
function picker(name, values, value) {
  const s = document.createElement('select');
  s.name = name;
  s.required = true;
  s.append(...values.map((v) => option(v, v)));
  s.value = value;
  return s;
}

function field(labelText, input) {
  const l = document.createElement('label');
  l.append(labelText, input);
  return l;
}

function personaForm(p) {
  const form = document.createElement('form');
  form.className = 'persona-form';
  form.dataset.id = p.id;
  form.dataset.seen = seenKey(p);
  form.style.setProperty('--persona', p.colour);
  const head = document.createElement('h3');
  head.textContent = p.badge + ' ' + p.name;
  const tpl = templates.find((t) => t.id === p.template_id);
  const from = document.createElement('p');
  from.className = 'hint';
  const use = contextUse(p);
  from.textContent = 'Runs on ' + level(p) + '. '
    + (tpl ? 'From "' + tpl.title + '" v' + p.template_version : 'Its template was removed from the library.')
    + (p.pending ? ' Waits for the host\'s approval.' : '')
    + (use === null ? '' : ' Its session holds ' + use + '% of its context window.');
  const input = (name, value, attrs) => {
    const i = document.createElement('input');
    i.name = name;
    i.value = value;
    Object.assign(i, attrs || {});
    return i;
  };
  const text = document.createElement('textarea');
  text.name = 'text';
  text.rows = 6;
  text.maxLength = 8000;
  text.value = p.text;
  const grid = document.createElement('div');
  grid.className = 'persona-fields';
  grid.append(
    field('Name', input('name', p.name, { maxLength: 30, required: true })),
    field('Badge', input('badge', p.badge, { maxLength: 8, required: true })),
    field('Colour', input('colour', p.colour, { type: 'color' })),
    field('Model', picker('model', agent.models, p.model)),
    field('Effort', picker('effort', agent.efforts, p.effort)),
    field('Every N rounds', input('every_n', p.every_n || 0, { type: 'number', min: 0, max: 100, title: '0 runs it on demand only' })),
    field('Autocompact (k tokens)', input('autocompact', p.autocompact ? p.autocompact / 1000 : '', { type: 'number', min: 100, max: 1000,
      placeholder: 'auto', title: 'Context size at which Claude Code compacts the session; empty for auto' })),
  );
  const self = input('self_call', '', { type: 'checkbox', checked: p.self_call });
  const selfCall = field('', self);
  selfCall.className = 'check';
  selfCall.append('Self-call: its own @' + p.name + ' gives it another turn');
  const actions = document.createElement('div');
  actions.className = 'persona-actions';
  const save = document.createElement('button');
  save.type = 'submit';
  save.textContent = 'Save';
  actions.append(save);
  const act = (label, aria, fn) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = label;
    b.setAttribute('aria-label', aria);
    b.addEventListener('click', () => fn().catch((err) => personasStatus(err.message)));
    actions.append(b);
    return b;
  };
  if (p.update_version) {
    act('Update to v' + p.update_version, 'Update ' + p.name + ' to template version ' + p.update_version, async () => {
      const done = await sendJSON('POST', api + '/personas/' + p.id + '/update');
      if (done) personasStatus(done.held ? p.name + ': the update waits for the host\'s approval.' : p.name + ' updated.');
    });
  }
  const mine = tpl && tpl.mine;
  act(mine ? 'Publish as v' + (tpl.version + 1) : 'Publish as a template', 'Publish ' + p.name + ' to the library', async () => {
    const body = formFields(form);
    if (mine) body.template_id = tpl.id;
    const t = await sendJSON('POST', api + '/templates', body);
    if (t) personasStatus('Published "' + t.title + '" v' + t.version + ' to the library.');
    await loadTemplates();
  });
  if (isHost()) {
    act('New session', 'Start a new session for ' + p.name, async () => {
      if (confirm('Start a new session for ' + p.name + '? It forgets its earlier turns and rereads the room history.')) await pressPersona(p.id, true);
    });
    act('New session with handoff', 'New session with handoff for ' + p.name, () => handOff(p));
  }
  act('Remove', 'Remove ' + p.name, async () => {
    if (!confirm('Remove ' + p.name + ' from the room? Its session goes; what it wrote stays.')) return;
    const res = await fetch(api + '/personas/' + p.id, { method: 'DELETE' });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || 'HTTP ' + res.status);
  });
  form.append(head, from, grid, selfCall, skillsBlock(p), field('Persona text', text), actions);
  form.addEventListener('input', () => { form.dataset.dirty = '1'; });
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    sendJSON('PATCH', api + '/personas/' + p.id, formFields(form)).then((saved) => {
      if (!saved) return;
      delete form.dataset.dirty;
      personasStatus(saved.held ? saved.name + ': the change waits for the host\'s approval.' : saved.name + ' saved. It applies from its next turn.');
    }).catch((err) => personasStatus(err.message));
  });
  return form;
}

// Skills: a member turns a skill of the library or one of their own on for a persona, pinned at
// its latest version; a newer upload is offered, never applied by itself (002-AC31). The first
// activation of a profile shows the disclaimer (002-AC32).
let skillLib = { skills: [], disclaimer: false, no_profile: false };

// scopeNote tells a personal or room skill from a shared one.
const scopeNote = (k) => ({ personal: ' (personal)', room: ' (room)' }[k.scope] || '');

// renderRoomSkills lists the room's own skills, and offers the member's personal skills to copy in
// (004-AC10).
function renderRoomSkills() {
  const own = agent.room_skills || [];
  $('room-skill-list').replaceChildren(...own.map((k) => {
    const li = document.createElement('li');
    li.dataset.skill = k.name;
    li.textContent = k.name + ' v' + k.version + ', copied by ' + k.author + (k.pending ? ': waits for the host\'s approval' : '');
    return li;
  }));
  if (!own.length) $('room-skill-list').append(Object.assign(document.createElement('li'), { textContent: 'None yet.' }));
  const mine = skillLib.skills.filter((x) => x.scope === 'personal' && !own.some((k) => k.name === x.name));
  $('room-skill-copy').hidden = !mine.length;
  const chosen = $('room-skill-source').value;
  $('room-skill-source').replaceChildren(...mine.map((x) => option(x.id, x.name + ' v' + x.version)));
  if (mine.some((x) => String(x.id) === chosen)) $('room-skill-source').value = chosen;
}
$('room-skill-copy').addEventListener('submit', (e) => {
  e.preventDefault();
  sendJSON('POST', api + '/skills', { skill_id: Number($('room-skill-source').value) }).then((k) => {
    if (k) personasStatus(k.name + ' v' + k.version + (k.pending ? ' copied. It waits for the host\'s approval.' : ' copied into the room.'));
  }).catch((err) => personasStatus(err.message)).finally(() => loadSkills().catch(console.error));
});

function skillsBlock(p) {
  const box = document.createElement('fieldset');
  box.className = 'persona-skills';
  const legend = document.createElement('legend');
  legend.textContent = 'Skills';
  const list = document.createElement('ul');
  const button = (label, aria, fn) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = label;
    b.setAttribute('aria-label', aria);
    b.addEventListener('click', () => fn().catch((err) => personasStatus(err.message)));
    return b;
  };
  for (const k of p.skills || []) {
    const li = document.createElement('li');
    li.dataset.skill = k.name;
    li.append(k.name + ' v' + k.version + scopeNote(k));
    // A personal skill's update is its owner's to take: only they see it in their library.
    if (k.update_version && skillLib.skills.some((x) => x.id === k.skill_id)) {
      li.append(button('Update to v' + k.update_version, 'Update skill ' + k.name + ' of ' + p.name + ' to version ' + k.update_version,
        () => turnSkillOn(p, k.skill_id)));
    }
    li.append(button('Turn off', 'Turn off skill ' + k.name + ' for ' + p.name, async () => {
      const done = await sendJSON('DELETE', api + '/personas/' + p.id + '/skills/' + k.skill_id);
      if (done) personasStatus(done.held ? p.name + ': the change waits for the host\'s approval.' : k.name + ' is off for ' + p.name + '.');
    }));
    list.append(li);
  }
  if (!list.children.length) list.append(Object.assign(document.createElement('li'), { textContent: 'None on.' }));
  box.append(legend, list);
  if (skillLib.no_profile) {
    box.append(Object.assign(document.createElement('p'), { className: 'hint', textContent: 'Turning skills on needs a profile.' }));
    return box;
  }
  const offered = skillLib.skills.filter((x) => !(p.skills || []).some((k) => k.skill_id === x.id));
  if (!offered.length) {
    box.append(Object.assign(document.createElement('p'), { className: 'hint', textContent: 'Upload skills from your profile page.' }));
    return box;
  }
  const add = document.createElement('div');
  add.className = 'skill-add';
  const sel = document.createElement('select');
  sel.setAttribute('aria-label', 'Skill to turn on for ' + p.name);
  sel.append(...offered.map((x) => option(x.id, x.name + ' v' + x.version + scopeNote(x))));
  // Picking a skill is not an edit of the persona's fields: the form keeps refreshing.
  sel.addEventListener('input', (e) => e.stopPropagation());
  add.append(sel, button('Turn on', 'Turn on the chosen skill for ' + p.name, () => turnSkillOn(p, Number(sel.value))));
  box.append(add);
  return box;
}

// skillsDisclaimer resolves true once the person accepts the disclaimer.
function skillsDisclaimer() {
  const d = $('skills-disclaimer');
  d.returnValue = '';
  d.showModal();
  return new Promise((resolve) => d.addEventListener('close', () => resolve(d.returnValue === 'accept'), { once: true }));
}

async function turnSkillOn(p, id) {
  const accept = skillLib.disclaimer && await skillsDisclaimer();
  if (skillLib.disclaimer && !accept) return;
  try {
    const done = await sendJSON('PUT', api + '/personas/' + p.id + '/skills/' + id, { accept_disclaimer: accept });
    if (done) {
      const k = (done.skills || []).find((x) => x.skill_id === id);
      personasStatus(done.held ? p.name + ': the change waits for the host\'s approval.' : (k ? k.name + ' v' + k.version + ' is on for ' + p.name + '.' : ''));
    }
  } finally {
    // A refusal may mean the library or the disclaimer state moved on: read them again.
    await loadSkills();
  }
}

async function loadSkills() {
  const [lib, st] = await Promise.all([fetch(api + '/skills'), fetch(api + '/agent')]);
  if (!lib.ok) return;
  skillLib = await lib.json();
  [...$('persona-list').children].forEach((f) => { if (!f.dataset.dirty) delete f.dataset.seen; });
  // An upload to the library does not wake the room: read the update offers afresh.
  if (st.ok) {
    agent = await st.json();
    renderAgent();
  } else {
    renderPersonas();
  }
}

// formFields reads the inputs' own values: FormData would turn the text's line breaks into CRLF.
function formFields(form) {
  const v = (name) => form.elements.namedItem(name).value;
  return { name: v('name'), badge: v('badge'), colour: v('colour'), model: v('model'), effort: v('effort'), text: v('text'),
    every_n: Number(v('every_n')), self_call: form.elements.namedItem('self_call').checked,
    autocompact: v('autocompact') ? Math.round(Number(v('autocompact')) * 1000) : 0 };
}

// seenKey is what a form shows: the persona, and whether the viewer hosts (New session).
const seenKey = (p) => JSON.stringify([p, isHost()]);

// renderPersonas refreshes the open panel; a form being edited keeps what its user typed.
function renderPersonas() {
  if (!$('personas').open) return;
  renderRoomSkills();
  const list = $('persona-list');
  const kept = new Map([...list.children].map((f) => [f.dataset.id, f]));
  list.replaceChildren(...agent.personas.map((p) => {
    const f = kept.get(String(p.id));
    if (f && (f.dataset.dirty || f.dataset.seen === seenKey(p))) return f;
    return personaForm(p);
  }));
}

async function loadTemplates() {
  const res = await fetch(api + '/templates');
  if (!res.ok) return;
  templates = await res.json();
  // A choice made while the library loaded survives it.
  const chosen = $('persona-template').value;
  $('persona-template').replaceChildren(...templates.map((t) =>
    option(t.id, t.badge + ' ' + t.title + ' v' + t.version + (t.preset ? '' : ', by ' + t.author))));
  if (templates.some((t) => String(t.id) === chosen)) $('persona-template').value = chosen;
  [...$('persona-list').children].forEach((f) => { if (!f.dataset.dirty) delete f.dataset.seen; });
  renderPersonas();
}

$('personas-open').addEventListener('click', () => {
  personasStatus('');
  $('persona-list').replaceChildren();
  $('personas').showModal();
  renderPersonas();
  loadTemplates().catch(console.error);
  loadSkills().catch(console.error);
});
$('personas-close').addEventListener('click', () => $('personas').close());
$('persona-add').addEventListener('submit', (e) => {
  e.preventDefault();
  sendJSON('POST', api + '/personas', { template_id: Number($('persona-template').value) })
    .then((p) => { if (p) personasStatus(p.name + (p.pending ? ' added. It waits for the host\'s approval.' : ' added.')); })
    .catch((err) => personasStatus(err.message));
});

// agentControls shows who hosts and, to the host, the button right and the hand-over per row.
function agentControls(li, p) {
  const id = p.id;
  const host = agent.host_participant_id === id;
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

// Each role badge and its tooltip (005-AC14).
const roles = [
  ['you', 'This is you', (p) => String(p.id) === myID],
  ['host', 'Host: holds the agents, which run on their machine', (p) => agent.host_participant_id === p.id],
  ['admin', 'Room admin: manages the room, its invites and its admins', (p) => p.admin],
];

// audioState is how a participant is in the audio: from the server's presence, which every
// viewer gets whether or not they joined the audio, with the LiveKit state for those they hear.
function audioState(id) {
  const pr = presence.get(id);
  const vp = voiceParticipant(id);
  const audio = !!(pr && pr.audio) || !!vp;
  if (!audio) return { key: 'none', icon: '', text: 'Not in the audio' };
  const mic = vp ? vp.isMicrophoneEnabled : !!(pr && pr.mic);
  const muted = !mic && (vp ? !!vp.getTrackPublication(LivekitClient.Track.Source.Microphone) : !!(pr && pr.muted));
  if (mic) return { key: 'live', icon: '🎙', text: 'Mic live' };
  if (muted) return { key: 'muted', icon: '🔇', text: 'Muted' };
  return { key: 'listening', icon: '🎧', text: 'Listening only' };
}

// ago says how long ago a time was, coarsely: the list refreshes every 30 seconds.
function ago(ts) {
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  if (s < 86400) return Math.floor(s / 3600) + ' h ago';
  return Math.floor(s / 86400) + ' d ago';
}

function span(cls, text, title) {
  const el = document.createElement('span');
  el.className = cls;
  el.textContent = text;
  if (title) el.title = title;
  return el;
}

// renderParticipants splits the list into those in the room and those not here (005-AC7). Rows
// update in place: speaker changes fire many times a second, and a rebuilt row would drop a
// volume slider mid-drag.
function renderParticipants() {
  const here = $('participants');
  const away = $('participants-away');
  const kept = new Map([...here.children, ...away.children].map((li) => [li.dataset.pid, li]));
  const rows = { here: [], away: [] };
  for (const p of participants) {
    const id = String(p.id);
    let li = kept.get(id);
    if (!li) {
      li = document.createElement('li');
      li.dataset.pid = id;
      li.append(span('voice-dot', ''), span('name', ''), span('badges', ''), span('audio', ''), span('seen', ''));
    }
    const pr = presence.get(id);
    const isHere = id === myID || !!(pr && pr.here);
    li.querySelector('.name').textContent = p.name;
    li.querySelector('.badges').replaceChildren(...roles.filter(([, , on]) => on(p))
      .map(([name, title]) => span('badge role-' + name + (name === 'host' ? ' host-badge' : ''), name, title)));
    const vp = voiceParticipant(id);
    const st = isHere ? audioState(id) : { key: 'none', icon: '', text: 'Not in the audio' };
    const deaf = isHere && !!(pr && pr.deaf);
    const audio = li.querySelector('.audio');
    audio.dataset.state = st.key;
    audio.textContent = st.icon + (deaf ? ' 🔕' : '');
    audio.title = st.text + (deaf ? ', deafened: hears nobody' : '');
    audio.setAttribute('aria-label', audio.title);
    let tr = li.querySelector('.transcribed');
    if (transcribed.has(id) && !tr) {
      tr = span('badge transcribed', 'transcribed', 'The transcription hears this voice');
      audio.after(tr);
    } else if (!transcribed.has(id) && tr) {
      tr.remove();
    }
    li.querySelector('.seen').textContent = isHere ? '' : 'seen ' + ago((pr && pr.seen_at) || p.last_seen_at || p.joined_at);
    li.classList.toggle('in-voice', st.key !== 'none');
    li.classList.toggle('speaking', st.key === 'live' && speaking.has(id));
    li.classList.toggle('muted', st.key === 'muted');
    li.classList.toggle('deaf', deaf);
    li.querySelector('.voice-dot').title = st.key === 'none' ? '' : st.key === 'live' && speaking.has(id) ? 'Speaking' : st.text;
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
    rows[isHere ? 'here' : 'away'].push(li);
  }
  for (const [list, want] of [[here, rows.here], [away, rows.away]]) {
    if (want.length !== list.children.length || want.some((li, i) => list.children[i] !== li)) list.replaceChildren(...want);
  }
  $('away').hidden = !rows.away.length;
  $('away-count').textContent = String(rows.away.length);
  $('people-open').textContent = 'People (' + rows.here.length + ')';
}
setInterval(() => { if (participants.length) renderParticipants(); }, 30_000);

// On a phone the list opens over the chat; Escape or the button closes it.
function showPeople(on) {
  document.body.classList.toggle('people-open', on);
  $('people-open').setAttribute('aria-expanded', String(on));
}
$('people-open').addEventListener('click', () => showPeople(!document.body.classList.contains('people-open')));
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') showPeople(false); });

// showSTT counts the voices the STT worker transcribes, and tells viewers only whether their own
// is one of them (005-AC8).
function showSTT(st) {
  const el = $('stt-status');
  const voices = st.transcribed || [];
  transcribed = new Set(voices.map(String));
  renderParticipants();
  const mine = voices.some((id) => String(id) === myID);
  el.classList.toggle('down', !st.available);
  el.textContent = !st.available ? 'Transcription unavailable'
    : !st.listening || !voices.length ? ''
    : (voices.length === 1 ? '1 voice' : voices.length + ' voices') + ' transcribed' + (mine ? ' · your voice is being transcribed' : '');
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
  $('voice-deaf').hidden = !on;
  $('voice-keepmic').hidden = !on;
  $('voice-deaf').setAttribute('aria-pressed', String(on && deaf));
  $('voice-deaf').textContent = on && deaf ? 'Undeafen' : 'Deafen';
  const muted = on && !voice.localParticipant.isMicrophoneEnabled;
  $('voice-mute').setAttribute('aria-pressed', String(muted));
  $('voice-mute').textContent = muted ? 'Unmute' : 'Mute';
  $('voice-unlock').hidden = !on || voice.canPlaybackAudio;
}

function voiceChanged() {
  voiceButtons();
  renderParticipants();
  clearTimeout(beatTimer);
  beatTimer = setTimeout(beat, 300);
}

// Presence heartbeat (005-AC6): it keeps this tab counted while its stream is down, and reports
// its audio state. A closing page says so, so its audio state goes at once.
let beatTimer;
function beat(gone) {
  if (ended || !myID) return;
  const lp = voice && voice.localParticipant;
  const mic = !!lp && lp.isMicrophoneEnabled;
  const body = JSON.stringify({
    tab, audio: !!lp, mic, muted: !!lp && !mic && !!lp.getTrackPublication(LivekitClient.Track.Source.Microphone),
    deaf: !!lp && deaf, gone: !!gone,
  });
  // keepalive lets the request outlive a closing page.
  fetch(api + '/presence', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body, keepalive: !!gone })
    .catch(() => {});
}
setInterval(() => { if (events) beat(); }, 10_000);
window.addEventListener('pagehide', () => { if (events) beat(true); });

// Deafen stops hearing the others and, unless the participant keeps it on, mutes their mic;
// undeafen restores the mic as it was (005-AC22). The state outlives a reconnect in this session.
const deafKey = 'grill-room-deaf:' + roomID;
let deaf = false;
let micBefore = true;
$('voice-keepmic').querySelector('input').checked = !!JSON.parse(sessionStorage.getItem(deafKey) || '{}').keepMic;

function saveDeaf() {
  sessionStorage.setItem(deafKey, JSON.stringify({ deaf, micBefore, keepMic: $('voice-keepmic').querySelector('input').checked }));
}

function hearOthers(on) {
  for (const rp of voice.remoteParticipants.values()) {
    for (const pub of rp.audioTrackPublications.values()) pub.setSubscribed(on);
  }
}

async function setDeaf(on, before) {
  const room = voice;
  if (!room || on === deaf) return;
  const lp = room.localParticipant;
  deaf = on;
  hearOthers(!on);
  try {
    if (on) {
      micBefore = before ?? lp.isMicrophoneEnabled;
      if (!$('voice-keepmic').querySelector('input').checked) await lp.setMicrophoneEnabled(false);
    } else if (lp.isMicrophoneEnabled !== micBefore) {
      await lp.setMicrophoneEnabled(micBefore);
    }
  } catch (err) {
    voiceError('Microphone: ' + err.message);
  }
  saveDeaf();
  voiceChanged();
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
    room.on(LK.RoomEvent.TrackSubscribed, (track, pub, p) => {
      if (track.kind !== 'audio') return;
      if (deaf) return pub.setSubscribed(false);
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
    // A reconnect in the same session comes back deafened, with the mic it had before.
    const saved = JSON.parse(sessionStorage.getItem(deafKey) || '{}');
    if (saved.deaf) {
      const before = saved.micBefore;
      deaf = false;
      await setDeaf(true, before);
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
$('voice-deaf').addEventListener('click', () => setDeaf(!deaf));
$('voice-keepmic').querySelector('input').addEventListener('change', saveDeaf);
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
  text.textContent = 'An agent daemon asks to run the agent for you in this room, with your Claude subscription. ' +
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

function fitViewport() {
  // A pinch zoom also shrinks the visual viewport: only the keyboard and toolbars should resize.
  if (Math.abs(visualViewport.scale - 1) > 0.01) return;
  document.body.style.setProperty('--vv-height', visualViewport.height + 'px');
  document.body.style.setProperty('--vv-top', visualViewport.offsetTop + 'px');
}
if (window.visualViewport) {
  visualViewport.addEventListener('resize', fitViewport);
  visualViewport.addEventListener('scroll', fitViewport);
  fitViewport();
}

loadRoom().then((ok) => {
  if (!ok) return;
  showRecovery();
  connect();
  showHostAgent().catch(console.error);
  const pair = new URLSearchParams(location.search).get('pair');
  if (pair) showPairing(pair).catch(console.error);
}).catch(console.error);
