'use strict';

const $ = (id) => document.getElementById(id);
const roomID = decodeURIComponent(location.pathname.split('/')[2] || '');
const api = '/api/rooms/' + encodeURIComponent(roomID);
const messages = $('messages');
const sendForm = $('send');
const bodyInput = $('body');
let events;
let refreshTimer;

function end(text) {
  if (events) events.close();
  $('banner').textContent = text;
  $('banner').hidden = false;
  sendForm.hidden = true;
}

// loadRoom fetches the room header and participant list; false when access is gone.
async function loadRoom() {
  const res = await fetch(api);
  if (res.status === 401) {
    end('You are not in this room, or your access was revoked. Open your invite link again.');
    return false;
  }
  if (!res.ok) throw new Error('room: HTTP ' + res.status);
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

function connect() {
  events = new EventSource(api + '/events');
  events.onmessage = (e) => addMessage(JSON.parse(e.data));
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
