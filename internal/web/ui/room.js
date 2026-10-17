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
// stt/device.js's pipeline sends its device beats under the same tab (010-AC13).
window.grillTab = tab;
// presence holds the server's "presence" event by participant id; transcribed, the participants
// the STT worker hears.
let presence = new Map();
let transcribed = new Set();
// Personas speak in the call (spec 013): ttsOn when the server offers it, ttsLive the pod's states
// by persona id, voiceList the catalogue the voice picker shows, loaded once.
let ttsOn = false;
let ttsLive = { pod: false, personas: {} };
let voiceList = null;
// The personas this listener muted, by id, per room in this browser (013-AC28).
const personaMutedKey = 'grill-room-persona-muted:' + roomID;
const personaMuted = new Set(JSON.parse(localStorage.getItem(personaMutedKey) || '[]').map(String));
// cardFor is the name the open details card describes (009-AC36).
let cardFor = null;
// hoverTimer delays the card on hover; hideTimer is the grace before it closes.
let hoverTimer;
let hideTimer;

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
    link.dataset.tip = 'Back to the room';
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
  renderBlocked();
  renderApprovals();
  $('voice').hidden = !data.voice;
  ttsOn = !!data.tts;
  showManage();
  // An appointment or a step-down changes who turns plugins on (Capabilities).
  renderPersonaSheet();
  renderParticipants();
  showEmpty();
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
  if (pinned) messages.scrollTop = messages.scrollHeight;
  if (m.id === jumpTo) jump(m.id);
}

// An edit or delete replaces the line in place; one not shown yet arrives later as it stands. A
// replay of a change already shown leaves the line alone, with any edit box open on it.
function replaceMessage(m) {
  const li = messages.querySelector('li[data-id="' + m.id + '"]');
  if (li && m.change_seq > Number(li.dataset.seq)) li.replaceWith(renderMessage(m));
}

// blockedLines are the shown lines of websites a turn was refused, by id: their buttons follow the
// agent state (014-AC9).
const blockedLines = new Map();

// blockedActions are an undecided blocked line's buttons for whoever may approve it: the AI host
// for the room or everywhere, a room admin the host lets for the room only; disabled, with why,
// while the host's app cannot take it (014-AC9).
function blockedActions(m) {
  const b = m.blocked;
  if (b.decision || b.dismissed_by) return null;
  const box = document.createElement('span');
  box.className = 'blocked-actions';
  if (b.pending) {
    box.textContent = 'Allowing…';
    return box;
  }
  const scopes = approves() ? [['room', 'Allow for this room'], ['server', 'Allow everywhere']]
    : meAdmin && agent.egress_admins_allow ? [['room', 'Allow for this room']] : [];
  if (!scopes.length) {
    // Whoever cannot allow it learns who can, the same person on another seat included.
    box.append(Object.assign(document.createElement('span'), { className: 'hint', textContent: whoAllows() }));
    return box;
  }
  const why = agent.egress_host || '';
  for (const [scope, label] of scopes) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn-secondary';
    btn.textContent = label;
    btn.disabled = !!why;
    btn.addEventListener('click', () => {
      for (const x of box.querySelectorAll('button')) x.disabled = true;
      sendJSON('POST', api + '/blocked/' + m.id + '/allow', { scope }).catch((err) => {
        showNotice(err.message);
        for (const x of box.querySelectorAll('button')) x.disabled = !!why;
      });
    });
    box.append(btn, ' ');
  }
  const note = why || (b.error ? 'Not allowed: ' + b.error : '');
  if (note) box.append(Object.assign(document.createElement('span'), { className: 'hint', textContent: note }));
  return box;
}

// whoAllows names who may allow a blocked website, for a viewer who may not (014-AC9).
function whoAllows() {
  const host = participants.find((p) => p.id === agent.host_participant_id);
  if (!host) return 'Pending: whoever runs the AI for this room can allow it.';
  if (host.self) return 'Pending: you run the AI as ' + host.name + ': open the room as ' + host.name + ' to allow it.';
  return 'Pending: only the AI host, ' + host.name + (agent.egress_admins_allow ? ', or a room admin' : ',') + ' can allow it.';
}

// renderBlocked redraws the blocked lines' buttons after the agent state changed.
function renderBlocked() {
  for (const m of blockedLines.values()) {
    const li = messages.querySelector('li[data-id="' + m.id + '"]');
    if (li) li.replaceWith(renderMessage(m));
  }
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
  time.textContent = grillTime(ts);
  time.dataset.tip = grillDate(ts);
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
    if (m.blocked) {
      blockedLines.set(m.id, m);
      li.classList.add('blocked');
      const acts = blockedActions(m);
      if (acts) li.append(' ', acts);
    }
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
    // Focus shows the details card (009-AC36); not a Tab stop, or a long chat would take one per line.
    sender.tabIndex = -1;
    nameSender(li, sender);
  }
  meta.append(sender, ' ');
  // Who wrote a voice line down shows in its tip only (010-AC12); a line from before 010 is the server's.
  if (m.kind === 'voice') meta.append(voiceSource(m), ' ');
  meta.append(timeOf(m.ts));
  if (m.kind === 'agent') {
    // The model and effort the turn ran with (002-AC18), the model checked against what claude
    // reported (005-AC10). Lines from before 005-AC9 may carry no effort.
    const model = document.createElement('span');
    model.className = 'model';
    model.textContent = m.model || 'model unknown';
    model.dataset.tip = 'Claude model of this turn';
    const ran = [model];
    if (m.effort) {
      const effort = document.createElement('span');
      effort.className = 'effort';
      effort.textContent = 'effort ' + m.effort;
      effort.dataset.tip = 'Effort of this turn';
      ran.push(' · ', effort);
    }
    if (m.model_note || m.model_mismatch) {
      const note = document.createElement('span');
      note.className = 'model-note' + (m.model_mismatch ? ' mismatch' : '');
      note.textContent = m.model_mismatch ? '⚠ claude reported ' + (m.model_note || 'no model') : m.model_note;
      note.dataset.tip = m.model_mismatch ? 'Claude did not report the model this turn asked for' : 'Other models of this turn, with their share of its cost';
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

// voiceSource is a voice line's "(voice)" mark, which says who wrote it down in its tip, to the
// keyboard and the screen reader too, and on a tap (010-AC12).
function voiceSource(m) {
  const tip = m.source === 'device' ? 'Transcribed on ' + m.sender + "'s device" : 'Transcribed by the server';
  const el = span('voice-src', '(voice)', tip);
  el.tabIndex = 0;
  el.setAttribute('role', 'img');
  el.setAttribute('aria-label', 'Voice line. ' + tip);
  return el;
}
// Touch has no hover: a tap shows the tip.
document.addEventListener('click', (e) => {
  const v = e.target instanceof Element && e.target.closest('.voice-src');
  if (v && (e.pointerType === 'touch' || e.pointerType === 'pen')) grillTip.show(v);
});

// nameSender shows a person's line under their current name, and the name it was written under on
// hover once they renamed (005-AC1).
function nameSender(li, el) {
  const p = participants.find((x) => String(x.id) === li.dataset.pid);
  const was = li.dataset.sender;
  el.textContent = p ? p.name : was;
  // The details card says it (009-AC36): no native tooltip on author names.
  if (p && p.name !== was) el.dataset.writtenAs = was;
  else delete el.dataset.writtenAs;
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
    el.dataset.tip = (p ? p.name : n.at.slice(1)) + (String(n.participant_id) === myID ? ', you' : '');
  }
  return el;
}

// paintChip gives a persona chip its persona's badge and colour, once the agent state names them.
function paintChip(el) {
  const p = personaByID(Number(el.dataset.persona));
  el.dataset.tip = (p ? p.name : el.textContent.slice(1)) + ', persona';
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

// noticeText says to a note's author why the personas it names will not run (009-AC29).
const noticeText = (ns) => (ns || []).map((n) => '@' + n.name + ': ' + n.reason + '. Only you see this.').join(' ');

function mentionChip(s) {
  const p = personaByID(s.persona_id);
  const el = document.createElement(s.line_id ? 'a' : 'span');
  el.className = 'mention-turn ' + s.status;
  el.textContent = '→ @' + (p ? p.name : 'persona') + ' · ' + s.status;
  if (s.line_id) {
    el.href = '#';
    el.dataset.tip = s.status === 'done' ? 'Show the synthesis' : 'Show how the turn ended';
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
    when.textContent = grillDate(v.written_at);
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
  const list = document.createElement('ul');
  list.className = 'mention-list';
  list.id = 'edit-mention-list-' + m.id;
  list.setAttribute('role', 'listbox');
  list.setAttribute('aria-label', 'Mention');
  list.hidden = true;
  // #messages is a live region: the suggestions must not be read out on each keystroke.
  list.setAttribute('aria-live', 'off');
  const mentions = mentionAutocomplete(box, list);
  form.append(list, box, save, cancel);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    sendJSON('PATCH', api + '/messages/' + m.id, { body: box.value }).then((saved) => {
      if (saved && form.isConnected) li.replaceWith(renderMessage(saved));
    }).catch((err) => { box.setCustomValidity(err.message); box.reportValidity(); });
  });
  box.addEventListener('input', () => box.setCustomValidity(''));
  box.addEventListener('keydown', (e) => {
    if (mentions.keydown(e)) return;
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
  b.append(grillIcon('more'));
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
// A revoke from Room settings may have cut people off: People and the empty-chat card follow.
addEventListener('grill:revoked', () => loadRoom().catch(console.error));

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
// The document's and a persona's menus have their own (008-AC8).
for (const menu of document.querySelectorAll('.menu:not(#doc-menu, #person-menu, #persona-menu)')) {
  menu.addEventListener('click', (e) => { if (e.target.closest('button, a')) menu.hidePopover(); });
}
for (const b of document.querySelectorAll('dialog .close')) b.addEventListener('click', () => b.closest('dialog').close());
$('notify-open').addEventListener('click', () => $('notify').showModal());
$('invite-open').addEventListener('click', () => { openInvites(); });
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

// After the first message of a new profile, one notice offers the link. With email set up, the
// email nudge replaces it (011-AC8).
async function nudgeRecovery() {
  // A message sent before the profile answered must still get the email nudge.
  await ownLoaded;
  if (own.email_enabled) {
    nudgeEmail();
    return;
  }
  if (!sessionStorage.getItem(recoveryKey) || sessionStorage.getItem(recoveryKey + ':nudged')) return;
  sessionStorage.setItem(recoveryKey + ':nudged', '1');
  showNotice('To use this profile on another device, save its link: your initials, then "Use on another device".');
}

// own is GET /api/profile's answer: the room API says nothing of email state (011-AC6). It is read
// on load and when the tab comes back, as an email bound on /profile then takes the marker away (011-AC9).
let own = { email_enabled: false, profile: null };
const anonText = 'Profile without email: add one to find it on another device';
const noEmail = () => own.email_enabled && !!own.profile && !own.profile.email;
// ownLoaded is the latest loadOwn, settled.
let ownLoaded = Promise.resolve();
const reloadOwn = () => { ownLoaded = loadOwn().catch(console.error); };
async function loadOwn() {
  const res = await netFetch('/api/profile');
  if (!res.ok) return;
  own = await res.json();
  if (!$('me-anon').firstChild) {
    $('me-anon').dataset.tip = anonText;
    $('me-anon').setAttribute('aria-label', anonText);
    $('me-anon').append(badgeIcon('anon'));
  }
  $('me-anon').hidden = !noEmail();
  if (!noEmail()) $('email-nudge').hidden = true;
  if (participants.length) renderParticipants();
}
document.addEventListener('visibilitychange', () => { if (!document.hidden) reloadOwn(); });

// nudgeEmail shows the banner once per page, unless "Later" snoozed it on this profile (011-AC8).
let nudged = false;
function nudgeEmail() {
  const until = own.profile && own.profile.email_nudge_until;
  if (nudged || !noEmail() || (until && new Date(until) > new Date())) return;
  nudged = true;
  $('email-nudge').hidden = false;
}
for (const id of ['email-nudge-later', 'email-nudge-close']) {
  $(id).addEventListener('click', () => {
    const b = $(id);
    b.disabled = true;
    netFetch('/api/profile/nudge', { method: 'POST' }).then((res) => {
      if (!res.ok) throw new Error('HTTP ' + res.status);
      $('email-nudge').hidden = true;
    }).catch((err) => toast("Couldn't snooze: " + err.message)).finally(() => { b.disabled = false; });
  });
}

// Invites (011-AC26): the popup and the empty chat's card show your one shareable link, which the
// server makes on the first ask; extra links and revoking live in Room settings › Invites.
const shareLink = () => sendJSON('POST', api + '/invites/share').then((inv) => inv && inv.url);
function openInvites() {
  $('invite-link').value = '';
  $('invite-copy').disabled = true;
  $('invite-status').textContent = '';
  $('invites').showModal();
  return shareLink().then((url) => {
    if (!url) return;
    $('invite-link').value = url;
    $('invite-copy').disabled = false;
  }).catch((err) => { $('invite-status').textContent = err.message; });
}
$('invite-copy').addEventListener('click', () => copyField($('invite-link')));
$('invite-manage').addEventListener('click', () => $('invites').close());

// The empty chat's card copies the same link in one press (005-AC34); a refused clipboard opens
// the popup on it.
$('chat-invite').addEventListener('click', () => {
  const b = $('chat-invite');
  b.disabled = true;
  $('chat-invite-status').textContent = '';
  shareLink().then((url) => {
    if (!url) return;
    return navigator.clipboard.writeText(url).then(() => { $('chat-invite-status').textContent = 'Copied. Paste it to the people you invite.'; },
      () => openInvites().then(() => $('invite-link').select()));
  }).catch((err) => { $('chat-invite-status').textContent = err.message; }).finally(() => { b.disabled = false; });
});

// showEmpty shows the invite card while nobody else has joined, whatever the chat holds (011-AC26):
// the room's list holds only the participants whose invite is still valid, and self marks your
// profile's other seats.
function showEmpty() {
  $('chat-empty').hidden = ended || docsMode || participants.some((p) => String(p.id) !== myID && !p.self);
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
    // One line with an ellipsis: the full name in the tip (008-AC14).
    b.dataset.tip = d.name + ', ' + describe(d);
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
  $('doc-name').dataset.tip = d.name;
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
    $('rev').replaceChildren(...revisions.map((r) => option(r.n, 'r' + r.n + ' · ' + personaLabel(r.author) + ' · ' + grillDate(r.created_at))));
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

// attachImages wires a notes form's paperclip (docs/design.md §5, File input): the native input
// stays hidden, and the files chosen show as a chip whose ✕ drops them. Returns its redraw.
function attachImages(wrap) {
  const input = wrap.querySelector('input[type=file]');
  wrap.querySelector('.attach-open').addEventListener('click', () => input.click());
  const chip = document.createElement('span');
  chip.className = 'chip file-chip';
  chip.hidden = true;
  const name = document.createElement('span');
  name.className = 'file-name';
  const drop = document.createElement('button');
  drop.type = 'button';
  drop.className = 'btn-icon btn-ghost';
  drop.setAttribute('aria-label', 'Remove the images');
  drop.append(grillIcon('close'));
  const show = () => {
    const n = input.files.length;
    chip.hidden = !n;
    name.textContent = n === 1 ? input.files[0].name : n + ' images';
  };
  drop.addEventListener('click', () => {
    input.value = '';
    show();
  });
  chip.append(name, drop);
  wrap.append(chip);
  input.addEventListener('change', show);
  // A form's reset clears its inputs after its reset event.
  input.form?.addEventListener('reset', () => queueMicrotask(show));
  return show;
}
const showNewImages = attachImages($('annot-new').querySelector('.attach'));

function closeNew() {
  newAnchor = undefined;
  $('annot-new').hidden = true;
  $('annot-body').value = '';
  $('annot-files').value = '';
  showNewImages();
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
    const said = $('annot-list').querySelector(`li[data-id="${saved.id}"] .annot-error`);
    if (said) said.textContent = noticeText(saved.notices);
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
  $('doc-width').dataset.tip = 'Text width: ' + mdWidths[mdWidth] + '. Press for ' + mdWidths[nextWidth()] + '.';
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

// actionMenu makes a ⋯ menu (008-AC8): under its button, the first item focused, arrows move.
// The document's and a persona's sheet's use it.
function actionMenu(menu, button) {
  menu.addEventListener('beforetoggle', (e) => {
    if (e.newState !== 'open') return;
    const r = button.getBoundingClientRect();
    menu.style.top = (r.bottom + 4) + 'px';
    menu.style.right = Math.max(8, innerWidth - r.right) + 'px';
  });
  menu.addEventListener('toggle', (e) => { if (e.newState === 'open') menuItems(menu)[0]?.focus(); });
  arrowKeys(menu);
  // Capture: the menu closes, its focus back on ⋯, before the item's own action moves the focus.
  menu.addEventListener('click', (e) => { if (e.target.closest('a, button')) menu.hidePopover(); }, true);
}
actionMenu($('doc-menu'), $('doc-more'));

function renderAnnotations() {
  $('annot-count').textContent = annotations.length ? '(' + annotations.length + ')' : '';
  $('annot-empty').hidden = annotations.length > 0;
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
    + '<div class="annot-actions"><span class="attach"><button type="button" class="btn-icon btn-ghost attach-open" aria-label="Attach images" data-tip="Attach images"></button>'
    + '<input type="file" accept="image/png,image/jpeg,image/gif,image/webp" multiple hidden></span>'
    + '<button type="button" class="annot-state"></button><button type="submit">Reply</button></div>'
    + '<p class="annot-error" role="status"></p></form>';
  li.querySelector('.annot-head').addEventListener('click', () => focusItem(id, true));
  li.querySelector('.attach-open').append(grillIcon('clip'));
  attachImages(li.querySelector('.attach'));
  const form = li.querySelector('form');
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    (async () => {
      const attachments = await readImages(form.querySelector('input[type=file]'));
      const body = form.querySelector('textarea').value;
      const sent = await sendJSON('POST', api + '/annotations/' + id + '/messages', { body, attachments });
      if (!sent) return;
      form.reset();
      form.querySelector('.annot-error').textContent = noticeText(sent.notices);
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
    time.textContent = grillTime(m.created_at);
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
    // 009-AC29: the turns this message's @mentions queued, live from the agent state.
    const live = (agent.note_mentions || []).filter((s) => s.message_id === m.id);
    const states = live.length ? live : m.mentions || [];
    if (states.length) {
      const box = document.createElement('div');
      box.className = 'mentions';
      box.append(...states.map(mentionChip));
      item.append(box);
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
    state.replaceChildren(grillIcon('alert'));
    state.setAttribute('role', 'img');
    state.setAttribute('aria-label', 'Not added');
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
    drop.append(grillIcon('close'));
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
    state.replaceChildren(grillIcon('check'));
    state.className = 'state done';
    state.setAttribute('role', 'img');
    state.setAttribute('aria-label', 'Added');
    setTimeout(() => li.remove(), 5000);
    if (open) showDocument(data.id).catch(console.error);
  });
  xhr.addEventListener('error', () => fail('The connection dropped. Check it, then retry.'));
  const form = new FormData();
  form.append('file', file);
  xhr.send(form);
}

$('upload-open').addEventListener('click', () => $('upload-input').click());
$('docs-empty-add').addEventListener('click', () => $('upload-input').click());
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
    beat();
    loadDocuments().then(loadAnnotations).catch(console.error);
    loadPlugins().catch(console.error);
  };
  es.addEventListener('message_edited', (e) => {
    const m = JSON.parse(e.data);
    if (m.change_seq > editSeq) editSeq = m.change_seq;
    replaceMessage(m);
  });
  es.addEventListener('documents', () => loadDocuments().catch(console.error));
  es.addEventListener('annotations', () => loadAnnotations().catch(console.error));
  es.addEventListener('plugins', () => loadPlugins().catch(console.error));
  es.addEventListener('revoked', () => end('Your access to this room has ended.'));
  es.addEventListener('stt', (e) => showSTT(JSON.parse(e.data)));
  es.addEventListener('tts', (e) => {
    ttsLive = JSON.parse(e.data);
    renderAgent();
  });
  es.addEventListener('presence', (e) => {
    presence = new Map(JSON.parse(e.data).people.map((p) => [String(p.id), p]));
    renderParticipants();
  });
  es.addEventListener('agent', (e) => {
    const wasHost = agentSeen && isHost();
    const wasOnline = agent.online;
    agent = JSON.parse(e.data);
    // A version under inspection says whether the AI host is there to check it (009-AC13).
    if (agentSeen && wasOnline !== agent.online && plugins.length) loadPlugins().catch(console.error);
    // Someone else moved the role away from this participant (009-AC34).
    if (wasHost && !isHost() && agent.host_by) {
      const standby = agent.daemons.some((d) => String(d.participant_id) === myID);
      showNotice(agent.host_by + '.' + (standby ? ' Your AI host app stays connected as a standby.' : ''));
    }
    // A deep link to a persona's sheet opens once the room's personas are known (009-AC2).
    if (!agentSeen) {
      agentSeen = true;
      followPersona();
    }
    renderAgent();
    renderMentions();
    renderBlocked();
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
// The room's plugins, as GET .../plugins lists them, read again on each "plugins" event;
// pluginsSeen tells the persona form that the list moved on.
let plugins = [];
let pluginsSeen = 0;
let agent = { online: false, can_trigger: [], daemons: [], turn: null, running: [], personas: [], queue: [], models: [], efforts: [], cap_requests: [],
  changes: [], room_skills: [], chain: { budget: 25, count: 0, suspended: false }, parallel_turns: false };

// level shows a model and effort as the room says them, "opus · effort max". Both are always set
// since 005-AC9; "unset" only shows a line written before.
const level = (l) => (l.model || 'unset') + ' · effort ' + (l.effort || 'unset');

const isHost = () => String(agent.host_participant_id || '') === myID;
// Approvals follow the host's profile: any of its seats in this room answers them (daemon-r2).
const approves = () => isHost() || !!agent.you_host;
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

// The pill's run button (013-AC39): one constant, so its icon is a one-line change.
const RUN_ICON = 'play';

// personaVoice is the persona's state in the call while the tts pod reports it, null otherwise (013-AC29).
function personaVoice(p) {
  const st = ttsOn && ttsLive.pod && ttsLive.personas[p.id];
  if (!st) return null;
  if (st.state === 'speaking') return { icon: 'speaking', word: 'speaking', label: p.name + ' is speaking in the call' };
  if (st.state === 'waiting') return { icon: 'speaking-wait', word: 'waiting to speak', label: p.name + ' is waiting to speak in the call' };
  return null;
}

// personaPill builds a persona's pill: a click opens its card, ▶ runs it, the speaker mutes it for
// this listener (013-AC39, 013-AC28). A right-click, or a long press on a touch screen, opens its
// sheet (009-AC4). iOS fires no contextmenu: a press held 500 ms does it there.
function personaPill(id) {
  const pill = document.createElement('span');
  pill.className = 'persona-pill';
  pill.dataset.id = id;
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'persona';
  const led = document.createElement('span');
  led.className = 'led';
  led.setAttribute('role', 'img');
  const vstate = span('vstate', '');
  vstate.setAttribute('aria-hidden', 'true');
  b.append(led, span('badge', ''), span('pname', ''), watchIcon(), vstate, span('pstate', ''));
  let held = 0;
  let longPressed = false;
  const pid = () => Number(pill.dataset.id);
  b.addEventListener('contextmenu', (e) => {
    e.preventDefault();
    openPersonaSheet(pid());
  });
  b.addEventListener('pointerdown', (e) => {
    if (e.pointerType !== 'touch') return;
    longPressed = false;
    held = setTimeout(() => {
      longPressed = true;
      openPersonaSheet(pid());
    }, 500);
  });
  for (const ev of ['pointerup', 'pointercancel', 'pointerleave']) b.addEventListener(ev, () => clearTimeout(held));
  b.addEventListener('click', (e) => {
    if (longPressed) {
      longPressed = false;
      e.preventDefault();
      return;
    }
    openPersonaCard(pid(), pill);
  });
  const run = document.createElement('button');
  run.type = 'button';
  run.className = 'persona-run';
  run.append(grillIcon(RUN_ICON));
  run.addEventListener('click', () => askPersona(pid()));
  const hear = document.createElement('button');
  hear.type = 'button';
  hear.className = 'persona-hear';
  hear.hidden = true;
  hear.addEventListener('click', () => setPersonaMuted(pid(), !personaMuted.has(String(pid()))));
  pill.append(b, run, hear);
  return pill;
}

// askPersona runs the persona now, or says why not (005-AC35).
function askPersona(id) {
  const p = personaByID(id);
  if (!p) return;
  const why = whyNot(p, personaState(p));
  if (why) return explain(p.badge + ' ' + p.name, why);
  pressPersona(id, false);
}

// setPersonaMuted mutes a persona for this listener only: its track is unsubscribed, which saves
// the bandwidth too, and the choice is kept for the room in this browser (013-AC28).
function setPersonaMuted(id, muted) {
  if (muted) personaMuted.add(String(id));
  else personaMuted.delete(String(id));
  localStorage.setItem(personaMutedKey, JSON.stringify([...personaMuted]));
  const bot = voice && voice.remoteParticipants.get('persona-' + id);
  if (bot) {
    for (const pub of bot.audioTrackPublications.values()) {
      // The element goes at once: LiveKit reports the unsubscription only later, if at all.
      if (muted) pub.track?.detach().forEach((el) => el.remove());
      pub.setSubscribed(!muted && !deaf);
    }
  }
  renderAgent();
}

// personaMutedIdentity reports a LiveKit participant as a persona this listener muted.
const personaMutedIdentity = (identity) => identity.startsWith('persona-') && personaMuted.has(identity.slice(8));

// The persona card (013-AC39): what the persona is set to and what this listener can do with it.
let personaCardFor = 0;
let personaCardSeen = '';
function openPersonaCard(id, anchor) {
  personaCardFor = id;
  personaCardSeen = '';
  renderPersonaCard();
  const card = $('persona-card');
  if (!card.matches(':popover-open')) card.showPopover();
  grillPlace(card, anchor);
  card.querySelector('button, input')?.focus();
}
$('persona-card').addEventListener('toggle', (e) => { if (e.newState === 'closed') personaCardFor = 0; });

function renderPersonaCard() {
  const card = $('persona-card');
  const p = personaCardFor && personaByID(personaCardFor);
  if (!p) {
    if (card.matches(':popover-open')) card.hidePopover();
    return;
  }
  const st = personaState(p);
  const vs = personaVoice(p);
  const muted = personaMuted.has(String(p.id));
  const key = JSON.stringify([p, st, vs, muted, ttsOn, voiceList && voiceList.voices.length, whyNot(p, st)]);
  if (key === personaCardSeen) return;
  personaCardSeen = key;
  // A redraw keeps the focus on the control that had it: a keyboard user toggling Mute stays on it.
  const focused = card.contains(document.activeElement) ? document.activeElement.id : '';
  const title = document.createElement('h3');
  title.id = 'persona-card-title';
  title.append(span('badge persona-badge', p.badge), ' ' + p.name);
  const rows = document.createElement('dl');
  const row = (k, v) => {
    const dt = document.createElement('dt');
    dt.textContent = k;
    const dd = document.createElement('dd');
    dd.textContent = v;
    rows.append(dt, dd);
  };
  row('State', vs ? vs.word : st.word);
  row('Model', p.model + ' · effort ' + p.effort);
  row('Speaks up', triggerWords(p));
  if (ttsOn) row('In the call', p.speaks ? voiceName(p.voice) + (p.voice_rate && p.voice_rate !== 1 ? ' · rate ' + p.voice_rate : '') : 'Silent');
  const parts = [title, rows];
  if (ttsOn && p.speaks) {
    const box = Object.assign(document.createElement('input'), { type: 'checkbox', checked: muted, id: 'persona-card-mute' });
    box.addEventListener('change', () => setPersonaMuted(p.id, box.checked));
    const l = document.createElement('label');
    l.className = 'check';
    l.append(box, ' Mute for me');
    parts.push(l, Object.assign(document.createElement('p'), { className: 'hint', textContent: 'Only you stop hearing it; the others still do.' }));
  }
  const foot = document.createElement('div');
  foot.className = 'row';
  const ask = Object.assign(document.createElement('button'), { type: 'button', className: 'btn-primary', textContent: 'Ask ' + p.name + ' now' });
  ask.addEventListener('click', () => {
    card.hidePopover();
    askPersona(p.id);
  });
  const edit = Object.assign(document.createElement('button'), { type: 'button', className: 'btn-secondary', textContent: 'Edit' });
  edit.setAttribute('aria-label', 'Edit ' + p.name);
  edit.addEventListener('click', () => {
    card.hidePopover();
    openPersonaSheet(p.id);
  });
  ask.id = 'persona-card-ask';
  edit.id = 'persona-card-edit';
  foot.append(ask, edit);
  parts.push(foot);
  card.replaceChildren(...parts);
  if (focused) $(focused)?.focus();
}

// voiceName says a catalogue voice as the picker does, "Alba (English)".
function voiceName(id) {
  const v = voiceList && voiceList.voices.find((x) => x.id === id);
  if (!v) return id || 'its default voice';
  return v.name + ' (' + langName(v.lang) + ')';
}

const langName = (l) => ({ fr: 'French', en: 'English' }[l] || l);

// loadVoices reads the catalogue once, with the browser's language for a room with no line yet
// (013-AC16), then redraws what shows voices.
async function loadVoices() {
  if (!ttsOn || voiceList) return;
  const lang = (navigator.language || 'en').toLowerCase().startsWith('fr') ? 'fr' : 'en';
  const res = await netFetch(api + '/voices?lang=' + lang);
  if (!res.ok) return;
  voiceList = await res.json();
  staleSheet();
  renderPersonaSheet();
  renderPersonaCard();
}

function renderAgent() {
  hidePairingOnceConnected();
  // The app's page is on this computer only when one of my apps is connected (daemon-r2).
  $('host-app-open').hidden = docsMode || !agent.daemons.some((d) => String(d.participant_id) === myID);
  const bar = $('persona-buttons');
  const kept = new Map([...bar.children].map((b) => [b.dataset.id, b]));
  const buttons = agent.personas.map((p) => {
    let pill = kept.get(String(p.id));
    if (!pill) pill = personaPill(p.id);
    const b = pill.querySelector('button.persona');
    const st = personaState(p);
    const why = whyNot(p, st);
    const voiceSt = personaVoice(p);
    pill.dataset.id = p.id;
    pill.dataset.name = p.name;
    pill.style.setProperty('--persona', p.colour);
    b.setAttribute('aria-label', p.name + (p.watches_notes ? ', watches the notes' : '') + ': ' + (voiceSt ? voiceSt.label : st.label) + '. Open its card');
    markWatch(b.querySelector('.watch'), p.watches_notes);
    b.dataset.name = p.name;
    b.querySelector('.badge').textContent = p.badge;
    b.querySelector('.pname').textContent = p.name;
    const locked = agent.online && !canPress();
    b.querySelector('.pstate').textContent = voiceSt ? voiceSt.word : locked ? 'locked' : st.word;
    const led = b.querySelector('.led');
    led.className = 'led ' + st.cls + (locked ? ' locked' : '');
    led.setAttribute('aria-label', st.label);
    // Speaking or waiting to speak: an icon and a word, never the colour alone (013-AC29).
    const vs = b.querySelector('.vstate');
    vs.hidden = !voiceSt;
    vs.className = 'vstate' + (voiceSt ? ' ' + voiceSt.icon : '');
    vs.replaceChildren(...(voiceSt ? [grillIcon(voiceSt.icon)] : []));
    b.dataset.tip = voiceSt ? voiceSt.label : st.label;
    const run = pill.querySelector('.persona-run');
    run.setAttribute('aria-label', 'Ask ' + p.name + ' now');
    // Not disabled: a press must still reach the reason.
    run.setAttribute('aria-disabled', String(!!why));
    run.dataset.tip = why || 'Ask ' + p.name + ' now';
    const hear = pill.querySelector('.persona-hear');
    const muted = personaMuted.has(String(p.id));
    hear.hidden = !ttsOn || !p.speaks;
    hear.setAttribute('aria-pressed', String(muted));
    hear.setAttribute('aria-label', 'Mute ' + p.name + ' for me');
    hear.dataset.tip = muted ? p.name + ' is muted for you: hear it again' : 'Mute ' + p.name + ' for you';
    hear.replaceChildren(grillIcon(muted ? 'volume-off' : 'volume'));
    return pill;
  });
  if (buttons.length !== bar.children.length || buttons.some((b, i) => bar.children[i] !== b)) bar.replaceChildren(...buttons);
  renderPersonaCard();
  if (ttsOn && !voiceList) loadVoices().catch(console.error);
  const running = (agent.running || []).map((t) => [personaByID(t.persona_id), t]).filter(([p]) => p);
  $('agent-status').textContent = !agent.online ? (agent.reason || 'AI offline')
    : running.length ? running.map(([p, t]) => turnText(p, t)).join(', ') + '…' : agent.personas.length === 0 ? 'No persona: add one.' : '';
  renderRunning();
  renderQueue();
  renderApprovals();
  renderChain();
  renderParallel();
  renderPersonas();
  renderWatcher();
}

// renderWatcher names the notes watcher above Notes, and redraws the notes' chips when a turn they
// queued moves on (009-AC29, 009-AC31).
let noteChipsSeen = '';
function renderWatcher() {
  const w = agent.personas.find((p) => p.watches_notes);
  $('notes-watcher').hidden = !w;
  $('notes-watcher').textContent = w ? w.badge + ' ' + w.name + ' watches the notes' : '';
  const chips = JSON.stringify(agent.note_mentions || []);
  if (chips === noteChipsSeen) return;
  noteChipsSeen = chips;
  renderAnnotations();
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
    .catch((err) => { box.checked = !box.checked; personasStatus(err.message, true); })
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
    .catch((err) => personasStatus(err.message, true));
});

// renderApprovals is the banner of what waits for the host: personas asking above the host cap
// (002-AC19) and persona changes held while the host approves everything (002-AC21). Everyone sees
// it; only the host answers.
function renderApprovals() {
  const items = [];
  const decide = (li, what, path, body) => {
    if (!approves()) {
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
  // A plugin version waiting for the host opens the approval sheet (admin/plugins.js): the host
  // reads what it holds before deciding (009-AC14).
  for (const pl of plugins) {
    const v = pl.versions[pl.versions.length - 1];
    if (!v || v.state !== 'pending') continue;
    const li = document.createElement('li');
    li.textContent = v.added_by + (v.n === 1 ? ' added the plugin ' : ' updated the plugin ') + pl.name + ', ' + pl.ref_name + ' @ ' + v.short_commit + '.';
    if (approves()) {
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = 'Review';
      b.setAttribute('aria-label', 'Review the plugin ' + pl.name + ' v' + v.n);
      b.addEventListener('click', () => dispatchEvent(new CustomEvent('grill:plugin-review', { detail: { id: pl.id, n: v.n } })));
      li.append(' ', b);
    } else {
      li.append(' The AI host decides.');
    }
    items.push(li);
  }
  items.push(...blockedItems());
  $('approvals').hidden = items.length === 0;
  $('approval-list').replaceChildren(...items);
}

// blockedItems are the banner's websites a turn was refused, one per domain, for whoever may
// allow them: the AI host for the room or everywhere, a room admin the host lets for the room. It
// holds them until decided or dismissed, across reloads and host changes (014-AC9).
function blockedItems() {
  const host = approves();
  if (!host && !(meAdmin && agent.egress_admins_allow)) return [];
  const why = agent.egress_host || '';
  return (agent.blocked_requests || []).map((b) => {
    const li = document.createElement('li');
    li.className = 'blocked-request';
    li.textContent = b.persona + ' tried to reach ' + b.domain + (b.count > 1 ? ' (' + b.count + ' times)' : '') + '.';
    if (b.pending) {
      li.append(' Allowing…');
      return li;
    }
    const acts = [['Allow for this room', () => sendJSON('POST', api + '/blocked/' + b.message_id + '/allow', { scope: 'room' })]];
    if (host) acts.push(['Allow everywhere', () => sendJSON('POST', api + '/blocked/' + b.message_id + '/allow', { scope: 'server' })]);
    acts.push(['Dismiss', () => sendJSON('POST', api + '/blocked/' + b.message_id + '/dismiss')]);
    for (const [label, run] of acts) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.textContent = label;
      btn.setAttribute('aria-label', label + ': ' + b.domain);
      btn.disabled = !!why && label !== 'Dismiss';
      btn.addEventListener('click', () => {
        btn.disabled = true;
        run().catch((err) => { btn.disabled = false; agentError(err.message); });
      });
      li.append(' ', btn);
    }
    const note = why || (b.error ? 'Not allowed: ' + b.error : '');
    if (note) li.append(' ', Object.assign(document.createElement('span'), { className: 'hint', textContent: note }));
    return li;
  });
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

// personasStatus says how an action went in both sheets; only a failure reads as an error.
function personasStatus(text, error = false) {
  for (const id of ['personas-status', 'persona-status']) {
    $(id).textContent = text;
    $(id).classList.toggle('error', error);
  }
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

// The persona sheet's tabs, each a deep link #personas/<id>/<tab> (009-AC2).
const personaTabs = [['identity', 'Identity'], ['model', 'Model'], ['triggers', 'Triggers'], ['capabilities', 'Capabilities']];

// personaForm is one form across the four tabs: Save sends every tab at once (009-AC3).
function personaForm(p) {
  const form = document.createElement('form');
  form.className = 'persona-form';
  form.dataset.id = p.id;
  form.dataset.seen = seenKey(p);
  // A hidden tab's field cannot show the browser's bubble: the submit handler checks instead.
  form.noValidate = true;
  form.style.setProperty('--persona', p.colour);
  const tpl = templates.find((t) => t.id === p.template_id);
  const hint = (text) => Object.assign(document.createElement('p'), { className: 'hint', textContent: text });
  const input = (name, value, attrs) => {
    const i = document.createElement('input');
    i.name = name;
    i.value = value;
    Object.assign(i, attrs || {});
    return i;
  };
  const grid = (...fields) => {
    const g = document.createElement('div');
    g.className = 'persona-fields';
    g.append(...fields);
    return g;
  };
  const panel = (tab, ...children) => {
    const s = document.createElement('section');
    s.dataset.tab = tab;
    s.append(...children);
    return s;
  };
  const text = document.createElement('textarea');
  text.name = 'text';
  text.rows = 6;
  text.maxLength = 8000;
  text.value = p.text;
  const identity = panel('identity',
    hint((tpl ? 'From "' + tpl.title + '" v' + p.template_version + '.' : 'Its template was removed from the library.')
      + (p.pending ? ' Waits for the AI host\'s approval.' : '')),
    grid(field('Name', input('name', p.name, { maxLength: 30, required: true })),
      field('Badge', input('badge', p.badge, { maxLength: 8, required: true })),
      field('Colour', input('colour', p.colour, { type: 'color' }))),
    field('Persona text', text), ...(ttsOn ? [voiceBlock(p, input)] : []));
  const use = contextUse(p);
  const model = panel('model',
    grid(field('Model', picker('model', agent.models, p.model)), field('Effort', picker('effort', agent.efforts, p.effort)),
      field('Memory size (k tokens)', input('autocompact', p.autocompact ? p.autocompact / 1000 : '', { type: 'number', min: 100, max: 1000,
        placeholder: 'auto' }))),
    hint('Memory size is how much it remembers before it summarizes its session; empty for automatic. '
      + (use === null ? 'Context use: known after its next answer.' : 'Context use: its session holds ' + use + '% of its context window.')));
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
  const self = input('self_call', '', { type: 'checkbox', checked: p.self_call });
  const selfCall = field('', self);
  selfCall.className = 'check';
  selfCall.append('Can follow up on its own: its own @' + p.name + ' gives it another answer');
  const watches = input('watches_notes', '', { type: 'checkbox', checked: !!p.watches_notes, defaultChecked: !!p.watches_notes });
  const watch = field('', watches);
  watch.className = 'check';
  watch.append('Watches the notes');
  const next = nextTrigger(p).trim();
  const triggers = panel('triggers', grid(field('Speaks up', speaks), everyField), ...(next ? [hint(next)] : []), selfCall,
    watch, hint('Answers note threads that name no persona, ' + agent.notes_watch_delay + ' s after the last new message.'),
    hint('An answer that names another persona makes it answer next. These auto turns pause at the room\'s limit of '
      + agent.chain.budget + ', set in Personas.'));
  const capabilities = panel('capabilities', skillsBlock(p), pluginsBlock(p));
  // One footer for the four tabs: the changes it would save, counted across them, and Save (004b-AC10).
  const actions = document.createElement('div');
  actions.className = 'settings-foot';
  const dirty = span('dirty', '');
  const save = document.createElement('button');
  save.type = 'submit';
  save.className = 'btn-primary';
  save.textContent = 'Save';
  actions.append(dirty, save);
  form.append(identity, model, triggers, capabilities, actions);
  // The capabilities' switches apply at once and carry no name: only named fields wait for Save.
  const fields = [...form.elements].filter((el) => el.name);
  const valueOf = (el) => (el.type === 'checkbox' || el.type === 'radio' ? el.checked : el.value);
  let initial = fields.map(valueOf);
  const mark = () => {
    const n = fields.filter((el, i) => valueOf(el) !== initial[i]).length;
    if (n) form.dataset.dirty = '1';
    else delete form.dataset.dirty;
    dirty.textContent = n ? n + ' unsaved change' + (n > 1 ? 's' : '') : 'No unsaved changes';
    save.disabled = !n;
  };
  mark();
  form.addEventListener('input', mark);
  form.addEventListener('change', mark);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    // An invalid field on another tab: show that tab, then the field's error.
    const bad = form.querySelector('input:invalid, select:invalid, textarea:invalid');
    if (bad) {
      const tab = bad.closest('[data-tab]').dataset.tab;
      history.replaceState(null, '', '#personas/' + p.id + '/' + tab);
      showPersonaTab(tab);
      bad.reportValidity();
      return;
    }
    sendJSON('PATCH', api + '/personas/' + p.id, formFields(form)).then((saved) => {
      if (!saved) return;
      initial = fields.map(valueOf);
      mark();
      personasStatus(saved.held ? saved.name + ': the change waits for the AI host\'s approval.' : saved.name + ' saved. It applies from its next turn.');
    }).catch((err) => personasStatus(err.message, true));
  });
  return form;
}

// fillPersonaMenu builds the sheet's ⋯ for the persona as it is when it opens (009-AC3): the
// template update, the library, a fresh start for the host, then Remove.
function fillPersonaMenu() {
  const form = $('persona-body').firstElementChild;
  const p = form && personaByID(Number(form.dataset.id));
  const items = [];
  const act = (label, aria, fn, cls) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = label;
    b.setAttribute('aria-label', aria);
    if (cls) b.className = cls;
    b.addEventListener('click', () => fn().catch((err) => personasStatus(err.message, true)));
    items.push(b);
  };
  if (p) {
    const tpl = templates.find((t) => t.id === p.template_id);
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
    act('Remove', 'Remove ' + p.name, async () => {
      if (!(await grillConfirm('Remove ' + p.name + '?', 'Its session goes; what it wrote stays.', 'Remove'))) return;
      const res = await netFetch(api + '/personas/' + p.id, { method: 'DELETE' });
      if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || 'HTTP ' + res.status);
    }, 'danger');
  }
  $('persona-menu').replaceChildren(...items);
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
  if (!own.length) $('room-skill-list').append(Object.assign(document.createElement('li'), { className: 'empty', textContent: 'None yet.' }));
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
  }).catch((err) => personasStatus(err.message, true)).finally(() => loadSkills().catch(console.error));
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
    b.addEventListener('click', () => fn().catch((err) => personasStatus(err.message, true)));
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
  if (!list.children.length) list.append(Object.assign(document.createElement('li'), { className: 'empty', textContent: 'None on.' }));
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
  staleSheet();
  // An upload to the library does not wake the room: read the update offers afresh.
  if (st.ok) {
    agent = await st.json();
    renderAgent();
  } else {
    renderPersonas();
  }
}

// loadPlugins reads the room's plugins (009-AC5); Room settings › Plugins and the approval sheet
// (admin/plugins.js) get them as window.grillPlugins.
async function loadPlugins() {
  const res = await netFetch(api + '/plugins');
  if (!res.ok) return;
  plugins = (await res.json()).plugins;
  pluginsSeen++;
  window.grillPlugins = plugins;
  dispatchEvent(new CustomEvent('grill:plugins', { detail: plugins }));
  staleSheet();
  renderApprovals();
  renderPersonaSheet();
}

// pluginCost is a version's token cost per answer from its inspection, null while unknown.
function pluginCost(v) {
  const d = v && v.report && v.report.details;
  return d && d.cost_known ? d.always_on_tokens : null;
}
const tokens = (n) => '~' + n.toLocaleString('en') + ' tokens';

// pluginsBlock lists the room's plugins on the Capabilities tab (009-AC16, 009-AC18): on or off for
// the persona, the version it loads, why one does not load, the token cost per answer, and the
// total of those it loads. Only a room admin turns one on or off (009-AC10).
function pluginsBlock(p) {
  const box = document.createElement('fieldset');
  box.className = 'persona-skills persona-plugins';
  const legend = document.createElement('legend');
  legend.textContent = 'Plugins';
  const list = document.createElement('ul');
  let total = 0;
  let unknown = 0;
  // Counted apart from the cost: a plugin that loads may cost 0 tokens.
  let loaded = 0;
  for (const pl of plugins) {
    const on = pl.personas.includes(p.id);
    const run = pl.versions.find((v) => v.n === pl.running);
    const shown = run || pl.versions[pl.versions.length - 1];
    const cost = pluginCost(shown);
    // A plugin that needs host setup does not load (009-AC24).
    if (on && run && !pl.needs_setup) {
      loaded++;
      if (cost === null) unknown++;
      else total += cost;
    }
    const li = document.createElement('li');
    li.dataset.plugin = pl.name;
    const name = document.createElement('label');
    name.className = 'check';
    const toggle = Object.assign(document.createElement('input'), { type: 'checkbox', checked: on, disabled: !meAdmin });
    toggle.setAttribute('aria-label', pl.name + ' for ' + p.name);
    // Turning a plugin on is not an edit of the persona's fields: the form keeps refreshing.
    for (const ev of ['input', 'change']) toggle.addEventListener(ev, (e) => e.stopPropagation());
    // Not disabled while it saves: a disabled box loses the focus. A second press is idempotent.
    toggle.addEventListener('change', () => {
      const want = toggle.checked;
      sendJSON(want ? 'PUT' : 'DELETE', api + '/personas/' + p.id + '/plugins/' + pl.id)
        .then((done) => { if (done) personasStatus(pl.name + (want ? ' is on for ' : ' is off for ') + p.name + ' from its next answer.'); })
        .catch((err) => { toggle.checked = !want; personasStatus(err.message, true); })
        .finally(() => loadPlugins().catch(console.error));
    });
    name.append(toggle, ' ' + pl.name);
    const meta = [(shown.manifest && shown.manifest.version ? shown.manifest.version + ', ' : '') + pl.ref_name + ' @ ' + shown.short_commit];
    meta.push(run && !pl.needs_setup ? (cost === null ? 'cost unknown' : tokens(cost) + ' per answer') : pl.why || 'does not load');
    if (on && run && run !== pl.versions[pl.versions.length - 1]) meta.push('a newer version waits');
    li.append(name, span('hint', meta.join(' · ')));
    list.append(li);
  }
  box.append(legend);
  if (!plugins.length) {
    box.append(Object.assign(document.createElement('p'), { className: 'empty', textContent: 'No plugins in this room yet. A room admin adds them in Room settings › Plugins.' }));
    return box;
  }
  box.append(list);
  const sum = !loaded ? 'None of them loads for ' + p.name + '.'
    : 'Total: ' + tokens(total) + ' per answer' + (unknown ? ', plus ' + unknown + ' with an unknown cost' : '') + '.';
  box.append(Object.assign(document.createElement('p'), { className: 'plugin-total', textContent: sum }));
  if (!meAdmin) box.append(Object.assign(document.createElement('p'), { className: 'hint', textContent: 'Only room admins turn plugins on or off.' }));
  return box;
}

// voiceBlock is the persona in the call (013-AC26, AC27, AC37): whether it speaks, its voice, each
// with a sample and its credit, and its rate.
function voiceBlock(p, input) {
  const box = document.createElement('fieldset');
  box.className = 'persona-voice';
  const legend = document.createElement('legend');
  legend.textContent = 'In the call';
  const speaks = input('speaks', '', { type: 'checkbox', checked: !!p.speaks });
  const sl = document.createElement('label');
  sl.className = 'check';
  sl.append(speaks, ' Speaks in the call');
  box.append(legend, sl, Object.assign(document.createElement('p'), { className: 'hint',
    textContent: 'After its turns it says a short line while someone is in the call. Each listener can mute it for themselves.' }));
  if (!voiceList) {
    box.append(Object.assign(document.createElement('p'), { className: 'hint', textContent: 'Loading the voices…' }));
    loadVoices().catch(console.error);
  } else {
    box.append(voicePicker(p));
  }
  box.append(field('Speaking rate', input('voice_rate', String(p.voice_rate || 1), { type: 'number', min: 0.9, max: 1.3, step: 0.05, required: true })));
  return box;
}

// voicePicker lists the catalogue: name, native language, a sample in the room's language and the
// credit, on hover, focus or a tap. A voice another persona has is marked, and may be picked anyway.
function voicePicker(p) {
  const list = document.createElement('div');
  list.className = 'voice-list';
  list.setAttribute('role', 'radiogroup');
  list.setAttribute('aria-label', 'Voice of ' + p.name);
  for (const v of voiceList.voices) {
    const row = document.createElement('div');
    row.className = 'voice-opt';
    const radio = Object.assign(document.createElement('input'), { type: 'radio', name: 'voice', value: v.id, checked: p.voice === v.id, defaultChecked: p.voice === v.id });
    const other = agent.personas.find((o) => o.id !== p.id && o.voice === v.id);
    const l = document.createElement('label');
    l.append(radio, ' ' + v.name, span('hint', ' ' + langName(v.lang) + (other ? ' · ' + other.name + ' has it' : '')));
    const lang = v.samples.includes(voiceList.lang) ? voiceList.lang : v.samples[0];
    const play = Object.assign(document.createElement('button'), { type: 'button', className: 'btn-icon voice-play', disabled: !lang });
    play.setAttribute('aria-label', 'Play a sample of ' + v.name);
    play.dataset.tip = lang ? 'Play a sample of ' + v.name : 'No sample yet';
    play.append(grillIcon('play'));
    play.addEventListener('click', () => {
      const a = new Audio(api + '/voices/' + encodeURIComponent(v.id) + '/' + lang);
      a.play().catch((err) => personasStatus('Could not play the sample: ' + err.message, true));
    });
    const credit = Object.assign(document.createElement('button'), { type: 'button', className: 'btn-icon voice-credit' });
    credit.setAttribute('aria-label', 'Credit for ' + v.name);
    credit.dataset.tip = v.credit + '.';
    credit.append(grillIcon('info'));
    // Touch has no hover: a tap shows the credit.
    credit.addEventListener('click', () => grillTip.show(credit));
    row.append(l, play, credit);
    list.append(row);
  }
  return list;
}

// formFields reads the inputs' own values: FormData would turn the text's line breaks into CRLF.
function formFields(form) {
  const v = (name) => form.elements.namedItem(name).value;
  return { name: v('name'), badge: v('badge'), colour: v('colour'), model: v('model'), effort: v('effort'), text: v('text'),
    ...triggerFields(v('trigger'), Number(v('every'))), self_call: form.elements.namedItem('self_call').checked,
    autocompact: v('autocompact') ? Math.round(Number(v('autocompact')) * 1000) : 0, ...watchField(form), ...voiceFields(form) };
}

// voiceFields is the persona in the call, sent only when the sheet shows it (013-AC26, AC27, AC37).
function voiceFields(form) {
  const speaks = form.elements.namedItem('speaks');
  if (!speaks) return {};
  const out = { speaks: speaks.checked, voice_rate: Number(form.elements.namedItem('voice_rate').value) };
  const picked = form.querySelector('input[name=voice]:checked');
  if (picked) out.voice = picked.value;
  return out;
}

// watchField sends the notes watcher role only when the form changed it: a sheet drawn before
// someone moved the role must not take it back (009-AC31).
function watchField(form) {
  const w = /** @type {HTMLInputElement} */ (form.elements.namedItem('watches_notes'));
  return w.checked === w.defaultChecked ? {} : { watches_notes: w.checked };
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

// seenKey is what a form shows: the persona, the room's auto-turn limit (Triggers), its plugins and
// whether this participant may turn them on (Capabilities).
const seenKey = (p) => JSON.stringify([p, agent.chain.budget, pluginsSeen, meAdmin]);

// staleSheet redraws the open persona form at the next render, unless someone is editing it.
function staleSheet() {
  const f = $('persona-body').firstElementChild;
  if (f && !f.dataset.dirty) delete f.dataset.seen;
}

// triggerWords is a persona's automatic trigger in words (009-AC1, 006-AC18).
function triggerWords(p) {
  const every = (n, one, many) => 'every ' + (n === 1 ? one : n + ' ' + many);
  if (p.every_n) return every(p.every_n, 'answer', 'answers');
  if (p.every_voice) return every(p.every_voice, 'voice line', 'voice lines');
  if (p.every_minutes) return every(p.every_minutes, 'minute', 'minutes');
  return 'on mention only';
}

// watchIcon marks the notes watcher on its pill and its row; the label sits on the pill or row (009-AC31).
function watchIcon() {
  const el = span('watch', '', 'Watches the notes');
  el.setAttribute('aria-hidden', 'true');
  el.hidden = true;
  return el;
}

// markWatch shows el, a watchIcon, for the notes watcher. Empty when hidden: a row's text is read whole.
function markWatch(el, on) {
  el.hidden = !on;
  el.replaceChildren(...(on ? [grillIcon('eye')] : []));
}

// rowState is the list's word for a persona's state (009-AC1).
const rowState = (p) => ({ pending: 'waiting for approval', busy: 'working', queued: 'queued', on: 'ready' }[personaState(p).cls] || 'offline');

// renderPersonas refreshes the open sheets: the list of one-line rows (009-AC1), updated in place
// so a focused row keeps its focus, and the open persona's sheet.
function renderPersonas() {
  renderPersonaSheet();
  if (!$('personas').open) return;
  renderRoomSkills();
  const list = $('persona-list');
  const kept = new Map([...list.children].map((li) => [li.dataset.id, li]));
  const rows = agent.personas.map((p) => {
    let li = kept.get(String(p.id));
    if (!li) {
      li = document.createElement('li');
      li.dataset.id = p.id;
      const a = document.createElement('a');
      a.className = 'persona-row';
      a.href = '#personas/' + p.id + '/identity';
      const led = span('led', '');
      led.setAttribute('aria-hidden', 'true');
      a.append(span('badge', ''), span('pr-name', ''), watchIcon(), span('pr-meta', ''), led, span('pr-state', ''));
      li.append(a);
    }
    const a = li.firstElementChild;
    const meta = level(p) + ' · ' + triggerWords(p);
    const state = rowState(p);
    a.style.setProperty('--persona', p.colour);
    a.querySelector('.badge').textContent = p.badge;
    a.querySelector('.pr-name').textContent = p.name;
    // A phone drops the word "effort" and says the state by the pill's shape: the trigger stays whole.
    a.querySelector('.pr-meta').replaceChildren(p.model + ' · ', span('pr-long', 'effort '), p.effort + ' · ' + triggerWords(p));
    a.querySelector('.led').className = 'led ' + personaState(p).cls;
    a.querySelector('.pr-state').textContent = state;
    markWatch(a.querySelector('.watch'), p.watches_notes);
    a.setAttribute('aria-label', p.name + ': ' + meta + (p.watches_notes ? ', watches the notes' : '') + ', ' + state);
    return li;
  });
  if (rows.length !== list.children.length || rows.some((li, i) => list.children[i] !== li)) list.replaceChildren(...rows);
}

// sheetRoute reads #personas/<id>/<tab>; null on any other hash. An unknown tab is Identity.
function sheetRoute() {
  const m = location.hash.match(/^#personas\/(\d+)(?:\/([a-z]+))?$/);
  if (!m) return null;
  return { id: Number(m[1]), tab: personaTabs.some(([t]) => t === m[2]) ? m[2] : 'identity' };
}

// showPersonaTab shows one tab of the open form and marks it in the bar.
function showPersonaTab(tab) {
  for (const s of $('persona-body').querySelectorAll('[data-tab]')) s.hidden = s.dataset.tab !== tab;
  for (const a of $('persona-tabs').children) {
    if (a.id === 'persona-tab-' + tab) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  // A bar that scrolls sideways on a phone keeps its current tab in sight (005-AC35).
  window.grillShowTab($('persona-tabs'));
}

// agentSeen: the room's personas are known, so a deep link to a missing one closes its sheet.
let agentSeen = false;

// renderPersonaSheet draws the persona the hash names; a form being edited keeps what its user typed.
function renderPersonaSheet() {
  const d = $('persona-sheet');
  const r = sheetRoute();
  if (!d.open || !r) return;
  const p = personaByID(r.id);
  if (!p) {
    if (agentSeen) d.close();
    return;
  }
  $('persona-sheet-title').replaceChildren(span('badge persona-badge', p.badge), ' ' + p.name);
  const nav = $('persona-tabs');
  if (nav.dataset.id !== String(p.id)) {
    nav.dataset.id = p.id;
    nav.replaceChildren(...personaTabs.map(([t, label]) => {
      const a = Object.assign(document.createElement('a'), { id: 'persona-tab-' + t, href: '#personas/' + p.id + '/' + t, textContent: label });
      // A tab replaces the sheet's history entry: Back after closing never reopens it (009-AC2).
      a.addEventListener('click', (e) => {
        if (e.ctrlKey || e.metaKey || e.shiftKey || e.button) return;
        e.preventDefault();
        location.replace(a.href);
        renderPersonaSheet();
      });
      return a;
    }));
  }
  const body = $('persona-body');
  const f = body.firstElementChild;
  if (!f || f.dataset.id !== String(p.id) || (!f.dataset.dirty && f.dataset.seen !== seenKey(p))) {
    // A redraw keeps the focus on the control that had it: a plugin's toggle redraws the form.
    const label = f && f.contains(document.activeElement) ? document.activeElement.getAttribute('aria-label') : null;
    body.replaceChildren(personaForm(p));
    if (label) [...body.querySelectorAll('[aria-label]')].find((el) => el.getAttribute('aria-label') === label)?.focus();
  }
  showPersonaTab(r.tab);
}

// sheetReturn is the Room settings link the sheet was opened from, to go back to on close.
let sheetReturn = '';

// followPersona opens, moves or closes the persona sheet as the hash says (009-AC2). Room settings
// follows #manage on its own (admin/room-admin.js).
function followPersona(e) {
  const d = $('persona-sheet');
  if (!sheetRoute()) {
    if (d.open) d.close();
    return;
  }
  if (!d.open) {
    const from = e instanceof HashChangeEvent ? new URL(e.oldURL).hash : '';
    sheetReturn = /^#manage(\/|$)/.test(from) ? from : '';
    personasStatus('');
    $('persona-body').replaceChildren();
    d.showModal();
    loadTemplates().catch(console.error);
    loadSkills().catch(console.error);
  }
  renderPersonaSheet();
}
addEventListener('hashchange', followPersona);
// Closed by Escape, a click outside or ✕: the deep link goes too, without a history entry.
$('persona-sheet').addEventListener('close', () => {
  if ($('persona-menu').matches(':popover-open')) $('persona-menu').hidePopover();
  // Opened from Room settings › Personas, closing goes back there.
  if (sheetRoute()) {
    if (sheetReturn) location.replace(sheetReturn);
    else history.replaceState(null, '', location.pathname + location.search);
  }
  sheetReturn = '';
});
$('persona-sheet-close').addEventListener('click', () => $('persona-sheet').close());
$('persona-menu').addEventListener('beforetoggle', (e) => { if (e.newState === 'open') fillPersonaMenu(); });
actionMenu($('persona-menu'), $('persona-more'));

// openPersonaSheet opens a persona's sheet on its Identity tab (009-AC4).
function openPersonaSheet(id) {
  location.hash = '#personas/' + id + '/identity';
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
  staleSheet();
  renderPersonas();
}

// openPersonas opens the Personas sheet (005-AC40): the list, the room's limits and skills.
function openPersonas() {
  personasStatus('');
  if (!$('personas').open) $('personas').showModal();
  renderPersonas();
  loadTemplates().catch(console.error);
  loadSkills().catch(console.error);
}
$('personas-open').addEventListener('click', () => openPersonas());
$('personas-close').addEventListener('click', () => $('personas').close());
$('persona-add').addEventListener('submit', (e) => {
  e.preventDefault();
  sendJSON('POST', api + '/personas', { template_id: Number($('persona-template').value) })
    .then((p) => { if (p) personasStatus(p.name + (p.pending ? ' added. It waits for the AI host\'s approval.' : ' added.')); })
    .catch((err) => personasStatus(err.message, true));
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

// badgeIcons are the People badges' shapes, drawn as block SVGs (009-AC39): an emoji's ink sits
// wherever its font puts it, off the name's centre line.
const badgeIcons = {
  anon: '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
  host: '<path d="M12.2 2h-.4a2 2 0 0 0-2 2v.2a2 2 0 0 1-1 1.7l-.4.3a2 2 0 0 1-2 0l-.2-.1a2 2 0 0 0-2.7.7l-.2.4a2 2 0 0 0 .7 2.7l.2.1a2 2 0 0 1 1 1.7v.5a2 2 0 0 1-1 1.7l-.2.1a2 2 0 0 0-.7 2.7l.2.4a2 2 0 0 0 2.7.7l.2-.1a2 2 0 0 1 2 0l.4.3a2 2 0 0 1 1 1.7v.2a2 2 0 0 0 2 2h.4a2 2 0 0 0 2-2v-.2a2 2 0 0 1 1-1.7l.4-.3a2 2 0 0 1 2 0l.2.1a2 2 0 0 0 2.7-.7l.2-.4a2 2 0 0 0-.7-2.7l-.2-.1a2 2 0 0 1-1-1.7v-.5a2 2 0 0 1 1-1.7l.2-.1a2 2 0 0 0 .7-2.7l-.2-.4a2 2 0 0 0-2.7-.7l-.2.1a2 2 0 0 1-2 0l-.4-.3a2 2 0 0 1-1-1.7V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>',
  transcribed: '<path d="M12 20h9"/><path d="M16.4 3.6a2.1 2.1 0 1 1 3 3L7 19l-4 1 1-4z"/>',
  'transcribed-device': '<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4M7 10h5"/>',
  mic: '<rect x="9" y="2" width="6" height="12" rx="3"/><path d="M19 10v1a7 7 0 0 1-14 0v-1M12 18v4"/>',
  muted: '<path d="M11 5 6 9H2v6h4l5 4z"/><path d="m22 9-6 6M16 9l6 6"/>',
  listening: '<path d="M3 14h3a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-7a9 9 0 0 1 18 0v7a2 2 0 0 1-2 2h-1a2 2 0 0 1-2-2v-3a2 2 0 0 1 2-2h3"/>',
  deafened: '<path d="M8.7 3A6 6 0 0 1 18 8a21.3 21.3 0 0 0 .6 5M17 17H3s3-2 3-9a4.7 4.7 0 0 1 .3-1.7M10.3 21a1.9 1.9 0 0 0 3.4 0M2 2l20 20"/>',
};

function badgeIcon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  for (const [k, v] of [['viewBox', '0 0 24 24'], ['fill', 'none'], ['stroke', 'currentColor'], ['stroke-width', '2'],
    ['stroke-linecap', 'round'], ['stroke-linejoin', 'round'], ['aria-hidden', 'true']]) svg.setAttribute(k, v);
  svg.innerHTML = badgeIcons[name];
  return svg;
}

// audioState is how a participant is in the audio: from the server's presence, which every
// viewer gets whether or not they joined the audio, with the LiveKit state for those they hear.
function audioState(id) {
  const pr = presence.get(id);
  const vp = voiceParticipant(id);
  const audio = !!(pr && pr.audio) || !!vp;
  if (!audio) return { key: 'none', icon: '', text: 'Not in the audio' };
  const mic = vp ? vp.isMicrophoneEnabled : !!(pr && pr.mic);
  const muted = !mic && (vp ? !!vp.getTrackPublication(LivekitClient.Track.Source.Microphone) : !!(pr && pr.muted));
  if (mic) return { key: 'live', icon: 'mic', text: 'Mic live' };
  if (muted) return { key: 'muted', icon: 'muted', text: 'Muted' };
  return { key: 'listening', icon: 'listening', text: 'Listening only' };
}


function span(cls, text, tip) {
  const el = document.createElement('span');
  el.className = cls;
  el.textContent = text;
  if (tip) el.dataset.tip = tip;
  return el;
}

// tagColumns are the badges a person row may carry, in their fixed order (008-AC13): a column shows
// in every row of a list once any row has it, so a badge sits at the same place on every row. You
// and room admins are marked on the name, can ask the AI in the card (008-AC18).
const tagColumns = ['anon', 'host', 'transcribed', 'audio'];

// personTags are a participant's badges by column: [icon, accessible name, tooltip].
function personTags(p, id, isHere) {
  const t = {};
  if (agent.host_participant_id === p.id) t.host = ['host', 'host', 'AI host: the personas run on their computer'];
  // Only your own row: nobody else learns whether a profile has an email (011-AC6).
  if (id === myID && noEmail()) t.anon = ['anon', anonText, anonText];
  // Their own browser writes their voice down instead of the server (010-AC12).
  if (presence.get(id)?.device_stt) t.transcribed = ['transcribed-device', 'Their device transcribes this voice', 'Their device transcribes this voice'];
  else if (transcribed.has(id)) t.transcribed = ['transcribed', 'The transcription hears this voice', 'The transcription hears this voice'];
  const st = isHere ? audioState(id) : { key: 'none', icon: '', text: 'Not in the audio' };
  const deaf = isHere && !!presence.get(id)?.deaf;
  // One icon per square: deafened shows its own alone, its audio state in the tooltip.
  t.audio = [deaf ? 'deafened' : st.icon, '', st.text + (deaf ? ', deafened: hears nobody' : ''), st.key, deaf];
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
  // The host hands over; a room admin moves the role from someone else, asked first (009-AC33).
  if (!host && (isHost() || meAdmin) && agent.daemons.some((d) => d.participant_id === p.id && d.can_host)) {
    acts.push({ label: 'Make AI host', run: () => makeHost(p) });
  }
  if (other && voiceParticipant(id)) acts.push({ volume: true });
  return acts;
}

async function makeHost(p) {
  const from = participants.find((x) => x.id === agent.host_participant_id);
  const standby = from && agent.daemons.some((d) => d.participant_id === from.id);
  if (!isHost() && !(await grillConfirm('Make ' + p.name + ' the AI host?', 'The personas run on ' + p.name + "'s computer from the next turn"
    + (standby ? '; ' + from.name + "'s app stays connected as a standby." : '.'), 'Make AI host'))) return;
  sendJSON('PUT', api + '/host', { participant_id: p.id }).catch((err) => agentError(err.message));
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
  // Right-aligned on its ⋯, kept 8 px inside the left edge too: a row near it pushed it to 0.
  menu.style.right = Math.max(8, Math.min(innerWidth - r.right, innerWidth - menu.offsetWidth - 8)) + 'px';
  b.setAttribute('aria-expanded', 'true');
  menu.querySelector('button, input')?.focus();
}
$('person-menu').addEventListener('toggle', (e) => {
  if (e.newState === 'closed' && personFor) personFor.b.setAttribute('aria-expanded', 'false');
  if (e.newState === 'open') hideCard();
});

// The details card (009-AC36): hovering or focusing a person in People or their name on a chat
// line shows it; on touch a tap does. A non-modal tooltip tied by aria-describedby. The People
// badges get the same styled tip for their meaning: no native title tooltip there.
// personCardLines are its lines under the name, in order, each '' to skip.
const personCardLines = [
  (p, id, el) => (el.dataset.writtenAs ? 'Written as ' + el.dataset.writtenAs : ''),
  (p) => (p.previous_name ? 'Earlier name: ' + p.previous_name : ''),
  (p, id) => 'Roles: ' + personRoles(p, id),
  (p) => (agent.can_trigger.includes(p.id) || agent.host_participant_id === p.id ? 'Can ask the AI' : ''),
  (p, id) => 'Audio: ' + cardAudio(id),
  (p, id) => (presence.get(id)?.device_stt ? 'Transcribed on their device' : transcribed.has(id) ? 'Transcribed by the server' : 'Not transcribed'),
  (p, id) => cardHere(p, id),
  (p) => 'Joined ' + grillDate(p.joined_at),
  (p, id) => (id === myID && noEmail() ? anonLink() : ''),
];

function anonLink() {
  const a = document.createElement('a');
  a.href = '/profile#email';
  a.textContent = anonText;
  return a;
}
// hoverDelay keeps a pointer passing over the list from flashing cards.
const hoverDelay = 300;

function personRoles(p, id) {
  const r = [];
  if (id === myID) r.push('you');
  if (p.admin) r.push('room admin');
  if (agent.host_participant_id === p.id) r.push('AI host');
  else if (agent.daemons.some((d) => d.participant_id === p.id)) r.push('standby host');
  return r.join(', ') || 'member';
}

function cardAudio(id) {
  if (id !== myID && !presence.get(id)?.here) return 'not in the audio';
  if (presence.get(id)?.deaf) return 'room muted';
  return audioState(id).text.toLowerCase();
}

function cardHere(p, id) {
  const pr = presence.get(id);
  if (id === myID || pr?.here) return pr?.here_since ? 'Here since ' + grillTime(pr.here_since) : 'Here';
  return 'Seen ' + grillAgo(pr?.seen_at || p.last_seen_at || p.joined_at, true);
}

function showCard(el) {
  const id = el.closest('li[data-pid]')?.dataset.pid;
  const p = participants.find((x) => String(x.id) === id);
  if (!p || $('person-menu').matches(':popover-open')) return;
  grillTip.hide();
  const card = $('person-card');
  const list = document.createElement('ul');
  for (const line of personCardLines) {
    const text = line(p, id, el);
    if (!text) continue;
    const li = document.createElement('li');
    li.append(text);
    list.append(li);
  }
  const name = document.createElement('strong');
  name.textContent = p.name;
  card.replaceChildren(name, list);
  if (cardFor !== el) hideCard();
  // A close pending for what was open before must not close this one.
  clearTimeout(hideTimer);
  cardFor = el;
  el.setAttribute('aria-describedby', 'person-card');
  if (!card.matches(':popover-open')) card.showPopover();
  grillPlace(card, el);
}

function hideCard() {
  if (!cardFor) return;
  cardFor.removeAttribute('aria-describedby');
  cardFor = null;
  $('person-card').hidePopover();
}

// hideGrace lets the pointer cross the gap between a name and its card.
const hideGrace = 150;
function hideSoon(from) {
  clearTimeout(hideTimer);
  hideTimer = setTimeout(() => {
    if (cardFor === from || from === $('person-card')) hideCard();
  }, hideGrace);
}

const cardTarget = (t) => (t instanceof Element ? t.closest('#participants .name, #participants-away .name, #messages li[data-pid] > .meta > .sender') : null);
document.addEventListener('pointerover', (e) => {
  if (e.pointerType !== 'mouse' || !(e.target instanceof Element)) return;
  const el = cardTarget(e.target);
  // Reaching the open card, or coming back to its anchor, keeps it.
  if (e.target.closest('#person-card') || (el && el === cardFor)) {
    clearTimeout(hideTimer);
    return;
  }
  if (!el) return;
  clearTimeout(hoverTimer);
  const [x, y] = [e.clientX, e.clientY];
  hoverTimer = setTimeout(() => {
    // A re-render may have replaced what the pointer rests on: take what is under it now.
    const under = document.elementFromPoint(x, y);
    const card = cardTarget(under);
    if (card) showCard(card);
  }, hoverDelay);
});
// The pointer may move onto the card or tip and stay there to read it (WCAG 1.4.13).
document.addEventListener('pointerout', (e) => {
  if (e.pointerType !== 'mouse') return;
  const from = cardTarget(e.target);
  if (!from || from.contains(e.relatedTarget)) return;
  clearTimeout(hoverTimer);
  if (from === cardFor) hideSoon(from);
});
$('person-card').addEventListener('pointerleave', (e) => { if (e.pointerType === 'mouse') hideSoon($('person-card')); });
document.addEventListener('focusin', (e) => { if (cardTarget(e.target)) showCard(e.target); });
document.addEventListener('focusout', (e) => { if (e.target === cardFor) hideCard(); });
// The marker (011-AC7): on touch the first tap shows its tip, the next one opens the email form.
document.addEventListener('click', (e) => {
  const m = e.target instanceof Element && e.target.closest('.anon-mark');
  if (!m || (e.pointerType !== 'touch' && e.pointerType !== 'pen') || grillTip.shownFor() === m) return;
  e.preventDefault();
  grillTip.show(m);
});
// A tap anywhere on a person's row but its ⋯, or on a chat name, opens the card; a tap elsewhere closes it.
document.addEventListener('click', (e) => {
  const row = e.target instanceof Element && !e.target.closest('.person-more, .anon-mark') ? e.target.closest('#participants li, #participants-away li') : null;
  const el = row ? row.querySelector('.name') : cardTarget(e.target);
  if (el) showCard(el);
  else if (!$('person-card').contains(e.target)) hideCard();
});
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  clearTimeout(hoverTimer);
  hideCard();
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
      more.append(grillIcon('more'));
      more.setAttribute('aria-expanded', 'false');
      const name = span('name', '');
      name.tabIndex = 0;
      const sigil = span('sigil', '');
      sigil.setAttribute('aria-hidden', 'true');
      li.append(span('voice-dot', ''), sigil, name, span('tags', ''), more);
    }
    const isHere = id === myID || !!presence.get(id)?.here;
    const name = li.querySelector('.name');
    name.textContent = p.name;
    // IRC's @ for a room admin, in its own column so every name starts at the same x (008-AC18).
    const sigil = li.querySelector('.sigil');
    sigil.textContent = p.admin ? '@' : '';
    sigil.dataset.tip = p.admin ? 'Room admin' : '';
    name.classList.toggle('me', id === myID);
    const tags = personTags(p, id, isHere);
    const [, , , key, deaf] = tags.audio;
    li.classList.toggle('in-voice', key !== 'none');
    li.classList.toggle('speaking', key === 'live' && speaking.has(id));
    li.classList.toggle('muted', key === 'muted');
    li.classList.toggle('deaf', deaf);
    li.querySelector('.voice-dot').dataset.tip = key === 'none' ? '' : key === 'live' && speaking.has(id) ? 'Speaking' : tags.audio[2];
    const more = li.querySelector('.person-more');
    more.setAttribute('aria-label', 'Actions for ' + p.name);
    more.onpointerdown = () => { openFor = $('person-menu').matches(':popover-open') ? personFor?.b : null; };
    more.onclick = (e) => openPersonMenu(more, p, id, e.detail > 0);
    rows[isHere ? 'here' : 'away'].push({ li, tags, menu: personActions(p, id).length > 0 });
  }
  for (const [list, want] of [[here, rows.here], [away, rows.away]]) {
    const cols = tagColumns.filter((c) => c === 'audio' || want.some((r) => r.tags[c] && r.tags[c][0]));
    const anyMenu = want.some((r) => r.menu);
    for (const { li, tags, menu } of want) {
      const box = li.querySelector('.tags');
      const had = new Map([...box.children].map((x) => [x.dataset.col, x]));
      const kids = cols.map((c) => tagSpan(c, tags[c], had.get(c) || (c === 'anon' && tags.anon ? anonMark() : undefined)));
      if (kids.length !== box.children.length || kids.some((k, i) => box.children[i] !== k)) box.replaceChildren(...kids);
      const more = li.querySelector('.person-more');
      more.hidden = !anyMenu;
      more.classList.toggle('blank', !menu);
      more.disabled = !menu;
    }
    const lis = want.map((r) => r.li);
    if (lis.length !== list.children.length || lis.some((li, i) => list.children[i] !== li)) list.replaceChildren(...lis);
  }
  // An open card or tip follows the agent and presence changes that re-render the list.
  if (cardFor) {
    if (cardFor.isConnected) showCard(cardFor);
    else hideCard();
  }
  grillTip.refresh();
  $('away').hidden = !rows.away.length;
  $('away-count').textContent = String(rows.away.length);
  $('tab-people-count').textContent = String(rows.here.length);
}

// tagSpan is one badge square, or an empty one keeping its column. The audio one is always there
// with its state, for the people that are not in the audio too.
// el, when given, is the column's square from the last render, updated in place: an open tip stays
// on it.
function tagSpan(col, t, el = document.createElement('span')) {
  // The classes that name a state go on a filled square only.
  const on = !!(t && t[0]);
  const role = on && col === 'host';
  el.className = 'tag tag-' + col + (role ? ' role-' + col : '') + (on && col === 'host' ? ' host-badge' : '') + (on && col === 'transcribed' ? ' transcribed' : '') + (col === 'audio' ? ' audio' : '');
  const icon = t ? t[0] : '';
  if (el.dataset.icon !== icon) {
    el.replaceChildren(...(icon ? [badgeIcon(icon)] : []));
    el.dataset.icon = icon;
  }
  el.dataset.col = col;
  delete el.dataset.state;
  delete el.dataset.tip;
  el.removeAttribute('aria-label');
  if (col === 'audio') {
    el.dataset.state = t[3];
    el.dataset.tip = t[2];
    el.setAttribute('aria-label', t[2]);
  } else if (t) {
    el.dataset.tip = t[2];
    el.setAttribute('aria-label', t[1]);
  }
  if (!on) el.classList.add('blank');
  if (el.tagName === 'A') el.classList.add('anon-mark');
  else el.setAttribute('role', 'img');
  return el;
}

// anonMark is your own row's marker square: a link to the email form (011-AC7).
function anonMark() {
  const a = document.createElement('a');
  a.href = '/profile#email';
  return a;
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
  $('voice-mute').dataset.tip = mine ? 'Your voice is being written down' : '';
  $('voice-more').classList.toggle('down', sttDown);
  sttEngines = st.engines || [];
  showCredit();
  if (sttGlossary !== !!st.glossary) {
    sttGlossary = !!st.glossary;
    showManage();
  }
  // The Room settings sheet (admin/room-admin.js) shows its glossary tab from the same state, and
  // stt/device.js offers device transcription from it, the credit included (010-AC8).
  window.grillSTT = { glossary: sttGlossary, device: st.device || null };
  window.dispatchEvent(new CustomEvent('grill:stt', { detail: window.grillSTT }));
}
let sttDown = false;
let sttEngines = [];
// The model's license asks for credit wherever it runs: on the server (007-AC13) or on this
// device (010-AC8).
function showCredit() {
  const device = window.grillDeviceSTT && window.grillDeviceSTT.state !== 'off';
  $('stt-credit').hidden = !(sttEngines.includes('parakeet') || device);
}
window.addEventListener('grill:device-stt', showCredit);
// sttGlossary is true while a transcription worker reads room glossaries (007-AC14).
let sttGlossary = false;

// showManage offers Room settings to every member: each reads the room's plugins there (009-AC5),
// and the glossary while one is in use (005-AC23).
function showManage() {
  $('manage-open').hidden = false;
}

function voiceError(text) {
  $('voice-error-text').textContent = text;
  $('voice-error').hidden = !text;
}
$('voice-error-close').addEventListener('click', () => voiceError(''));

// The browser's own NotAllowedError text ("not allowed by the user agent or the platform") names no
// way out; the site's mic permission is the one.
function micError(err) {
  return err?.name === 'NotAllowedError'
    ? 'Your browser blocks the microphone here. Allow it in the site settings (the icon left of the address), then try again.'
    : 'Microphone unavailable: ' + String(err?.message).replace(/\.?$/, '.');
}

function voiceButtons() {
  const on = !!voice;
  // stt/device.js runs its engine only while this tab is in the audio (010).
  if (window.grillVoiceOn !== on) {
    window.grillVoiceOn = on;
    window.dispatchEvent(new CustomEvent('grill:voice', { detail: { on } }));
  }
  $('voice-join').hidden = on;
  $('voice-mute').hidden = !on;
  $('voice-more').hidden = !on;
  if (!on && $('voice-menu').matches(':popover-open')) $('voice-menu').hidePopover();
  $('voice-deaf').setAttribute('aria-pressed', String(on && deaf));
  $('voice-deaf').textContent = on && deaf ? 'Hear the room again' : 'Mute the room';
  const muted = on && !voice.localParticipant.isMicrophoneEnabled;
  // stt/device.js taps the live mic only: muted, or deafened without keeping it, it hears nothing (010-AC9).
  const track = muted || !on ? null : voice.localParticipant.getTrackPublication(LivekitClient.Track.Source.Microphone)?.track?.mediaStreamTrack || null;
  if (window.grillMic !== track) {
    window.grillMic = track;
    window.dispatchEvent(new CustomEvent('grill:mic', { detail: { track } }));
  }
  $('voice-mute').setAttribute('aria-pressed', String(muted));
  $('voice-mute').replaceChildren(grillIcon(muted ? 'mic-off' : 'mic'), muted ? 'Unmute' : 'Mute');
  $('voice-unlock').hidden = !on || voice.canPlaybackAudio;
}

function voiceChanged() {
  // A live mic settles any earlier mic error: the browser let it through after all.
  if (voice?.localParticipant.isMicrophoneEnabled) voiceError('');
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

// pushOn: turned on here, and the browser still allows notifications; revoked in its settings, the
// toggle offers to turn them on again.
const pushOn = () => localStorage.getItem(pushKey) === '1' && pushable && Notification.permission === 'granted';

// pushRooms records in Cache Storage the rooms with notifications on and their "every line"
// choice: the service worker renews the subscription for each (sw.js resubscribe).
async function pushRooms(edit) {
  const cache = await caches.open('grill-room-push');
  const saved = await cache.match('/rooms');
  const rooms = saved ? await saved.json() : {};
  edit(rooms);
  await cache.put('/rooms', Response.json(rooms));
}

function renderPush() {
  const on = pushOn();
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
  if (!create) return sub;
  const res = await netFetch(api + '/push');
  if (!res.ok) throw new Error('HTTP ' + res.status);
  const key = Uint8Array.from(atob((await res.json()).key.replaceAll('-', '+').replaceAll('_', '/')), (c) => c.charCodeAt(0));
  const held = sub && sub.options.applicationServerKey;
  if (sub && held && key.join() !== new Uint8Array(held).join()) {
    // Made with another VAPID key, as after the instance's key changed: push services refuse it.
    await sendJSON('DELETE', api + '/push', { subscription: { endpoint: sub.endpoint } });
    await sub.unsubscribe();
    const fresh = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
    // The other rooms turned on in this browser held the old one too.
    reg.active.postMessage({ resubscribe: fresh.toJSON() });
    return fresh;
  }
  return sub || reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
}

async function savePush() {
  const sub = await pushSubscription(true);
  const all = $('push-all').checked;
  const done = await sendJSON('PUT', api + '/push', { subscription: sub.toJSON(), all_messages: all });
  if (!done) return;
  localStorage.setItem(pushKey, '1');
  await pushRooms((rooms) => { rooms[roomID] = all; });
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
  if (pushOn()) {
    (async () => {
      const sub = await pushSubscription(false);
      if (sub) await sendJSON('DELETE', api + '/push', { subscription: { endpoint: sub.endpoint } });
      localStorage.removeItem(pushKey);
      await pushRooms((rooms) => { delete rooms[roomID]; });
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
if (pushOn() && !docsMode) {
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
    // A persona this listener muted stays unsubscribed when the room comes back (013-AC28).
    const want = on && !personaMutedIdentity(rp.identity);
    for (const pub of rp.audioTrackPublications.values()) pub.setSubscribed(want);
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
    voiceError(micError(err));
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
      if (deaf || personaMutedIdentity(p.identity)) return pub.setSubscribed(false);
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
    // ActiveDeviceChanged and a restarted track carry a new mic track: stt/device.js taps that one.
    for (const ev of ['ParticipantConnected', 'ParticipantDisconnected', 'TrackMuted', 'TrackUnmuted',
      'LocalTrackPublished', 'LocalTrackUnpublished', 'AudioPlaybackStatusChanged', 'ActiveDeviceChanged']) {
      room.on(LK.RoomEvent[ev], voiceChanged);
    }
    room.on(LK.RoomEvent.LocalTrackPublished, (pub) => pub.track?.on(LK.TrackEvent.Restarted, voiceChanged));
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
      voiceError(micError(err) + ' You can still listen.');
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

// The Diagnostics sheet (diagnostics.js, 010-AC22) reads the event stream's state, the room's name
// and the audio's connection from here; nothing leaves the page.
window.grillDiag = {
  roomName: () => roomName,
  // Whether this page receives a participant's audio, by LiveKit identity: the e2e reads a mute (013-AC28).
  subscribed: (identity) => !!voice && [...(voice.remoteParticipants.get(identity)?.audioTrackPublications.values() || [])].some((p) => p.isSubscribed),
  stream: () => (!events ? 'not open' : ['connecting', 'open', 'closed'][events.readyState]),
  voice: async () => {
    if (!voice) return { state: 'not in the audio', stats: null };
    const lp = voice.localParticipant;
    // A track's own report holds its transport and the candidate pair in use.
    const track = lp.getTrackPublication(LivekitClient.Track.Source.Microphone)?.track
      || [...voice.remoteParticipants.values()].flatMap((p) => [...p.audioTrackPublications.values()]).map((p) => p.track).find(Boolean);
    const stats = await track?.getRTCStatsReport?.().catch(() => null);
    return { state: voice.state, stats: stats || null };
  },
};

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
    .catch((err) => voiceError(micError(err)));
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
  nudgeRecovery().catch(console.error);
  offerPush();
  const m = await res.json();
  if (m.mention_notices) {
    notices.set(m.id, m.mention_notices);
    const li = messages.querySelector('li[data-id="' + m.id + '"]');
    if (li) fillMentions(li);
  }
}

// @ autocompletion (005-AC2), on the send box and on an edit box: the room's personas first, then
// the people, filtered as you type; arrows, Enter or Tab, and a tap pick one. keydown answers
// whether it took the key, so the box's own Enter/Escape run only when the list did not.
function mentionAutocomplete(input, list) {
  let hits = [];
  let at = -1;
  let pick = 0;

  function suggest() {
    const before = input.value.slice(0, input.selectionStart);
    const found = input.selectionStart === input.selectionEnd && before.match(/(?:^|[^\p{L}\p{N}_@])@([\p{L}\p{N}_-]*)$/u);
    hits = [];
    if (found) {
      const q = found[1].toLowerCase();
      at = before.length - found[1].length - 1;
      const all = [...agent.personas.map((p) => ({ token: p.name, label: p.badge + ' ' + p.name, kind: 'agent' })),
        ...participants.filter((p) => String(p.id) !== myID).map((p) => ({ token: mentionToken(p.name), label: p.name, kind: 'person' }))];
      const seen = new Set();
      for (const h of all) {
        const key = h.token.toLowerCase();
        if (h.token && key.startsWith(q) && !seen.has(key)) {
          seen.add(key);
          hits.push(h);
        }
      }
      hits = hits.slice(0, 8);
    }
    pick = 0;
    render();
  }

  function render() {
    const open = hits.length > 0;
    list.hidden = !open;
    input.setAttribute('aria-expanded', String(open));
    list.replaceChildren(...hits.map((h, i) => {
      const li = document.createElement('li');
      li.id = list.id + '-' + i;
      li.setAttribute('role', 'option');
      li.setAttribute('aria-selected', String(i === pick));
      const kind = document.createElement('span');
      kind.className = 'kind';
      kind.textContent = h.kind;
      li.append(h.label, kind);
      // On pointerdown, before the textarea loses focus to the tap.
      li.addEventListener('pointerdown', (e) => {
        e.preventDefault();
        choose(i);
      });
      return li;
    }));
    if (open) input.setAttribute('aria-activedescendant', list.id + '-' + pick);
    else input.removeAttribute('aria-activedescendant');
  }

  function choose(i) {
    const text = '@' + hits[i].token + ' ';
    const v = input.value;
    input.value = v.slice(0, at) + text + v.slice(input.selectionStart);
    input.setSelectionRange(at + text.length, at + text.length);
    input.focus();
    hits = [];
    render();
  }

  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-controls', list.id);
  input.setAttribute('aria-expanded', 'false');
  input.addEventListener('input', suggest);
  input.addEventListener('click', suggest);
  input.addEventListener('blur', () => {
    hits = [];
    render();
  });

  return {
    keydown(e) {
      if (!hits.length || e.isComposing) return false;
      const move = { ArrowDown: 1, ArrowUp: -1 }[e.key];
      if (move) {
        pick = (pick + move + hits.length) % hits.length;
        render();
      } else if (e.key === 'Enter' || e.key === 'Tab') {
        choose(pick);
      } else if (e.key === 'Escape') {
        e.stopPropagation();
        hits = [];
        render();
      } else {
        return false;
      }
      e.preventDefault();
      return true;
    },
  };
}
const sendMentions = mentionAutocomplete(bodyInput, $('mention-list'));

// The keys' hint shows on the first focus only (005-AC33): the placeholder stays short.
bodyInput.addEventListener('focus', () => {
  if (localStorage.getItem('grill-room-send-hint')) return;
  localStorage.setItem('grill-room-send-hint', '1');
  $('send-hint').hidden = false;
  setTimeout(() => { $('send-hint').hidden = true; }, 8000);
}, { once: true });

sendForm.addEventListener('submit', (e) => {
  e.preventDefault();
  send().catch(console.error);
});
bodyInput.addEventListener('keydown', (e) => {
  if (sendMentions.keydown(e)) return;
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
  reloadOwn();
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
