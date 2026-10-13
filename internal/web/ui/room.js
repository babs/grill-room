'use strict';

const $ = (id) => document.getElementById(id);
const roomID = decodeURIComponent(location.pathname.split('/')[2] || '');
const api = '/api/rooms/' + encodeURIComponent(roomID);
// /r/<room>/docs[/<doc>] is the document manager in a window of its own (005-AC11): this page,
// without the chat, the audio and the agents. Its stream keeps it live like the room page.
const roomPath = '/r/' + encodeURIComponent(roomID);
const docsMode = location.pathname.split('/')[3] === 'docs';
if (docsMode) document.body.classList.add('docs-window');
const phone = () => matchMedia('(max-width: 40rem)').matches;
const messages = $('messages');
const sendForm = $('send');
const bodyInput = $('body');
let events;
// lastID is the newest message shown: a reopened stream resumes after it. editSeq is the last
// edit or delete applied: the stream replays the ones past it.
let lastID = 0;
let editSeq = 0;
let retryTimer;

// netFetch is fetch, with a GET the browser dropped (a network change, ERR_NETWORK_CHANGED) tried
// twice more: a read is safe to repeat, and a lost one left panels empty until a reload.
async function netFetch(input, init) {
  const get = (init?.method || 'GET').toUpperCase() === 'GET';
  for (let i = 0; ; i++) {
    try {
      return await window.fetch(input, init);
    } catch (err) {
      if (!get || i >= 2 || !(err instanceof TypeError)) throw err;
      await new Promise((resolve) => setTimeout(resolve, 300 * (i + 1)));
    }
  }
}
let ended = false;
let refreshTimer;
let wasIn = false;
let participants = [];
let myID = '';
let myName = '';
let hasProfile = false;
let roomName = '';
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
  $('chat-empty').hidden = true;
  $('upload-open').hidden = true;
  $('docs-pane').classList.remove('dropping');
  for (const id of ['manage-open', 'invite-open', 'personas-open', 'more-open']) $(id).hidden = true;
  for (const d of document.querySelectorAll('dialog[open]')) d.close();
  for (const m of document.querySelectorAll('.menu:popover-open')) m.hidePopover();
  $('agent').hidden = true;
  $('annotations').hidden = true;
}

// loadRoom fetches the room header and participant list; false when access is gone.
async function loadRoom() {
  const res = await netFetch(api);
  if (res.status === 401) {
    // A refresh can race the stream's "revoked" event: once in, both must tell the same thing.
    end(wasIn ? 'Your access to this room has ended.'
      : 'You are not in this room, or your access was revoked. Open your invite link again.');
    return false;
  }
  if (!res.ok) throw new Error('room: HTTP ' + res.status);
  wasIn = true;
  const data = await res.json();
  // The docs window's room name leads back to the room (008-AC6). Each room event reloads this:
  // the link is made once, so its focus and anything holding it survive.
  const heading = $('room-name');
  let link = heading.querySelector('a');
  if (docsMode && !link) {
    link = document.createElement('a');
    link.href = roomPath;
    link.title = 'Back to the room';
    heading.replaceChildren(link);
  }
  (link || heading).textContent = data.room.name;
  roomName = data.room.name;
  showTitle();
  // Someone else renamed this participant (005-AC21): a rename of their own sets myName first.
  if (myName && data.me.name !== myName) showNotice('Your name in this room is now ' + data.me.name + '.');
  myName = data.me.name;
  showMe();
  participants = data.participants;
  myID = String(data.me.id);
  hasProfile = !!data.profile;
  meAdmin = !!data.me.admin;
  $('doc-delete').hidden = !meAdmin;
  renameSenders();
  $('voice').hidden = !data.voice;
  showManage();
  renderParticipants();
  // The room admin panel (admin/room-admin.js) renders from the same answer.
  window.grillRoom = data;
  window.dispatchEvent(new CustomEvent('grill:room', { detail: data }));
  return true;
}

// showMe draws the user menu's button: initials in a ring, and the name beside them when wide.
function showMe() {
  const who = document.createElement('span');
  who.className = 'who';
  who.textContent = myName;
  $('me').replaceChildren(who);
  $('me').dataset.initials = initials(myName);
  $('me-head').textContent = myName;
}

// initials takes the first letter of the first two words, "Ana María" → "AM".
const initials = (name) => name.trim().split(/\s+/).slice(0, 2).map((w) => [...w][0] || '').join('').toUpperCase();

// The list shrinks under a keyboard, a toolbar or a wrapping header: keep the newest message in
// view when it was. A scroll up unpins, as scroll events land after the layout changed, except
// right after a viewport resize: a list that grew clamps its scroll down, and when it shrinks back
// within the frame no resize of the list is observed to pin it again.
let pinned = true;
let lastTop = 0;
let resizedAt = -Infinity;
// A pinch zoom is no layout change, and a reader's own wheel, touch or key ends the window.
for (const target of [window, window.visualViewport]) {
  target?.addEventListener('resize', () => { if (!window.visualViewport || Math.abs(visualViewport.scale - 1) <= 0.01) resizedAt = performance.now(); });
}
for (const ev of ['wheel', 'touchstart', 'keydown']) messages.addEventListener(ev, () => { resizedAt = -Infinity; }, { passive: true });
messages.addEventListener('scroll', () => {
  if (messages.scrollTop + messages.clientHeight >= messages.scrollHeight - 40) pinned = true;
  else if (messages.scrollTop < lastTop) {
    if (pinned && performance.now() - resizedAt < 1000) messages.scrollTop = messages.scrollHeight;
    else pinned = false;
  }
  lastTop = messages.scrollTop;
});
new ResizeObserver(() => { if (pinned) messages.scrollTop = messages.scrollHeight; }).observe(messages);

// Unread count (005-AC12): lines from others and syntheses that arrive while the tab is not
// focused show as "(N) " in its title; voice lines only when opted in. The stream replays history
// on load, so only lines written since the page loaded count.
const loadedAt = Date.now();
const voiceKey = 'grill-room-count-voice';
let unread = 0;
// unreadMe: one of the unread lines mentions the viewer (005-AC27).
let unreadMe = false;

function showTitle() {
  document.title = (unread ? '(' + unread + (unreadMe ? ' @' : '') + ') ' : '') + (docsMode ? 'Documents · ' : '') + roomName + ' · grill-room';
}

function countUnread(m) {
  if (docsMode || document.hasFocus() || new Date(m.ts).getTime() < loadedAt || String(m.participant_id) === myID) return;
  if (m.kind === 'text' || m.kind === 'agent' || (m.kind === 'voice' && localStorage.getItem(voiceKey) === '1') || namesMe(m)) {
    unread++;
    unreadMe = unreadMe || namesMe(m);
    showTitle();
  }
}
window.addEventListener('focus', () => {
  unread = 0;
  unreadMe = false;
  showTitle();
});
$('count-voice').checked = localStorage.getItem(voiceKey) === '1';
$('count-voice').addEventListener('change', () => localStorage.setItem(voiceKey, $('count-voice').checked ? '1' : '0'));

// jumpTo is the line a deep link names, /r/<room>#m<id>, as a notification opens it (005-AC24):
// the chat scrolls there once the stream has delivered it.
let jumpTo = Number((location.hash.match(/^#m(\d+)$/) || [])[1] || 0);

function jump(id) {
  const li = messages.querySelector('li[data-id="' + id + '"]');
  if (!li) {
    jumpTo = id;
    return;
  }
  jumpTo = 0;
  pinned = false;
  li.scrollIntoView({ block: 'center' });
  li.classList.add('flash');
  setTimeout(() => li.classList.remove('flash'), 1500);
}

// A notification clicked while the room is open: its service worker names the line.
if ('serviceWorker' in navigator) {
  navigator.serviceWorker.addEventListener('message', (e) => { if (e.data && e.data.jump) jump(Number(e.data.jump)); });
}

function addMessage(m) {
  countUnread(m);
  if (m.kind === 'system') {
    // A join changes the participant list; history replay sends many, so refresh once.
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => loadRoom().catch(console.error), 200);
  }
  messages.append(renderMessage(m));
  if (m.kind !== 'system') showEmpty();
  if (pinned) messages.scrollTop = messages.scrollHeight;
  if (m.id === jumpTo) jump(m.id);
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
  sender.className = 'sender';
  // An agent shows its persona's badge and colour (002-AC42).
  sender.textContent = m.badge ? m.badge + ' ' + m.sender : m.sender;
  if (m.colour) sender.style.color = m.colour;
  if (m.participant_id) {
    li.dataset.pid = m.participant_id;
    li.dataset.sender = m.sender;
    nameSender(li, sender);
  }
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
      why.textContent = '(AI host\'s limit)';
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
  drawMentions(body, m.named || []);
  if (namesMe(m)) li.classList.add('mentions-me');
  if (m.edited_at) {
    meta.append(' ', button('(edited)', 'Show earlier versions', () => toggleVersions(li, m).catch(console.error)));
  }
  li.append(meta, body);
  if (String(m.participant_id) === myID && (m.kind === 'text' || m.kind === 'voice')) li.append(messageMenu(li, m));
  if (m.mentions) stored.set(m.id, m.mentions);
  if (m.kind === 'text') fillMentions(li);
  return li;
}

// nameSender shows a person's line under their current name, and the name it was written under on
// hover once they renamed (005-AC1).
function nameSender(li, el) {
  const p = participants.find((x) => String(x.id) === li.dataset.pid);
  const was = li.dataset.sender;
  el.textContent = p ? p.name : was;
  el.title = p && p.name !== was ? 'Written as ' + was : '';
}

function renameSenders() {
  for (const li of messages.querySelectorAll('li[data-pid]')) {
    const el = li.querySelector(':scope > .meta > .sender');
    if (el) nameSender(li, el);
  }
}

// agentError says why an action on the AI failed: in the header's status where it shows, in the
// notice where a narrow screen hides it.
function agentError(text) {
  $('agent-status').textContent = text;
  if (!$('agent-status').offsetParent) showNotice(text);
}

function explain(title, text) {
  $('why-title').textContent = title;
  $('why-text').textContent = text;
  $('why').showModal();
}

function showNotice(text) {
  $('notice-text').textContent = text;
  $('notice').hidden = false;
}
$('notice-close').addEventListener('click', () => { $('notice').hidden = true; });

// Self-rename (005-AC1), with the validation of a join; the option also names their profile. A
// room admin renames someone else from their ⋯ (008-AC13): renameTarget is that participant.
let renameTarget = null;
function openRename(p) {
  renameTarget = p;
  $('rename-title').textContent = p ? 'Rename ' + p.name : 'Your name in this room';
  $('rename-name').value = p ? p.name : myName;
  $('rename-profile').checked = false;
  $('rename-profile-label').hidden = !!p || !hasProfile;
  $('rename-status').textContent = '';
  $('rename').showModal();
  $('rename-name').select();
}
$('rename-open').addEventListener('click', () => openRename(null));
$('rename-cancel').addEventListener('click', () => $('rename').close());
$('rename-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const name = $('rename-name').value.trim();
  const other = renameTarget;
  sendJSON('PUT', api + '/participants/' + (other ? other.id : myID) + '/name', other ? { name } : { name, profile: $('rename-profile').checked }).then((done) => {
    if (!done) return;
    // Not an outside rename: no notice when loadRoom reads the new name.
    if (!other) myName = '';
    $('rename').close();
    toast('Renamed');
    return loadRoom();
  }).catch((err) => { $('rename-status').textContent = err.message; });
});

// Mention chips (005-AC27). The server names the line's @mentions by its one rule (mentionRe in
// mentions.go); the page only finds those words in the rendered text. A word boundary is what
// mentionRe takes: before, none of [\p{L}\p{N}_@]; after, none of [\p{L}\p{N}_-]
// (TestChipsFollowMentionRe checks these classes match the server's).
const notBefore = /[\p{L}\p{N}_@]/u;
const notAfter = /[\p{L}\p{N}_-]/u;
const namesMe = (m) => (m.named || []).some((n) => String(n.participant_id) === myID);

function chip(n) {
  const el = document.createElement('mark');
  el.className = 'mention-chip';
  el.tabIndex = 0;
  el.textContent = n.at;
  const p = n.persona_id ? null : participants.find((x) => x.id === n.participant_id);
  if (n.persona_id) {
    el.classList.add('persona');
    el.dataset.persona = n.persona_id;
    paintChip(el);
  } else {
    el.classList.add(String(n.participant_id) === myID ? 'me' : 'person');
    el.title = (p ? p.name : n.at.slice(1)) + (String(n.participant_id) === myID ? ', you' : '');
  }
  return el;
}

// paintChip gives a persona chip its persona's badge and colour, once the agent state names them.
function paintChip(el) {
  const p = personaByID(Number(el.dataset.persona));
  el.title = (p ? p.name : el.textContent.slice(1)) + ', persona';
  if (!p) return;
  el.dataset.badge = p.badge;
  el.style.setProperty('--persona', p.colour);
}

// drawMentions turns each named @word of body's text into a chip, outside code and links.
function drawMentions(body, named) {
  if (!named.length) return;
  const walk = document.createTreeWalker(body, NodeFilter.SHOW_TEXT, {
    acceptNode: (t) => (t.parentElement.closest('code, pre, a, mark') ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT),
  });
  const texts = [];
  for (let t = walk.nextNode(); t; t = walk.nextNode()) texts.push(t);
  for (const t of texts) {
    const s = t.data;
    const parts = [];
    let from = 0;
    for (let i = s.indexOf('@'); i >= 0; i = s.indexOf('@', i + 1)) {
      const n = named.find((x) => s.startsWith(x.at, i) && !notAfter.test(s.charAt(i + x.at.length)) && !(i > 0 && notBefore.test(s.charAt(i - 1))));
      if (!n) continue;
      parts.push(s.slice(from, i), chip(n));
      from = i + n.at.length;
      i = from - 1;
    }
    if (parts.length) t.replaceWith(...parts, s.slice(from));
  }
}

// Mentions. A message whose @mention queued a turn shows that turn's state, linked to how it
// ended (005-AC4); one that cannot run tells its author why, and nobody else (005-AC3).
const notices = new Map();

// mentionToken spells a person's name as the server's mentionToken does: "Ana María" is @Ana_María.
const mentionToken = (name) => name.replace(/[^\p{L}\p{N}_-]+/gu, '_').replace(/^_+|_+$/g, '');

// stored holds the chips each line came with from the stream: the agent state carries only the
// latest mentions, and an older line keeps its chips from there.
const stored = new Map();

function fillMentions(li) {
  const id = Number(li.dataset.id);
  const live = (agent.mentions || []).filter((s) => s.message_id === id);
  const states = live.length ? live : stored.get(id) || [];
  const said = notices.get(id) || [];
  // A mention past the agent state's window keeps the state it last showed.
  if (!states.length && !said.length) return;
  let box = li.querySelector(':scope > .mentions');
  if (!box) {
    box = document.createElement('div');
    box.className = 'mentions';
    li.append(box);
  }
  box.replaceChildren(...states.map(mentionChip), ...said.map((n) => {
    const el = document.createElement('span');
    el.className = 'notice';
    el.textContent = '@' + n.name + ': ' + n.reason + '. Only you see this.';
    return el;
  }));
}

function mentionChip(s) {
  const p = personaByID(s.persona_id);
  const el = document.createElement(s.line_id ? 'a' : 'span');
  el.className = 'mention-turn ' + s.status;
  el.textContent = '→ @' + (p ? p.name : 'persona') + ' · ' + s.status;
  if (s.line_id) {
    el.href = '#';
    el.title = s.status === 'done' ? 'Show the synthesis' : 'Show how the turn ended';
    el.addEventListener('click', (e) => {
      e.preventDefault();
      const line = messages.querySelector('li[data-id="' + s.line_id + '"]');
      if (!line) return;
      line.scrollIntoView({ block: 'center' });
      line.classList.add('flash');
      setTimeout(() => line.classList.remove('flash'), 1500);
    });
  }
  return el;
}

function renderMentions() {
  for (const id of new Set((agent.mentions || []).map((s) => s.message_id))) {
    const li = messages.querySelector('li[data-id="' + id + '"]');
    if (li) fillMentions(li);
  }
}

async function toggleVersions(li, m) {
  const shown = li.querySelector('.versions');
  if (shown) {
    shown.remove();
    return;
  }
  const res = await netFetch(api + '/messages/' + m.id + '/versions');
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
  cancel.className = 'btn-secondary';
  form.append(box, save, cancel);
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

// messageMenu is a message's one actions button (005-AC35): its Edit and Delete sit in a menu shared
// by every message, laid next to the button pressed.
let menuFor = null;
function messageMenu(li, m) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'msg-more btn-icon';
  b.textContent = '⋯';
  b.setAttribute('aria-label', 'Line actions');
  b.setAttribute('aria-expanded', 'false');
  b.addEventListener('click', () => {
    const menu = $('msg-menu');
    menuFor = { li, m, b };
    const r = b.getBoundingClientRect();
    menu.style.top = Math.min(r.bottom + 4, innerHeight - 120) + 'px';
    menu.style.right = Math.max(8, innerWidth - r.right) + 'px';
    menu.showPopover();
    b.setAttribute('aria-expanded', 'true');
    $('msg-edit').focus();
  });
  return b;
}
$('msg-menu').addEventListener('toggle', (e) => {
  if (e.newState === 'closed' && menuFor) menuFor.b.setAttribute('aria-expanded', 'false');
});
// The line may have been redrawn since the menu opened (an edit from elsewhere): act on it as shown.
$('msg-edit').addEventListener('click', () => {
  const li = menuFor && messages.querySelector('li[data-id="' + menuFor.m.id + '"]');
  if (li && li.querySelector(':scope > .body')) editMessage(li, menuFor.m);
});
$('msg-delete').addEventListener('click', () => { if (menuFor) deleteMessage(menuFor.m); });

// The Preact panels say what they did through a toast too (006-AC15).
addEventListener('grill:toast', (e) => toast(e.detail));

// toast says what an action did, with at most one action of its own (005-AC39); onEnd runs when it
// goes, undone or not.
function toast(text, action, onEnd) {
  const el = document.createElement('div');
  el.className = 'toast';
  el.append(span('text', text));
  let timer;
  const close = (undone) => {
    clearTimeout(timer);
    el.remove();
    if (!$('toasts').children.length && $('toasts').matches(':popover-open')) $('toasts').hidePopover();
    if (onEnd) onEnd(undone);
  };
  if (action) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'btn-ghost';
    b.textContent = action;
    b.addEventListener('click', () => close(true));
    el.append(b);
  }
  // The region is a popover: in the top layer, a toast shows over an open sheet too.
  $('toasts').append(el);
  if (!$('toasts').matches(':popover-open')) $('toasts').showPopover();
  // A region that only just appeared is often skipped by screen readers: this one always is there.
  $('toast-live').textContent = text;
  timer = setTimeout(() => close(false), action ? 5000 : 4000);
  return () => close(false);
}

// A delete waits out its toast: Undo keeps the line, and nothing reaches the server. A closing page
// sends the deletes it holds at once.
const pendingDeletes = new Map();
function deleteMessage(m) {
  const li = messages.querySelector('li[data-id="' + m.id + '"]');
  if (li) li.classList.add('deleting');
  const send = () => {
    pendingDeletes.delete(m.id);
    netFetch(api + '/messages/' + m.id, { method: 'DELETE', keepalive: true }).then((res) => {
      if (res.status === 401) end('Your access to this room has ended.');
    }).catch(console.error);
  };
  pendingDeletes.set(m.id, send);
  toast('Message deleted', 'Undo', (undone) => {
    if (!pendingDeletes.has(m.id)) return;
    if (!undone) return send();
    pendingDeletes.delete(m.id);
    messages.querySelector('li[data-id="' + m.id + '"]')?.classList.remove('deleting');
  });
}
window.addEventListener('pagehide', () => { for (const send of [...pendingDeletes.values()]) send(); });

// Modals (005-AC13): Escape closes a modal dialog natively; a click outside one closes it too,
// and focus goes back to where it was. A press that starts inside, say a text selection dragged
// out, is not a click outside.
let pressedOutside = null;
const outside = (d, e) => {
  const r = d.getBoundingClientRect();
  return e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom;
};
document.addEventListener('pointerdown', (e) => {
  pressedOutside = e.target instanceof HTMLDialogElement && e.target.open && outside(e.target, e) ? e.target : null;
});
document.addEventListener('click', (e) => {
  const d = e.target;
  if (d instanceof HTMLDialogElement && d === pressedOutside && d.open && outside(d, e)) d.close();
  pressedOutside = null;
});
let focusBefore = null;
document.addEventListener('focusin', (e) => {
  if (e.target.closest('dialog[open]')) return;
  // A menu item hides with its menu: focus goes back to the button that opened the menu.
  const menu = e.target.closest('.menu');
  focusBefore = menu ? document.querySelector('[popovertarget="' + menu.id + '"]') : e.target;
});
// close does not bubble: listen in the capture phase.
document.addEventListener('close', (e) => {
  if (e.target instanceof HTMLDialogElement && focusBefore && focusBefore.isConnected && !document.querySelector('dialog[open]')) focusBefore.focus();
}, true);

// The status strip shows its most urgent item, the first of its children shown; "+N more" opens the
// rest (005-AC38). Each item keeps its own hidden flag: the strip only folds the extra ones.
let stripOpen = false;
// The docs window hides the agent's items by its stylesheet: they never count there.
const docsHides = new Set(['running', 'queue', 'chain-unlimited', 'chain-paused', 'approvals']);
function updateStrip() {
  const items = [...$('strip').children].filter((el) => el.id !== 'strip-more' && !el.hidden && !(docsMode && docsHides.has(el.id)));
  items.forEach((el, i) => el.classList.toggle('extra', i > 0 && !stripOpen));
  const more = $('strip-more');
  // The observer watches hidden flags: writing an unchanged one would wake it again, forever.
  if (more.hidden !== items.length < 2) more.hidden = items.length < 2;
  more.textContent = stripOpen ? 'Show less' : '+' + (items.length - 1) + ' more';
  more.setAttribute('aria-expanded', String(stripOpen));
  if (items.length < 2) stripOpen = false;
}
new MutationObserver(updateStrip).observe($('strip'), { subtree: true, attributes: true, attributeFilter: ['hidden'] });
$('strip-more').addEventListener('click', () => {
  stripOpen = !stripOpen;
  updateStrip();
});

// Manage, its tabs and its deep links are admin/room-admin.js's (004-AC8, 005-AC15).

// Menus are popovers (005-AC32): the browser closes them on a click outside and on Escape; picking
// an item closes them too.
// The document's menu has its own (008-AC8).
for (const menu of document.querySelectorAll('.menu:not(#doc-menu, #person-menu)')) {
  menu.addEventListener('click', (e) => { if (e.target.closest('button, a')) menu.hidePopover(); });
}
for (const b of document.querySelectorAll('dialog .close')) b.addEventListener('click', () => b.closest('dialog').close());
$('notify-open').addEventListener('click', () => $('notify').showModal());
$('invite-open').addEventListener('click', () => {
  $('invites').showModal();
  loadInvites().catch(console.error);
});
$('host-open').addEventListener('click', () => $('host-agent').showModal());
$('docs-window-menu').addEventListener('click', (e) => {
  e.preventDefault();
  window.open(roomPath + '/docs', docsTarget);
});

// copyField copies a read-only field, and selects it for a manual copy when the clipboard refuses.
function copyField(input) {
  navigator.clipboard.writeText(input.value).then(() => toast('Copied'), () => input.select());
}

// The join page leaves the recovery link of a profile it just created. This tab keeps it for the
// user menu's "Use on another device" until its holder says they saved it (005-AC32); the server
// keeps only its hash, so once gone it is gone.
const recoveryKey = 'grill_room_recovery';
// A window this page opens gets a copy of its session storage: the docs window has no use for the link.
if (docsMode) sessionStorage.removeItem(recoveryKey);
$('recovery-open').addEventListener('click', () => {
  const link = sessionStorage.getItem(recoveryKey);
  $('recovery-link').value = link || '';
  $('recovery-has').hidden = !link;
  $('recovery-none').hidden = !!link;
  $('recovery-copy').textContent = 'Copy link';
  $('recovery').showModal();
});
$('recovery-copy').addEventListener('click', () => copyField($('recovery-link'), $('recovery-copy')));
$('recovery-done').addEventListener('click', () => {
  sessionStorage.removeItem(recoveryKey);
  $('recovery-link').value = '';
  $('recovery').close();
});

// After the first message of a new profile, one notice offers the link.
function nudgeRecovery() {
  if (!sessionStorage.getItem(recoveryKey) || sessionStorage.getItem(recoveryKey + ':nudged')) return;
  sessionStorage.setItem(recoveryKey + ':nudged', '1');
  showNotice('To use this profile on another device, save its link: your initials, then "Use on another device".');
}

// Invites: a member creates links and revokes the ones they created; a room admin, who gets every
// invite, manages the others from Manage.
async function loadInvites() {
  const res = await netFetch(api + '/invites');
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

// The empty chat's card makes an invite and copies it in one press (005-AC34).
$('chat-invite').addEventListener('click', () => {
  sendJSON('POST', api + '/invites').then((inv) => {
    if (!inv) return;
    return navigator.clipboard.writeText(inv.url).then(() => { $('chat-invite-status').textContent = 'Copied. Paste it to the people you invite.'; },
      () => {
        $('invite-link').value = inv.url;
        $('invite-new').hidden = false;
        $('invites').showModal();
        $('invite-link').select();
        loadInvites().catch(console.error);
      });
  }).catch((err) => { $('chat-invite-status').textContent = err.message; });
});

// showEmpty shows the invite card while the chat holds only event lines.
function showEmpty() {
  $('chat-empty').hidden = ended || docsMode || !!messages.querySelector('li.msg:not(.system)');
}

// Documents. Text documents render in an iframe sandboxed without allow-same-origin: their
// scripts get an opaque origin, so no room cookie and no same-origin call to the API.
let openDoc = null;

function docURL(id, suffix) {
  return api + '/documents/' + encodeURIComponent(id) + suffix;
}

// The docs window is one per room: a second "Open in a new window" reuses it.
const docsTarget = 'grill-docs-' + roomID;
for (const id of ['docs-window', 'docs-window-menu']) {
  $(id).href = roomPath + '/docs';
  $(id).target = docsTarget;
}
$('doc-window').target = docsTarget;

function describe(d) {
  if (d.kind === 'agent') return 'written by ' + personaLabel(d.author) + ', ' + d.revisions + (d.revisions === 1 ? ' revision' : ' revisions');
  return 'added by ' + d.author;
}

async function loadDocuments() {
  const res = await netFetch(api + '/documents');
  if (res.status === 401) return;
  if (!res.ok) throw new Error('documents: HTTP ' + res.status);
  const docs = await res.json();
  $('documents').replaceChildren(...docs.map((d) => {
    const li = document.createElement('li');
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'doc';
    b.textContent = d.name;
    // One line with an ellipsis: the full name in the tooltip (008-AC14).
    b.title = d.name + '\n' + describe(d);
    b.addEventListener('click', () => {
      showDocument(d.id).catch(console.error);
    });
    li.append(b);
    return li;
  }));
  $('docs-empty').hidden = docs.length > 0;
  $('tab-docs-count').textContent = docs.length ? String(docs.length) : '';
  if (openDoc) {
    const fresh = docs.find((d) => d.id === openDoc.id);
    // A room admin deleted it meanwhile: the viewer closes; renamed, it shows its new name.
    if (!fresh) $('doc-close').click();
    else if (fresh.revisions !== openDoc.revisions || fresh.name !== openDoc.name) await showDocument(fresh.id, true);
  }
}

function option(value, text) {
  const o = document.createElement('option');
  o.value = value;
  o.textContent = text;
  return o;
}

// showDocument opens a document; refresh keeps the revisions picked unless the latest was shown.
// A deep link pushes no history entry: Chrome lets Back skip an entry pushed before any tap.
async function showDocument(id, refresh, deepLink) {
  const res = await netFetch(docURL(id, ''));
  if (!res.ok) throw new Error('document: HTTP ' + res.status);
  const { document: d, revisions } = await res.json();
  // A refresh that lands after the viewer closed or moved to another document is stale.
  if (refresh && (!openDoc || openDoc.id !== id)) return;
  const followLatest = !refresh || $('rev').value === String(openDoc.revisions);
  const keepFrom = refresh ? $('rev-from').value : '';
  const opening = !refresh && !openDoc;
  openDoc = d;
  $('viewer').hidden = false;
  // A document opened from the chat (a single upload, a link) shows in the phone's Docs tab.
  if (!refresh && !docsMode && phone()) showTab('docs');
  $('doc-window').href = roomPath + '/docs/' + encodeURIComponent(d.id);
  // The docs window's address follows the document shown: a deep link to reload or share. On a
  // phone the document is a step Back undoes (008-AC9).
  const url = docsMode ? $('doc-window').href : location.href;
  if (opening && phone() && !deepLink) {
    history.pushState({ pane: 'doc' }, '', url);
    docPushed = true;
  } else if (docsMode) history.replaceState(history.state, '', url);
  $('doc-width').hidden = d.format !== 'md';
  $('doc-name').textContent = d.name;
  $('doc-name').title = d.name;
  $('doc-delete').hidden = !meAdmin;
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
      // The first paint at the chosen width; later changes go to the frame as a message (008-AC7).
      if (d.format === 'md' && mdWidth !== 'narrow') q.set('width', mdWidth);
      if (d.kind === 'agent') {
        q.set('rev', $('rev').value);
        if ($('rev-from').value) q.set('from', $('rev-from').value);
      }
      el.src = docURL(d.id, '/view' + (q.size ? '?' + q : ''));
    }
  }
  $('doc-frame').replaceChildren(el);
}

// Any member renames a document; a room admin deletes it, softly (005-AC25, 005-AC26).
let meAdmin = false;
$('doc-rename').addEventListener('click', () => {
  $('doc-rename-name').value = openDoc.name;
  $('doc-rename-status').textContent = '';
  $('doc-rename-form').hidden = false;
  $('doc-rename-name').focus();
});
$('doc-rename-cancel').addEventListener('click', () => { $('doc-rename-form').hidden = true; });
$('doc-rename-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const d = openDoc;
  sendJSON('PATCH', docURL(d.id, ''), { name: $('doc-rename-name').value.trim() }).then((done) => {
    if (!done) return;
    $('doc-rename-form').hidden = true;
    toast('Renamed');
    return Promise.all([loadDocuments(), showDocument(d.id, true)]);
  }).catch((err) => { $('doc-rename-status').textContent = err.message; });
});
// A soft delete acts at once; Undo restores it (005-AC39).
$('doc-delete').addEventListener('click', () => {
  const d = openDoc;
  sendJSON('DELETE', docURL(d.id, '')).then((done) => {
    if (!done) return;
    $('doc-close').click();
    toast(d.name + ' deleted', 'Undo', (undone) => {
      if (undone) sendJSON('POST', docURL(d.id, '/restore')).catch((err) => showNotice(err.message));
    });
  }).catch((err) => { $('doc-rename-status').textContent = err.message; $('doc-rename-form').hidden = false; });
});

$('rev').addEventListener('change', renderDocument);
$('rev-from').addEventListener('change', renderDocument);
// On a phone the document and Notes are history entries: closing one goes Back, so Back and ✕
// stay in step (008-AC9).
// The pane closes at once and the flags clear before going Back: a second tap meanwhile finds
// nothing to step back from.
$('doc-close').addEventListener('click', () => {
  const steps = (docPushed ? 1 : 0) + (notesPushed ? 1 : 0);
  docPushed = false;
  notesPushed = false;
  closeDocument();
  if (steps) history.go(-steps);
});
function closeDocument() {
  if (docsMode) history.replaceState(null, '', roomPath + '/docs');
  notesPane(false);
  openDoc = null;
  $('viewer').hidden = true;
  $('doc-frame').replaceChildren();
  closeNew();
  annotations = [];
  renderAnnotations();
}
window.addEventListener('popstate', (e) => {
  const pane = e.state?.pane;
  let closed = false;
  if (notesPushed && pane !== 'notes') {
    notesPushed = false;
    notesPane(false);
    closed = true;
  }
  if (docPushed && !pane) {
    docPushed = false;
    closeDocument();
    closed = true;
  } else if (pane === 'notes' && docPushed && openDoc) {
    notesPushed = true;
    notesPane(true);
  } else if (pane && !(pane === 'doc' ? docPushed : notesPushed)) {
    // Forward onto the step of a pane since closed, or one from before a reload: stay put.
    history.replaceState(null, '', docsMode && !openDoc ? roomPath + '/docs' : location.href);
  }
  // Back from the phone's Chat or People tab steps past the hidden panes: it leaves the room.
  if (closed && !docsMode && roomTab !== 'docs') history.back();
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
      if (openDoc?.format === 'md') postToFrame({ type: 'gr:width', width: mdWidth });
      sendAnnotations();
      break;
    case 'gr:anchor':
      setPick(false);
      // A frame script can post anchors at will: never replace what the participant is typing.
      if (canCreate() && !(newAnchor !== undefined && $('annot-body').value.trim())) openNew(msg.anchor);
      break;
    case 'gr:open':
      openNotes();
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
  // An annotation started from the document needs its form in sight.
  openNotes();
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
  const res = await netFetch(url, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
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
  const res = await netFetch(docURL(d.id, '/annotations'));
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

// Annotations are a panel the viewer toggles, closed by default below 100rem, and the chat folds
// into a rail (005-AC37). Both remember their last state.
const annotKey = 'grill-room-annotations';
function showAnnotations(on) {
  document.body.classList.toggle('annot-closed', !on);
  $('annot-toggle').setAttribute('aria-pressed', String(on));
}
showAnnotations(localStorage.getItem(annotKey) ? localStorage.getItem(annotKey) === '1' : matchMedia('(min-width: 100rem)').matches);
if (phone()) notesPane(false);
$('annot-toggle').addEventListener('click', () => {
  if (phone()) {
    if (!document.body.classList.contains('notes-pane')) openNotes();
    else $('notes-back').click();
    return;
  }
  const on = document.body.classList.contains('annot-closed');
  localStorage.setItem(annotKey, on ? '1' : '0');
  showAnnotations(on);
});
const chatKey = 'grill-room-chat-folded';
function foldChat(on) {
  document.body.classList.toggle('chat-folded', on);
  $('chat-fold').setAttribute('aria-pressed', String(on));
  $('chat-fold').setAttribute('aria-label', on ? 'Show the chat' : 'Fold the chat away');
  localStorage.setItem(chatKey, on ? '1' : '0');
}
foldChat(localStorage.getItem(chatKey) === '1');
$('chat-fold').addEventListener('click', () => foldChat(!document.body.classList.contains('chat-folded')));
$('chat-unfold').addEventListener('click', () => foldChat(false));

// A phone shows one pane at a time: the list, the document, then Notes (008-AC9). docPushed and
// notesPushed say which of them this page pushed a history entry for; a reload keeps the browser's
// entries but not these, so the current one starts plain.
let docPushed = false;
let notesPushed = false;
if (history.state?.pane) history.replaceState(null, '', location.href);
function notesPane(on) {
  document.body.classList.toggle('notes-pane', on);
  if (phone()) $('annot-toggle').setAttribute('aria-pressed', String(on));
}
// openNotes brings Notes in sight: a pane of its own on a phone, the side panel elsewhere.
function openNotes() {
  if (!phone()) {
    showAnnotations(true);
    return;
  }
  if (document.body.classList.contains('notes-pane')) return;
  history.pushState({ pane: 'notes' }, '', location.href);
  notesPushed = true;
  notesPane(true);
}
$('notes-back').addEventListener('click', () => {
  const pushed = notesPushed;
  notesPushed = false;
  notesPane(false);
  if (pushed) history.back();
});
// The Notes button says what the layout shows: the Notes pane on a phone, the side panel wider.
matchMedia('(max-width: 40rem)').addEventListener('change', () => {
  const on = phone() ? document.body.classList.contains('notes-pane') : !document.body.classList.contains('annot-closed');
  $('annot-toggle').setAttribute('aria-pressed', String(on));
});

// Pane widths and folds, per browser and shared by every room and both pages (008-AC2 to AC5).
const panes = {
  side: { key: 'grill-room-list-w', prop: '--list-w', min: 192, max: 480, def: 256, pane: $('side'), handle: $('side-handle'), edge: 'right' },
  notes: { key: 'grill-room-notes-w', prop: '--notes-w', min: 256, max: 640, def: 320, pane: $('annotations'), handle: $('notes-handle'), edge: 'left' },
};
const clampWidth = (p, w) => Math.round(Math.min(p.max, Math.max(p.min, w)));
function setWidth(p, w, store) {
  w = clampWidth(p, w);
  document.documentElement.style.setProperty(p.prop, w + 'px');
  p.handle.setAttribute('aria-valuenow', String(w));
  if (store) localStorage.setItem(p.key, String(w));
}
for (const p of Object.values(panes)) {
  p.handle.setAttribute('aria-valuemin', String(p.min));
  p.handle.setAttribute('aria-valuemax', String(p.max));
  setWidth(p, Number(localStorage.getItem(p.key)) || p.def, false);
  // Dragging the right edge of the side panel widens it; the left edge of Notes, the other way.
  const sign = p.edge === 'right' ? 1 : -1;
  p.handle.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    p.handle.setPointerCapture(e.pointerId);
    p.handle.classList.add('dragging');
    const r = p.pane.getBoundingClientRect();
    const origin = p.edge === 'right' ? r.left : r.right;
    const move = (ev) => setWidth(p, sign * (ev.clientX - origin), false);
    const done = () => {
      p.handle.classList.remove('dragging');
      p.handle.removeEventListener('pointermove', move);
      localStorage.setItem(p.key, p.handle.getAttribute('aria-valuenow'));
    };
    p.handle.addEventListener('pointermove', move);
    p.handle.addEventListener('lostpointercapture', done, { once: true });
  });
  p.handle.addEventListener('keydown', (e) => {
    const step = { ArrowRight: 16, ArrowLeft: -16 }[e.key];
    if (!step) return;
    e.preventDefault();
    setWidth(p, Number(p.handle.getAttribute('aria-valuenow')) + sign * step, true);
  });
}
// A drawer's disclosure triangle names the drawer and says its state in aria-expanded; the whole
// header toggles it, its own actions aside (008-AC5).
const drawers = [{ name: 'people', button: $('people-fold') }, { name: 'docs', button: $('docs-fold') }];
function foldDrawer(d, on, store) {
  document.body.classList.toggle('fold-' + d.name, on);
  d.button.setAttribute('aria-expanded', String(!on));
  if (store) localStorage.setItem('grill-room-fold-' + d.name, on ? '1' : '0');
}
for (const d of drawers) {
  foldDrawer(d, localStorage.getItem('grill-room-fold-' + d.name) === '1', false);
  d.button.addEventListener('click', () => foldDrawer(d, !document.body.classList.contains('fold-' + d.name), true));
  d.button.closest('.pane-head').addEventListener('click', (e) => {
    if (!phone() && !e.target.closest('a, button, input')) d.button.click();
  });
}

// Markdown reads at one of three widths, as in gld (008-AC7).
const mdWidths = { narrow: 'Narrow', large: 'Large', full: 'Full' };
const mdWidthKey = 'grill-room-md-width';
let mdWidth = mdWidths[localStorage.getItem(mdWidthKey)] ? localStorage.getItem(mdWidthKey) : 'narrow';
const widthNames = Object.keys(mdWidths);
const nextWidth = () => widthNames[(widthNames.indexOf(mdWidth) + 1) % widthNames.length];
function showWidth() {
  $('doc-width').setAttribute('aria-label', 'Text width: ' + mdWidths[mdWidth]);
  $('doc-width').title = 'Text width: ' + mdWidths[mdWidth] + '. Press for ' + mdWidths[nextWidth()] + '.';
}
showWidth();
$('doc-width').addEventListener('click', () => {
  mdWidth = nextWidth();
  localStorage.setItem(mdWidthKey, mdWidth);
  showWidth();
  postToFrame({ type: 'gr:width', width: mdWidth });
});

// arrowKeys moves the focus through a menu's shown items with ↑ and ↓ (008-AC8, 008-AC13).
const menuItems = (menu) => [...menu.querySelectorAll(':scope > :is(a, button), :scope > label > input')].filter((e) => !e.hidden && e.offsetParent !== null);
function arrowKeys(menu) {
  menu.addEventListener('keydown', (e) => {
    const step = { ArrowDown: 1, ArrowUp: -1 }[e.key];
    if (!step) return;
    e.preventDefault();
    const items = menuItems(menu);
    items[(items.indexOf(document.activeElement) + step + items.length) % items.length]?.focus();
  });
}

// The document's actions menu (008-AC8): under its button, the first item focused, arrows move.
const docMenu = $('doc-menu');
docMenu.addEventListener('beforetoggle', (e) => {
  if (e.newState !== 'open') return;
  const r = $('doc-more').getBoundingClientRect();
  docMenu.style.top = (r.bottom + 4) + 'px';
  docMenu.style.right = Math.max(8, innerWidth - r.right) + 'px';
});
docMenu.addEventListener('toggle', (e) => { if (e.newState === 'open') menuItems(docMenu)[0]?.focus(); });
arrowKeys(docMenu);
// Capture: the menu closes, its focus back on ⋯, before the item's own action moves the focus.
docMenu.addEventListener('click', (e) => { if (e.target.closest('a, button')) docMenu.hidePopover(); }, true);

function renderAnnotations() {
  $('annot-count').textContent = annotations.length ? '(' + annotations.length + ')' : '';
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

// Adding documents (005-AC31): the + button picks several files, and the documents pane, or the
// whole docs window, takes dropped ones. Each file gets a row with its progress, then ✓, or ✗ with
// the server's reason and Retry.
const accepted = $('upload-input').accept.split(',');
const typeHint = "This file type isn't supported. Use PDF, Word, slides, images, Markdown or HTML.";

function addFiles(files) {
  const list = [...files];
  for (const f of list) uploadRow(f, list.length === 1);
}

function uploadRow(file, open) {
  if (ended) return;
  const li = document.createElement('li');
  const name = span('name', file.name);
  const state = span('state', '');
  const bar = document.createElement('progress');
  bar.max = 1;
  bar.value = 0;
  bar.setAttribute('aria-label', 'Adding ' + file.name);
  li.append(name, state, bar);
  $('uploads').append(li);
  const fail = (why) => {
    bar.remove();
    state.textContent = '✗';
    state.title = 'Not added';
    const btns = document.createElement('span');
    btns.className = 'btns';
    const retry = document.createElement('button');
    retry.type = 'button';
    retry.className = 'btn-ghost';
    retry.textContent = 'Retry';
    retry.setAttribute('aria-label', 'Retry ' + file.name);
    retry.addEventListener('click', () => { li.remove(); uploadRow(file, open); });
    const drop = document.createElement('button');
    drop.type = 'button';
    drop.className = 'btn-icon btn-ghost';
    drop.textContent = '✕';
    drop.setAttribute('aria-label', 'Dismiss ' + file.name);
    drop.addEventListener('click', () => li.remove());
    btns.append(retry, drop);
    li.append(btns, span('why', why));
  };
  // The server checks the type by its content too; this only spares a pointless upload.
  if (!accepted.some((ext) => file.name.toLowerCase().endsWith(ext))) return fail(typeHint);
  const xhr = new XMLHttpRequest();
  xhr.open('POST', api + '/documents');
  xhr.responseType = 'json';
  xhr.upload.addEventListener('progress', (e) => { if (e.lengthComputable) bar.value = e.loaded / e.total; });
  xhr.addEventListener('load', () => {
    if (xhr.status === 401) {
      fail('Your access to this room has ended.');
      return end('Your access to this room has ended.');
    }
    const data = xhr.response || {};
    if (xhr.status < 200 || xhr.status >= 300 || !data.id) return fail(data.error || 'Not added: HTTP ' + xhr.status);
    bar.remove();
    state.textContent = '✓';
    state.className = 'state done';
    state.title = 'Added';
    setTimeout(() => li.remove(), 5000);
    if (open) showDocument(data.id).catch(console.error);
  });
  xhr.addEventListener('error', () => fail('The connection dropped. Check it, then retry.'));
  const form = new FormData();
  form.append('file', file);
  xhr.send(form);
}

$('upload-open').addEventListener('click', () => $('upload-input').click());
$('upload-input').addEventListener('change', (e) => {
  addFiles(e.target.files);
  e.target.value = '';
});

// A drop lands on the documents pane, or anywhere in the docs window. dragenter and dragleave fire
// for every child crossed: a depth count tells when the drag really left.
const zone = docsMode ? document.body : $('docs-pane');
let dragDepth = 0;
const carriesFiles = (e) => [...(e.dataTransfer?.types || [])].includes('Files');
const dropping = (on) => {
  $('docs-pane').classList.toggle('dropping', on);
  // The document frame would swallow the drag: it lets it through while files hover.
  document.body.classList.toggle('drag-files', on);
};
// A file dropped anywhere else must not make the browser open it in place of the room.
for (const type of ['dragover', 'drop']) document.addEventListener(type, (e) => { if (carriesFiles(e)) e.preventDefault(); });
zone.addEventListener('dragenter', (e) => {
  if (ended || !carriesFiles(e)) return;
  e.preventDefault();
  if (dragDepth++ === 0) dropping(true);
});
zone.addEventListener('dragleave', (e) => {
  if (!carriesFiles(e)) return;
  if (--dragDepth <= 0) {
    dragDepth = 0;
    dropping(false);
  }
});
zone.addEventListener('dragover', (e) => { if (!ended && carriesFiles(e)) e.preventDefault(); });
zone.addEventListener('drop', (e) => {
  if (!carriesFiles(e)) return;
  e.preventDefault();
  dragDepth = 0;
  dropping(false);
  if (!ended) addFiles(e.dataTransfer.files);
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
    // The history replays right after the stream opens: judge emptiness once it did.
    setTimeout(showEmpty, 500);
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
    renderMentions();
    messages.querySelectorAll('mark.mention-chip.persona').forEach(paintChip);
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

async function handOff(p) {
  if (!(await grillConfirm('Start ' + p.name + ' fresh, keeping a summary?', 'Its last answer on this session writes docs/_handoff-' + p.name
    + '.md, then a new session starts from that note and the transcripts.', 'Start fresh'))) return null;
  return sendJSON('POST', api + '/turns', { persona_id: p.id, handoff: true });
}
// unfolded holds the persona sheet's opened Advanced and More sections, by persona.
const unfolded = new Set();
// A persona signs documents and revisions by name: show its badge while one holds that name.
function personaLabel(name) {
  const p = agent.personas.find((x) => x.name === name);
  return p ? p.badge + ' ' + name : name;
}

// A turn the host's machine holds in its own queue, behind turns of other rooms, is "queued (host
// busy)" (003-AC18).
const runningTurn = (p) => (agent.running || []).find((t) => t.persona_id === p.id);
const turnText = (p, t) => p.name + (t.host_queued ? ' waiting (the AI host is busy)' : ' answering');

// personaState is a pill's state: its class draws the shape, word is what a wide screen writes.
function personaState(p) {
  const running = runningTurn(p);
  const pos = agent.queue.findIndex((q) => q.persona_id === p.id);
  if (p.pending) return { cls: 'pending', word: 'needs approval', label: p.name + ' waits for the AI host\'s approval' };
  if (!agent.online) return { cls: '', word: 'offline', label: p.name + ' offline' };
  if (running) return { cls: running.host_queued ? 'queued' : 'busy', word: running.host_queued ? 'waiting' : 'answering', label: turnText(p, running) };
  if (pos >= 0) return { cls: 'queued', word: 'waiting', label: p.name + ' waiting, position ' + (pos + 1) };
  return { cls: 'on', word: 'ready', label: p.name + ' ready' };
}

// whyNot says why a pill does nothing on a press, or '' when it asks (005-AC35): a title tooltip
// never shows on touch, so a press opens the reason instead.
function whyNot(p, st) {
  const host = participants.find((x) => x.id === agent.host_participant_id);
  if (!agent.online) return host ? host.name + '\'s AI host app is not connected: ' + p.name + ' answers once it is.' : 'No one runs the AI for this room yet. Anyone can, from ⋯ › Run the AI on my computer.';
  if (!canPress()) return 'Only ' + (host ? host.name : 'the AI host') + ' can let you ask ' + p.name + '. Ask them to tick "Can ask the AI" next to your name. You can still mention @' + p.name + ' in a message.';
  if (st.cls === 'pending') return p.name + ' waits for the AI host\'s approval.';
  if (st.cls === 'queued') return p.name + ' already waits for its turn.';
  return '';
}

function renderAgent() {
  hidePairingOnceConnected();
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
      const word = document.createElement('span');
      word.className = 'pstate';
      b.append(led, badge, name, word);
      // A right-click, or a long press on a touch screen, opens that persona's settings. iOS fires
      // no contextmenu: a press held 500 ms does it there, and the click that follows is dropped.
      let held = 0;
      let longPressed = false;
      b.addEventListener('contextmenu', (e) => {
        e.preventDefault();
        openPersonas(Number(b.dataset.id));
      });
      b.addEventListener('pointerdown', (e) => {
        if (e.pointerType !== 'touch') return;
        longPressed = false;
        held = setTimeout(() => {
          longPressed = true;
          openPersonas(Number(b.dataset.id));
        }, 500);
      });
      for (const ev of ['pointerup', 'pointercancel', 'pointerleave']) b.addEventListener(ev, () => clearTimeout(held));
      b.addEventListener('click', (e) => {
        if (longPressed) {
          longPressed = false;
          e.preventDefault();
          return;
        }
        const p = personaByID(Number(b.dataset.id));
        const why = p && whyNot(p, personaState(p));
        if (why) return explain(p.badge + ' ' + p.name, why);
        pressPersona(Number(b.dataset.id), false);
      });
    }
    const st = personaState(p);
    const why = whyNot(p, st);
    b.style.setProperty('--persona', p.colour);
    b.setAttribute('aria-label', 'Ask ' + p.name);
    b.dataset.name = p.name;
    b.querySelector('.badge').textContent = p.badge;
    b.querySelector('.pname').textContent = p.name;
    const locked = agent.online && !canPress();
    b.querySelector('.pstate').textContent = locked ? 'locked' : st.word;
    const led = b.querySelector('.led');
    led.className = 'led ' + st.cls + (locked ? ' locked' : '');
    led.setAttribute('aria-label', st.label);
    // Not disabled: a press must still reach the reason.
    b.setAttribute('aria-disabled', String(!!why));
    b.title = why || st.label;
    return b;
  });
  if (buttons.length !== bar.children.length || buttons.some((b, i) => bar.children[i] !== b)) bar.replaceChildren(...buttons);
  const running = (agent.running || []).map((t) => [personaByID(t.persona_id), t]).filter(([p]) => p);
  $('agent-status').textContent = !agent.online ? (agent.reason || 'AI offline')
    : running.length ? running.map(([p, t]) => turnText(p, t)).join(', ') + '…' : agent.personas.length === 0 ? 'No persona: add one.' : '';
  renderRunning();
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
  // The counter shows only while the personas call each other (005-AC32).
  $('chain-count').textContent = c.suspended ? 'auto ' + c.count + ', no limit' : 'auto ' + c.count + '/' + c.budget;
  $('chain-count').hidden = c.count === 0 && !c.suspended;
  $('chain-unlimited').hidden = !c.suspended;
  $('chain-paused').hidden = !paused;
  $('chain-paused-text').textContent = 'The personas reached the limit of ' + c.budget + ' auto turns: they cannot call each other until someone resumes them.'
    + (canPress() ? '' : ' The AI host, or someone they let ask the AI, can resume them.');
  for (const id of ['chain-rearm', 'chain-suspend', 'chain-restore']) $(id).hidden = !canPress();
  if (document.activeElement !== $('chain-budget')) $('chain-budget').value = c.budget;
  $('chain-budget').disabled = !canPress();
}

function steerChain(body) {
  return sendJSON('PATCH', api + '/chain', body).catch((err) => { agentError(err.message); });
}
$('chain-rearm').addEventListener('click', () => steerChain({ rearm: true }));
$('chain-suspend').addEventListener('click', () => {
  grillConfirm('Remove the limit on auto turns?', 'The personas may then call each other without end, on the AI host\'s subscription.', 'Remove the limit')
    .then((yes) => { if (yes) steerChain({ suspended: true }); });
});
$('chain-restore').addEventListener('click', () => steerChain({ suspended: false }));
$('chain-form').addEventListener('submit', (e) => {
  e.preventDefault();
  sendJSON('PATCH', api + '/chain', { budget: Number($('chain-budget').value) })
    .then((c) => { if (c) personasStatus('Auto-turn limit set to ' + c.budget + '.'); })
    .catch((err) => personasStatus(err.message));
});

// renderApprovals is the banner of what waits for the host: personas asking above the host cap
// (002-AC19) and persona changes held while the host approves everything (002-AC21). Everyone sees
// it; only the host answers.
function renderApprovals() {
  const items = [];
  const decide = (li, what, path, body) => {
    if (!isHost()) {
      li.append(' The AI host decides.');
      return;
    }
    for (const approve of [true, false]) {
      const b = document.createElement('button');
      b.type = 'button';
            b.textContent = approve ? 'Approve' : 'Refuse';
      b.setAttribute('aria-label', (approve ? 'Approve ' : 'Refuse ') + what);
      b.addEventListener('click', () => {
        sendJSON('POST', api + path, { ...body, approve }).catch((err) => { agentError(err.message); });
      });
      li.append(' ', b);
    }
  };
  for (const r of agent.cap_requests || []) {
    const p = personaByID(r.persona_id);
    if (!p) continue;
    const li = document.createElement('li');
    li.textContent = p.badge + ' ' + p.name + ' asks for ' + level(r.asks) + ', above the AI host\'s limit: it runs at ' + level(r.runs) + '.';
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
            b.textContent = 'Start fresh, keep a summary';
      b.setAttribute('aria-label', 'Start fresh with a summary for ' + p.name);
      b.addEventListener('click', () => handOff(p).catch((err) => { agentError(err.message); }));
      li.append(' ', b);
    } else {
      li.append(' The AI host can start it fresh, keeping a summary.');
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
    li.textContent = k.from_starter ? 'The room\'s starter brought the skill ' + k.name + ' v' + k.version + '.'
      : k.author + ' copied the skill ' + k.name + ' v' + k.version + ' into the room.';
    decide(li, 'the room skill ' + k.name, '/skills/' + k.id + '/decision', {});
    items.push(li);
  }
  $('approvals').hidden = items.length === 0;
  $('approval-list').replaceChildren(...items);
}

// Whoever started a turn, the host or a room admin stops it while it runs (005-AC5).
function mayStop(t) {
  const me = participants.find((x) => String(x.id) === myID);
  return String(t.participant_id) === myID || isHost() || !!(me && me.admin);
}

function renderRunning() {
  const running = agent.running || [];
  $('running').hidden = running.length === 0;
  $('running-list').replaceChildren(...running.map((t) => {
    const li = document.createElement('li');
    const p = personaByID(t.persona_id) || { name: t.persona_name, badge: '' };
    li.append((p.badge ? p.badge + ' ' : '') + turnText(p, t) + (t.cause ? ', ' + t.cause : ''));
    if (mayStop(t)) {
      const stop = document.createElement('button');
      stop.type = 'button';
            stop.textContent = 'Stop';
      stop.setAttribute('aria-label', 'Stop the turn of ' + p.name);
      stop.addEventListener('click', () => {
        stop.disabled = true;
        sendJSON('POST', api + '/turns/' + t.id + '/stop', {}).catch((err) => {
          stop.disabled = false;
          agentError(err.message);
        });
      });
      li.append(' ', stop);
    }
    return li;
  }));
}

function renderQueue() {
  $('queue').hidden = agent.queue.length === 0;
  $('queue-list').replaceChildren(...agent.queue.map((q) => {
    const li = document.createElement('li');
    const p = personaByID(q.persona_id);
    const who = participants.find((x) => x.id === q.participant_id);
    const by = { mention: ', mentioned by ', call: ', called by a persona', every_n: ', every ' + (p ? p.every_n : 'N') + ' answers' }[q.source];
    const label = (p ? p.badge + ' ' + p.name : 'a persona') + (q.cause ? ', ' + q.cause
      : (by === undefined ? ', asked by ' : by) + (q.participant_id ? (who ? who.name : 'someone') : ''));
    li.append(label);
    if (String(q.participant_id) === myID || (!q.participant_id && canPress())) {
      const cancel = document.createElement('button');
      cancel.type = 'button';
            cancel.textContent = 'Cancel';
      cancel.setAttribute('aria-label', 'Cancel the queued turn of ' + (p ? p.name : 'this persona'));
      cancel.addEventListener('click', () => {
        netFetch(api + '/queue/' + q.id, { method: 'DELETE' }).catch(console.error);
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
    agentError(err.message);
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
    + (p.pending ? ' Waits for the AI host\'s approval.' : '')
    + (use === null ? '' : ' Its session holds ' + use + '% of its context window.')
    + nextTrigger(p);
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
  );
  const self = input('self_call', '', { type: 'checkbox', checked: p.self_call });
  const selfCall = field('', self);
  selfCall.className = 'check';
  selfCall.append('Can follow up on its own: its own @' + p.name + ' gives it another answer');
  // The rarely touched settings and actions fold away (005-AC40); a refresh keeps them as opened.
  const fold = (label, cls, ...children) => {
    const d = document.createElement('details');
    d.className = cls;
    const key = cls + ':' + p.id;
    d.open = unfolded.has(key);
    d.addEventListener('toggle', () => { if (d.open) unfolded.add(key); else unfolded.delete(key); });
    const sum = document.createElement('summary');
    sum.textContent = label;
    d.append(sum, ...children);
    return d;
  };
  // One automatic trigger at most (006-AC18): the choice, then its N.
  const kind = p.every_n ? 'every_n' : p.every_voice ? 'every_voice' : p.every_minutes ? 'every_minutes' : '';
  const speaks = picker('trigger', [], kind);
  speaks.replaceChildren(...[['', 'Only when asked'], ['every_n', 'Every N answers'], ['every_voice', 'Every N voice lines'],
    ['every_minutes', 'Every N minutes']].map(([v, label]) => option(v, label)));
  speaks.required = false;
  speaks.value = kind;
  const every = input('every', p.every_n || p.every_voice || p.every_minutes || 1, { type: 'number', min: 1, max: 500 });
  const everyField = field('N', every);
  everyField.hidden = !kind;
  // The server's bounds per kind (006-AC18).
  // An empty N would turn the trigger off unseen: it is required while a trigger is chosen.
  const bound = () => {
    every.max = String({ every_n: 100, every_voice: 500, every_minutes: 240 }[speaks.value] || 500);
    every.required = !!speaks.value;
  };
  bound();
  speaks.addEventListener('change', () => { everyField.hidden = !speaks.value; bound(); });
  const advanced = fold('Advanced', 'advanced',
    field('Speaks up', speaks), everyField,
    field('Memory size (k tokens)', input('autocompact', p.autocompact ? p.autocompact / 1000 : '', { type: 'number', min: 100, max: 1000,
      placeholder: 'auto', title: 'How much it remembers before it summarizes its session; empty for automatic' })),
    selfCall);
  const actions = document.createElement('div');
  actions.className = 'persona-actions';
  const save = document.createElement('button');
  save.type = 'submit';
  save.textContent = 'Save';
  const moreBox = document.createElement('div');
  moreBox.className = 'persona-actions';
  const act = (label, aria, fn, into = moreBox, cls = 'btn-secondary') => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = cls;
    b.textContent = label;
    b.setAttribute('aria-label', aria);
    b.addEventListener('click', () => fn().catch((err) => personasStatus(err.message)));
    into.append(b);
    return b;
  };
  if (p.update_version) {
    act('Update to v' + p.update_version, 'Update ' + p.name + ' to template version ' + p.update_version, async () => {
      const done = await sendJSON('POST', api + '/personas/' + p.id + '/update');
      if (done) personasStatus(done.held ? p.name + ': the update waits for the AI host\'s approval.' : p.name + ' updated.');
    });
  }
  const mine = tpl && tpl.mine;
  act(mine ? 'Save to the library as v' + (tpl.version + 1) : 'Save to the library', 'Save ' + p.name + ' to the library', async () => {
    const body = formFields(form);
    if (mine) body.template_id = tpl.id;
    const t = await sendJSON('POST', api + '/templates', body);
    if (t) personasStatus('Saved "' + t.title + '" v' + t.version + ' to the library.');
    await loadTemplates();
  });
  if (isHost()) {
    act('Start fresh', 'Start fresh for ' + p.name, async () => {
      if (await grillConfirm('Start ' + p.name + ' fresh?', 'It forgets its earlier answers and rereads the room history.', 'Start fresh')) await pressPersona(p.id, true);
    });
    act('Start fresh, keep a summary', 'Start fresh with a summary for ' + p.name, () => handOff(p));
  }
  actions.append(save);
  act('Remove', 'Remove ' + p.name, async () => {
    if (!(await grillConfirm('Remove ' + p.name + '?', 'Its session goes; what it wrote stays.', 'Remove'))) return;
    const res = await netFetch(api + '/personas/' + p.id, { method: 'DELETE' });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || 'HTTP ' + res.status);
  }, actions, 'btn-danger-outline');
  form.append(head, from, grid, field('Persona text', text), skillsBlock(p), advanced, actions, fold('More', 'more', moreBox));
  form.addEventListener('input', () => { form.dataset.dirty = '1'; });
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    sendJSON('PATCH', api + '/personas/' + p.id, formFields(form)).then((saved) => {
      if (!saved) return;
      delete form.dataset.dirty;
      personasStatus(saved.held ? saved.name + ': the change waits for the AI host\'s approval.' : saved.name + ' saved. It applies from its next turn.');
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
    li.textContent = k.name + ' v' + k.version + ', copied by ' + k.author + (k.pending ? ': waits for the AI host\'s approval' : '');
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
    if (k) personasStatus(k.name + ' v' + k.version + (k.pending ? ' copied. It waits for the AI host\'s approval.' : ' copied into the room.'));
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
      if (done) personasStatus(done.held ? p.name + ': the change waits for the AI host\'s approval.' : k.name + ' is off for ' + p.name + '.');
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
      personasStatus(done.held ? p.name + ': the change waits for the AI host\'s approval.' : (k ? k.name + ' v' + k.version + ' is on for ' + p.name + '.' : ''));
    }
  } finally {
    // A refusal may mean the library or the disclaimer state moved on: read them again.
    await loadSkills();
  }
}

async function loadSkills() {
  const [lib, st] = await Promise.all([netFetch(api + '/skills'), netFetch(api + '/agent')]);
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
    ...triggerFields(v('trigger'), Number(v('every'))), self_call: form.elements.namedItem('self_call').checked,
    autocompact: v('autocompact') ? Math.round(Number(v('autocompact')) * 1000) : 0 };
}

// triggerFields is the persona's automatic trigger as the API takes it: the one chosen at n, the
// other two off (006-AC18).
function triggerFields(kind, n) {
  return { every_n: kind === 'every_n' ? n : 0, every_voice: kind === 'every_voice' ? n : 0, every_minutes: kind === 'every_minutes' ? n : 0 };
}

// nextTrigger says when the persona speaks up next on its own (006-AC18).
function nextTrigger(p) {
  if (p.every_voice) return p.next_voice ? ' Speaks up next in ' + p.next_voice + ' voice lines.' : ' Speaks up at the next voice line.';
  if (!p.every_minutes || !p.next_at) return '';
  const min = Math.ceil((new Date(p.next_at).getTime() - Date.now()) / 60000);
  return min > 0 ? ' Speaks up next in ' + min + ' min.' : ' Speaks up at the next activity, while someone is here.';
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
  const res = await netFetch(api + '/templates');
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

// openPersonas opens the Personas sheet, scrolled to persona id when given (005-AC40).
function openPersonas(id) {
  personasStatus('');
  $('persona-list').replaceChildren();
  if (!$('personas').open) $('personas').showModal();
  renderPersonas();
  if (id) $('persona-list').querySelector('form[data-id="' + id + '"]')?.scrollIntoView({ block: 'start' });
  loadTemplates().catch(console.error);
  loadSkills().catch(console.error);
}
$('personas-open').addEventListener('click', () => openPersonas());
$('personas-close').addEventListener('click', () => $('personas').close());
$('persona-add').addEventListener('submit', (e) => {
  e.preventDefault();
  sendJSON('POST', api + '/personas', { template_id: Number($('persona-template').value) })
    .then((p) => { if (p) personasStatus(p.name + (p.pending ? ' added. It waits for the AI host\'s approval.' : ' added.')); })
    .catch((err) => personasStatus(err.message));
});

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
// Each role badge, its tooltip and the icon it shrinks to when a row has more than two (005-AC35).
const roles = [
  ['you', 'This is you', (p) => String(p.id) === myID, '●'],
  ['host', 'AI host: the personas run on their computer', (p) => agent.host_participant_id === p.id, '⚙'],
  ['admin', 'Room admin: manages the room, its invites and its admins', (p) => p.admin, '★'],
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

// ago says how long ago a time was, coarsely (kit/ui.js): the list refreshes every 30 seconds.
const ago = (ts) => grillAgo(ts);

function span(cls, text, title) {
  const el = document.createElement('span');
  el.className = cls;
  el.textContent = text;
  if (title) el.title = title;
  return el;
}

// tagColumns are the badges a person row may carry, in their fixed order (008-AC13): a column shows
// in every row of a list once any row has it, so a badge sits at the same place on every row.
const tagColumns = ['you', 'host', 'admin', 'ask', 'transcribed', 'audio'];

// personTags are a participant's badges by column: [icon, accessible name, tooltip].
function personTags(p, id, isHere) {
  const t = {};
  for (const [name, title, on, icon] of roles) if (on(p)) t[name] = [icon, name, title];
  if (agent.can_trigger.includes(p.id) && agent.host_participant_id !== p.id) t.ask = ['✦', 'Can ask the AI', 'Can ask the AI'];
  if (transcribed.has(id)) t.transcribed = ['✍', 'The transcription hears this voice', 'The transcription hears this voice'];
  const st = isHere ? audioState(id) : { key: 'none', icon: '', text: 'Not in the audio' };
  const deaf = isHere && !!presence.get(id)?.deaf;
  // One glyph per square: deafened shows 🔕 alone, its audio state in the tooltip.
  t.audio = [deaf ? '🔕' : st.icon, '', st.text + (deaf ? ', deafened: hears nobody' : ''), st.key, deaf];
  return t;
}

// personActions are what the viewer may do about p, for its ⋯ menu (008-AC13): none, no menu.
function personActions(p, id) {
  const acts = [];
  const other = id !== myID;
  if (other && meAdmin) acts.push({ label: 'Rename', run: () => openRename(p) });
  const host = agent.host_participant_id === p.id;
  if (other && isHost() && !host) {
    acts.push({ label: 'Can ask the AI', checked: agent.can_trigger.includes(p.id),
      run: () => sendJSON('PATCH', api + '/participants/' + id, { can_trigger: !agent.can_trigger.includes(p.id) }).catch((err) => agentError(err.message)) });
  }
  if (other && meAdmin) {
    acts.push({ label: p.admin ? 'Remove room admin' : 'Make room admin', run: () => {
      // The list as it is now: another room admin may have changed it since the menu opened.
      const now = participants.find((x) => String(x.id) === id) || p;
      sendJSON(now.admin ? 'DELETE' : 'PUT', api + '/admins/' + id).then(() => loadRoom()).catch((err) => toast(err.message));
    } });
  }
  if (other && isHost() && !host && agent.daemons.some((d) => d.participant_id === p.id && d.can_host)) {
    acts.push({ label: 'Make AI host', run: () => sendJSON('PUT', api + '/host', { participant_id: p.id }).catch((err) => agentError(err.message)) });
  }
  if (other && voiceParticipant(id)) acts.push({ volume: true });
  return acts;
}

// The person menu, built for the row it opens from; arrows move through it like the document's.
let personFor = null;
// openFor is the ⋯ whose menu was open when the press began: a light dismiss closes it before the
// click, which must then not open it again.
let openFor = null;
function openPersonMenu(b, p, id, fromPointer) {
  const menu = $('person-menu');
  const was = openFor;
  openFor = null;
  if (menu.matches(':popover-open')) menu.hidePopover();
  if (personFor) personFor.b.setAttribute('aria-expanded', 'false');
  if (fromPointer && was === b) return;
  personFor = { b, id };
  const items = personActions(p, id).map((a) => {
    if (a.volume) {
      const label = document.createElement('label');
      const vol = document.createElement('input');
      vol.type = 'range';
      vol.min = '0';
      vol.max = '1';
      vol.step = '0.05';
      vol.value = String(volumes.get(id) ?? 1);
      vol.addEventListener('input', () => {
        volumes.set(id, Number(vol.value));
        voiceParticipant(id)?.setVolume(Number(vol.value));
      });
      label.append('Volume ', vol);
      return label;
    }
    const it = document.createElement('button');
    it.type = 'button';
    it.textContent = a.label;
    if ('checked' in a) it.setAttribute('aria-pressed', String(a.checked));
    it.addEventListener('click', () => { menu.hidePopover(); a.run(); });
    return it;
  });
  const head = span('menu-head', p.name);
  menu.replaceChildren(head, ...items);
  const r = b.getBoundingClientRect();
  menu.style.top = Math.min(r.bottom + 4, innerHeight - 200) + 'px';
  menu.style.right = Math.max(8, innerWidth - r.right) + 'px';
  menu.showPopover();
  menu.style.top = Math.max(8, Math.min(r.bottom + 4, innerHeight - menu.offsetHeight - 8)) + 'px';
  b.setAttribute('aria-expanded', 'true');
  menu.querySelector('button, input')?.focus();
}
$('person-menu').addEventListener('toggle', (e) => {
  if (e.newState === 'closed' && personFor) personFor.b.setAttribute('aria-expanded', 'false');
});
arrowKeys($('person-menu'));

// renderParticipants splits the list into those in the room and those not here (005-AC7). Rows
// update in place: speaker changes fire many times a second, and a rebuilt row would close a
// menu or drop a focus.
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
      const more = document.createElement('button');
      more.type = 'button';
      more.className = 'person-more';
      more.textContent = '⋯';
      more.setAttribute('aria-expanded', 'false');
      li.append(span('voice-dot', ''), span('name', ''), span('tags', ''), more);
    }
    const isHere = id === myID || !!presence.get(id)?.here;
    li.querySelector('.name').textContent = p.name;
    const tags = personTags(p, id, isHere);
    const [, , , key, deaf] = tags.audio;
    li.classList.toggle('in-voice', key !== 'none');
    li.classList.toggle('speaking', key === 'live' && speaking.has(id));
    li.classList.toggle('muted', key === 'muted');
    li.classList.toggle('deaf', deaf);
    li.querySelector('.voice-dot').title = key === 'none' ? '' : key === 'live' && speaking.has(id) ? 'Speaking' : tags.audio[2];
    const more = li.querySelector('.person-more');
    more.setAttribute('aria-label', 'Actions for ' + p.name);
    more.onpointerdown = () => { openFor = $('person-menu').matches(':popover-open') ? personFor?.b : null; };
    more.onclick = (e) => openPersonMenu(more, p, id, e.detail > 0);
    const pr = presence.get(id);
    rows[isHere ? 'here' : 'away'].push({ li, tags, menu: personActions(p, id).length > 0,
      seen: isHere ? '' : 'seen ' + ago((pr && pr.seen_at) || p.last_seen_at || p.joined_at) });
  }
  for (const [list, want] of [[here, rows.here], [away, rows.away]]) {
    const cols = tagColumns.filter((c) => c === 'audio' || want.some((r) => r.tags[c] && r.tags[c][0]));
    const anyMenu = want.some((r) => r.menu);
    for (const { li, tags, menu, seen } of want) {
      li.querySelector('.tags').replaceChildren(...cols.map((c) => tagSpan(c, tags[c])), ...(seen ? [span('seen', seen)] : []));
      const more = li.querySelector('.person-more');
      more.hidden = !anyMenu;
      more.classList.toggle('blank', !menu);
      more.disabled = !menu;
    }
    const lis = want.map((r) => r.li);
    if (lis.length !== list.children.length || lis.some((li, i) => list.children[i] !== li)) list.replaceChildren(...lis);
  }
  $('away').hidden = !rows.away.length;
  $('away-count').textContent = String(rows.away.length);
  $('tab-people-count').textContent = String(rows.here.length);
}

// tagSpan is one badge square, or an empty one keeping its column. The audio one is always there
// with its state, for the people that are not in the audio too.
function tagSpan(col, t) {
  // The classes that name a state go on a filled square only.
  const on = !!(t && t[0]);
  const role = on && ['you', 'host', 'admin'].includes(col);
  const el = span('tag tag-' + col + (role ? ' role-' + col : '') + (on && col === 'host' ? ' host-badge' : '') + (on && col === 'transcribed' ? ' transcribed' : '') + (col === 'audio' ? ' audio' : ''), t ? t[0] : '');
  if (col === 'audio') {
    el.dataset.state = t[3];
    el.title = t[2];
    el.setAttribute('aria-label', t[2]);
  } else if (t) {
    el.title = t[2];
    el.setAttribute('aria-label', t[1]);
  }
  if (!on) el.classList.add('blank');
  el.setAttribute('role', 'img');
  return el;
}
setInterval(() => { if (participants.length) renderParticipants(); }, 30_000);

// On a phone the room is three tabs under one bar (005-AC36): the chat, the documents (list and
// viewer) and the people. Each keeps its scroll position across a switch.
let roomTab = 'chat';
const tabScroll = new Map();
const tabScroller = (t) => (t === 'chat' ? messages : $('side'));
function showTab(t) {
  if (t === roomTab) return;
  tabScroll.set(roomTab, tabScroller(roomTab).scrollTop);
  roomTab = t;
  document.body.dataset.tab = t;
  for (const b of $('tabbar').querySelectorAll('button')) b.setAttribute('aria-pressed', String(b.dataset.tab === t));
  tabScroller(t).scrollTop = tabScroll.get(t) ?? (t === 'chat' ? messages.scrollHeight : 0);
}
if (!docsMode) document.body.dataset.tab = roomTab;
for (const b of $('tabbar').querySelectorAll('button')) b.addEventListener('click', () => showTab(b.dataset.tab));

// showSTT counts the voices the STT worker transcribes, and tells viewers only whether their own
// is one of them (005-AC8).
function showSTT(st) {
  const el = $('stt-status');
  const voices = st.transcribed || [];
  transcribed = new Set(voices.map(String));
  renderParticipants();
  const mine = voices.some((id) => String(id) === myID);
  sttDown = !st.available;
  // It lives in the audio menu: only people in the audio read it (005-AC32).
  el.classList.toggle('down', sttDown);
  el.textContent = sttDown ? 'Voice-to-text is off'
    : !st.listening || !voices.length ? ''
    : (voices.length === 1 ? '1 voice is' : voices.length + ' voices are') + ' being written down' + (mine ? ' · yours is one of them' : '');
  $('voice-mute').classList.toggle('captured', mine);
  $('voice-mute').title = mine ? 'Your voice is being written down' : '';
  $('voice-more').classList.toggle('down', sttDown);
  // The model's license asks for credit wherever it runs (007-AC13).
  $('stt-credit').hidden = !(st.engines || []).includes('parakeet');
  if (sttGlossary !== !!st.glossary) {
    sttGlossary = !!st.glossary;
    showManage();
    // The Room settings sheet (admin/room-admin.js) shows its glossary tab from the same state.
    window.grillSTT = { glossary: sttGlossary };
    window.dispatchEvent(new CustomEvent('grill:stt', { detail: window.grillSTT }));
  }
}
let sttDown = false;
// sttGlossary is true while a transcription worker reads room glossaries (007-AC14).
let sttGlossary = false;

// showManage offers Room settings to room admins, and to every member while the glossary, their
// only tab (005-AC23), is in use.
function showManage() {
  $('manage-open').hidden = !meAdmin && !sttGlossary;
}

function voiceError(text) {
  $('voice-error').textContent = text;
  $('voice-error').hidden = !text;
}

function voiceButtons() {
  const on = !!voice;
  $('voice-join').hidden = on;
  $('voice-mute').hidden = !on;
  $('voice-more').hidden = !on;
  if (!on && $('voice-menu').matches(':popover-open')) $('voice-menu').hidePopover();
  $('voice-deaf').setAttribute('aria-pressed', String(on && deaf));
  $('voice-deaf').textContent = on && deaf ? 'Hear the room again' : 'Mute the room';
  const muted = on && !voice.localParticipant.isMicrophoneEnabled;
  $('voice-mute').setAttribute('aria-pressed', String(muted));
  const icon = document.createElement('span');
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = muted ? '🔇' : '🎙';
  $('voice-mute').replaceChildren(icon, muted ? 'Unmute' : 'Mute');
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
    // A focused tab holds the room's Web Push notifications back (005-AC24).
    focused: !gone && document.visibilityState === 'visible' && document.hasFocus(),
  });
  // keepalive lets the request outlive a closing page.
  netFetch(api + '/presence', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body, keepalive: !!gone })
    .catch(() => {});
}
setInterval(() => { if (events) beat(); }, 10_000);
window.addEventListener('pagehide', () => { if (events) beat(true); });
for (const ev of ['focus', 'blur']) window.addEventListener(ev, () => { if (events) beat(); });
document.addEventListener('visibilitychange', () => { if (events) beat(); });

// Web Push (005-AC24): one browser subscription, registered for each room where the participant
// turns notifications on. Turning them off here leaves the other rooms' alone.
const pushKey = 'grill-room-push:' + roomID;
const pushable = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
const ios = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
const homeScreen = matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;

function pushStatus(text) {
  $('push-status').textContent = text;
}

function renderPush() {
  const on = localStorage.getItem(pushKey) === '1';
  $('push-hint').hidden = !(ios && !homeScreen) && pushable;
  $('push-hint').textContent = ios && !homeScreen
    ? 'On iPhone and iPad, notifications work once the room is on the home screen: Share, Add to Home Screen, then open it from there.'
    : 'This browser does not support notifications.';
  $('push-toggle').hidden = !pushable;
  $('push-toggle').textContent = on ? 'Turn off notifications' : 'Turn on notifications';
  $('push-all-label').hidden = !pushable || !on;
  $('push-all').checked = localStorage.getItem(pushKey + ':all') === '1';
}

async function pushSubscription(create) {
  const reg = await navigator.serviceWorker.register('/sw.js');
  await navigator.serviceWorker.ready;
  const sub = await reg.pushManager.getSubscription();
  if (sub || !create) return sub;
  const res = await netFetch(api + '/push');
  if (!res.ok) throw new Error('HTTP ' + res.status);
  const key = Uint8Array.from(atob((await res.json()).key.replaceAll('-', '+').replaceAll('_', '/')), (c) => c.charCodeAt(0));
  return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
}

async function savePush() {
  const sub = await pushSubscription(true);
  const done = await sendJSON('PUT', api + '/push', { subscription: sub.toJSON(), all_messages: $('push-all').checked });
  if (done) localStorage.setItem(pushKey, '1');
}

const pushDenied = 'The browser did not allow notifications: allow them in its site settings, then try again.';
// The explainer comes before the browser's permission request (008-AC10), and once, unprompted, after the
// first message of a page load (008-AC11). Dismissing it snoozes the offer for 14 days in this
// browser; the Notifications sheet still reaches it.
const pushSnoozeKey = 'grill-room-push-snooze';
const pushSnooze = 14 * 24 * 3600e3;
let pushAccepted = false;
let pushOffered = false;
const snoozePush = () => localStorage.setItem(pushSnoozeKey, String(Date.now() + pushSnooze));

// pushReport says how turning on went: in the Notifications sheet, and in a toast when the
// explainer came unprompted.
function pushReport(text) {
  pushStatus(text);
  if (!$('notify').open) toast(text);
}

function turnOnPush() {
  localStorage.removeItem(pushSnoozeKey);
  savePush().then(() => 'Notifications on for this room.', (err) => 'Notifications: ' + err.message)
    .then(pushReport).finally(renderPush);
}

$('push-toggle').addEventListener('click', () => {
  pushStatus('');
  if (localStorage.getItem(pushKey) === '1') {
    (async () => {
      const sub = await pushSubscription(false);
      if (sub) await sendJSON('DELETE', api + '/push', { subscription: { endpoint: sub.endpoint } });
      localStorage.removeItem(pushKey);
      pushStatus('Notifications off for this room.');
    })().catch((err) => pushStatus('Notifications: ' + err.message)).finally(renderPush);
  } else if (Notification.permission === 'granted') {
    // Allowed already, from another room: nothing to ask.
    turnOnPush();
  } else if (Notification.permission === 'denied') {
    pushStatus(pushDenied);
  } else {
    $('push-explain').showModal();
  }
});

$('push-later').addEventListener('click', () => $('push-explain').close());
$('push-yes').addEventListener('click', () => {
  pushAccepted = true;
  $('push-explain').close();
  Notification.requestPermission().then((perm) => {
    if (perm === 'granted') return turnOnPush();
    // The browser's own prompt dismissed: snoozed too, as Chrome blocks a site that asks again
    // and again.
    if (perm === 'default') {
      snoozePush();
      pushReport('Notifications stay off. Turn them on under Notifications at any time.');
    } else pushReport(pushDenied);
  }).catch((err) => pushReport('Notifications: ' + err.message));
});
// A room that ends closes every sheet: that is no answer from the participant.
$('push-explain').addEventListener('close', () => {
  if (!pushAccepted && !ended) snoozePush();
  pushAccepted = false;
});
function offerPush() {
  if (pushOffered) return;
  pushOffered = true;
  if (!pushable || docsMode || (ios && !homeScreen) || Notification.permission !== 'default') return;
  if (localStorage.getItem(pushKey) === '1' || Date.now() < Number(localStorage.getItem(pushSnoozeKey) || 0)) return;
  if (!document.querySelector('dialog[open]')) $('push-explain').showModal();
}
$('push-all').addEventListener('change', () => {
  localStorage.setItem(pushKey + ':all', $('push-all').checked ? '1' : '0');
  savePush().catch((err) => pushStatus('Notifications: ' + err.message));
});
renderPush();
// The browser may have renewed its subscription since: hand the server the current one.
if (pushable && localStorage.getItem(pushKey) === '1' && Notification.permission === 'granted' && !docsMode) {
  savePush().catch((err) => pushStatus('Notifications: ' + err.message));
}

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
    const res = await netFetch(api + '/voice');
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
  const typed = bodyInput.value;
  const body = typed.trim();
  if (!body) return;
  const res = await netFetch(api + '/messages', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ body }),
  });
  if (res.status === 401) {
    end('Your access to this room has ended.');
    return;
  }
  if (!res.ok) throw new Error('send: HTTP ' + res.status);
  // Only the line sent leaves the box: the next one may already be typed in it.
  if (bodyInput.value === typed) bodyInput.value = '';
  nudgeRecovery();
  offerPush();
  const m = await res.json();
  if (m.mention_notices) {
    notices.set(m.id, m.mention_notices);
    const li = messages.querySelector('li[data-id="' + m.id + '"]');
    if (li) fillMentions(li);
  }
}

// @ autocompletion (005-AC2): the room's personas first, then the people, filtered as you type;
// arrows, Enter or Tab, and a tap pick one.
const mentionList = $('mention-list');
let mentionHits = [];
let mentionAt = -1;
let mentionPick = 0;

function suggest() {
  const before = bodyInput.value.slice(0, bodyInput.selectionStart);
  const found = bodyInput.selectionStart === bodyInput.selectionEnd && before.match(/(?:^|[^\p{L}\p{N}_@])@([\p{L}\p{N}_-]*)$/u);
  mentionHits = [];
  if (found) {
    const q = found[1].toLowerCase();
    mentionAt = before.length - found[1].length - 1;
    const all = [...agent.personas.map((p) => ({ token: p.name, label: p.badge + ' ' + p.name, kind: 'agent' })),
      ...participants.filter((p) => String(p.id) !== myID).map((p) => ({ token: mentionToken(p.name), label: p.name, kind: 'person' }))];
    const seen = new Set();
    for (const h of all) {
      const key = h.token.toLowerCase();
      if (h.token && key.startsWith(q) && !seen.has(key)) {
        seen.add(key);
        mentionHits.push(h);
      }
    }
    mentionHits = mentionHits.slice(0, 8);
  }
  mentionPick = 0;
  renderSuggestions();
}

function renderSuggestions() {
  const open = mentionHits.length > 0;
  mentionList.hidden = !open;
  bodyInput.setAttribute('aria-expanded', String(open));
  mentionList.replaceChildren(...mentionHits.map((h, i) => {
    const li = document.createElement('li');
    li.id = 'mention-' + i;
    li.setAttribute('role', 'option');
    li.setAttribute('aria-selected', String(i === mentionPick));
    const kind = document.createElement('span');
    kind.className = 'kind';
    kind.textContent = h.kind;
    li.append(h.label, kind);
    // On pointerdown, before the textarea loses focus to the tap.
    li.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      pickMention(i);
    });
    return li;
  }));
  if (open) bodyInput.setAttribute('aria-activedescendant', 'mention-' + mentionPick);
  else bodyInput.removeAttribute('aria-activedescendant');
}

function pickMention(i) {
  const text = '@' + mentionHits[i].token + ' ';
  const v = bodyInput.value;
  bodyInput.value = v.slice(0, mentionAt) + text + v.slice(bodyInput.selectionStart);
  bodyInput.setSelectionRange(mentionAt + text.length, mentionAt + text.length);
  bodyInput.focus();
  mentionHits = [];
  renderSuggestions();
}

// The keys' hint shows on the first focus only (005-AC33): the placeholder stays short.
bodyInput.addEventListener('focus', () => {
  if (localStorage.getItem('grill-room-send-hint')) return;
  localStorage.setItem('grill-room-send-hint', '1');
  $('send-hint').hidden = false;
  setTimeout(() => { $('send-hint').hidden = true; }, 8000);
}, { once: true });
bodyInput.addEventListener('input', suggest);
bodyInput.addEventListener('click', suggest);
bodyInput.addEventListener('blur', () => {
  mentionHits = [];
  renderSuggestions();
});

sendForm.addEventListener('submit', (e) => {
  e.preventDefault();
  send().catch(console.error);
});
bodyInput.addEventListener('keydown', (e) => {
  if (mentionHits.length && !e.isComposing) {
    const move = { ArrowDown: 1, ArrowUp: -1 }[e.key];
    if (move) {
      e.preventDefault();
      mentionPick = (mentionPick + move + mentionHits.length) % mentionHits.length;
      renderSuggestions();
      return;
    }
    if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault();
      pickMention(mentionPick);
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      mentionHits = [];
      renderSuggestions();
      return;
    }
  }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    send().catch(console.error);
  }
});

// Pairing: the daemon's local page opens this room with ?pair=<id>. Approving here, in the
// participant's own session, hands the daemon its token; nobody sees or types it.
// The pairing notice goes once the approved app is connected to the room (its daemon is listed),
// and ✕ dismisses it any time (008-AC17).
let pairingApproved = false;
let myDaemonsAtApproval = 0;
const myDaemons = () => agent.daemons.filter((d) => String(d.participant_id) === myID).length;
// Another of this participant's apps may already be connected: only one more counts.
function hidePairingOnceConnected() {
  if (pairingApproved && myDaemons() > myDaemonsAtApproval) $('pairing').hidden = true;
}
$('pairing-close').addEventListener('click', () => { $('pairing').hidden = true; });

async function showPairing(id) {
  history.replaceState(null, '', location.pathname);
  const url = api + '/pairings/' + encodeURIComponent(id);
  const text = $('pairing-text');
  // The buttons show once they answer: a click before the request loads would do nothing.
  $('pairing-actions').hidden = true;
  $('pairing').hidden = false;
  const res = await netFetch(url);
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
    // Before the request: the approved app may announce itself before the answer is read.
    myDaemonsAtApproval = myDaemons();
    const r = await netFetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ approve }),
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) text.textContent = data.error || 'Pairing failed: HTTP ' + r.status;
    else text.textContent = approve ? 'Daemon paired. Its page shows it connected within a few seconds.' : 'Pairing denied.';
    pairingApproved = approve && r.ok;
    hidePairingOnceConnected();
  };
  $('pairing-approve').onclick = () => answer(true).catch(console.error);
  $('pairing-deny').onclick = () => answer(false).catch(console.error);
  $('pairing-actions').hidden = false;
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
  const res = await netFetch('/api/daemon/version');
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
    navigator.clipboard.writeText(oneLiner).then(() => toast('Copied'), console.error);
  });
  $('host-install').hidden = false;
}

// Both ways to run the AI for this room (008-AC16): the app already running takes the room's link
// on its page; the install part shows when the server hands out a release.
$('host-room-link').value = location.origin + roomPath;
$('host-room-copy').addEventListener('click', () => copyField($('host-room-link'), $('host-room-copy')));
$('host-open').hidden = docsMode;

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

// firstLoad retries the first room load until it answers: a request the browser drops (a network
// change, a server restarting) must not leave the page empty for good.
async function firstLoad() {
  for (;;) {
    try {
      return await loadRoom();
    } catch (err) {
      console.error(err);
      await new Promise((resolve) => setTimeout(resolve, 2000));
    }
  }
}

firstLoad().then((ok) => {
  if (!ok) return;
  connect();
  if (docsMode) {
    const doc = decodeURIComponent(location.pathname.split('/')[4] || '');
    if (doc) showDocument(doc, false, true).catch(() => history.replaceState(null, '', roomPath + '/docs'));
    return;
  }
  // A room just created from the profile opens on its invite card (005-AC34).
  const query = new URLSearchParams(location.search);
  if (query.has('invite')) {
    query.delete('invite');
    history.replaceState(null, '', location.pathname + (query.size ? '?' + query : '') + location.hash);
    $('invite-open').click();
  }
  showHostAgent().catch(console.error);
  const pair = new URLSearchParams(location.search).get('pair');
  if (pair) showPairing(pair).catch(console.error);
}).catch(console.error);
