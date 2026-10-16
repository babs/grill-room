// The supervisor's admin page (003-AC26): Preact, htm and signals, vendored, no build step.
import { render } from 'preact';
import { useState, useEffect, useRef } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';
import { startSession, api } from './api.js';
import { Monitoring } from './monitor.js';

/** @typedef {import('./api.js').StatusReply} StatusReply */
/** @typedef {import('./api.js').Instance} Instance */
/** @typedef {import('./api.js').Account} Account */
/** @typedef {import('./api.js').RoomStatus} RoomStatus */

/** @type {import('@preact/signals').Signal<StatusReply | null>} */
const data = signal(null);
const banner = signal('');
const stopped = signal(false);
// picked is the server the per-server settings show; empty follows the page's current one.
const picked = signal('');

function reload() {
  return api('GET', '/api/status').then((r) => { data.value = r; });
}

// restarting is when an update restarted the app, 0 otherwise: the page waits for the new
// version on the same address instead of giving up (005-AC45).
const restarting = signal(0);
// updatingFrom is the version before the update: the old process may still answer once.
let updatingFrom = '';

// pollErr is the banner a failed poll set: the next poll that works takes it down.
let pollErr = '';

async function poll() {
  try {
    await reload();
  } catch (err) {
    if (!restarting.value) {
      if (stopped.value) return;
      banner.value = pollErr = /** @type {Error} */ (err).message;
      // A network failure passes: Chrome fails requests in flight when Docker adds or removes a
      // network (ERR_NETWORK_CHANGED), as sandboxes starting and stopping do on this machine.
      if (err instanceof TypeError) setTimeout(poll, 2000);
      return;
    }
    if (Date.now() - restarting.value > 90_000) {
      banner.value = 'The AI host app did not come back on this address. Open the link it printed in its terminal or log, or run `grill-room-daemon admin`.';
    }
    setTimeout(poll, 2000);
    return;
  }
  if (pollErr && banner.value === pollErr) banner.value = '';
  pollErr = '';
  if (restarting.value && data.value?.status.version !== updatingFrom) {
    restarting.value = 0;
    banner.value = 'Updated to ' + (data.value?.status.version || 'the new version') + '.';
  }
  if (!stopped.value) setTimeout(poll, 2000);
}

/**
 * useStatus is a form's status line and the function that runs its request: the text says it
 * worked, or why not, and the page reloads its data.
 * @returns {[string, (p: Promise<unknown>, ok?: string) => Promise<boolean>]}
 */
function useStatus() {
  const [text, setText] = useState('');
  /** @type {(p: Promise<unknown>, ok?: string) => Promise<boolean>} */
  const run = async (p, ok = '') => {
    setText('');
    try {
      await p;
      await reload();
      setText(ok);
      return true;
    } catch (err) {
      setText(sentence(/** @type {Error} */ (err).message));
      return false;
    }
  };
  return [text, run];
}

/**
 * sentence starts an error in upper case: the daemon's errors are Go errors, lower-case (005-AC42).
 * @param {string} s
 */
const sentence = (s) => s.charAt(0).toUpperCase() + s.slice(1);

/** @param {string | undefined} t */
const known = (t) => !!t && !t.startsWith('0001');
/** @param {number} n @param {string} word */
const plural = (n, word) => n + ' ' + word + (n === 1 ? '' : 's');
/** A server by its host name (003b-AC1). @param {string} url */
const hostOf = (url) => { try { return new URL(url).host; } catch { return url; } };
/** The room's id, the tail of its link. @param {string} url */
const roomID = (url) => url.replace(/\/+$/, '').split('/').pop() || url;

/** @param {{text: string, cls?: string}} p */
const Badge = ({ text, cls = '' }) => html`<span class=${'badge ' + cls}>${text}</span>`;

/** @param {{text: string, id?: string}} p */
const Said = ({ text, id }) => html`<span id=${id} class="said" role="status">${text}</span>`;

/**
 * Pill is a state as the kit's .status: a shape and a word, its reason in the tip.
 * @param {{word: string, look: string, tip?: string, id?: string}} p
 */
const Pill = ({ word, look, tip, id }) => html`<span id=${id} class=${'status ' + look} tabindex=${tip ? 0 : undefined} data-tip=${tip || undefined}>${word}</span>`;

/** How far t is from now: "5 min ago", or "in 3 h" for an expiry ahead. @param {string} t */
const rel = (t) => {
  const s = (new Date(t).getTime() - Date.now()) / 1000;
  if (s < 60) return window.grillAgo(t);
  return 'in ' + (s < 3600 ? Math.floor(s / 60) + ' min' : s < 86400 ? Math.floor(s / 3600) + ' h' : Math.floor(s / 86400) + ' d');
};

/** A date as "5 min ago" or "in 3 h", the full date in its tip. @param {{t: string | undefined}} p */
const Ago = ({ t }) => (known(t) ? html`<span tabindex="0" data-tip=${window.grillDate(/** @type {string} */ (t))}>${rel(/** @type {string} */ (t))}</span>` : html`never`);

/** icon is an icon of the shared set (kit/ui.js) as a control's content: the control names itself. @param {string} name */
const icon = (name) => ({ __html: window.grillIconHTML(name) });

/**
 * Value is a value people copy, cut to one line, whole in the tip and the copy button (kit .value).
 * @param {{text: string, label: string, id?: string}} p
 */
const Value = ({ text, label, id }) => html`<span id=${id} class="value copyable" data-value=${text}>
  <span class="value-text" tabindex="0" data-tip=${text}>${text}</span>
  <button type="button" class="btn-icon btn-ghost copy" aria-label=${'Copy the ' + label} dangerouslySetInnerHTML=${icon('copy')}></button></span>`;

/**
 * More is a ⋯ button and its menu of actions; each item closes the menu.
 * @param {{id: string, label: string, items: [string, () => void, string?][]}} p
 */
const More = ({ id, label, items }) => html`<button type="button" class="btn-icon btn-ghost more" popovertarget=${id} aria-label=${label}
  dangerouslySetInnerHTML=${icon('more')}></button>
  <div id=${id} class="menu row-menu" popover>${items.map(([text, run, cls]) => html`<button type="button" class=${cls}
    popovertarget=${id} popovertargetaction="hide" onClick=${run}>${text}</button>`)}</div>`;

/** @param {{id: string, title: string, children?: any}} p */
const Section = ({ id, title, children }) => html`
  <section id=${id + '-section'} aria-labelledby=${id + '-title'}>
    <h2 id=${id + '-title'}>${title}</h2>
    ${children}
  </section>`;

const selfText = /** @type {Record<string, [string, string]>} */ ({
  project: ['project-signed', 'ok'],
  'dev-key': ['signed with a dev key', 'ok'],
  'dev build': ['dev build', ''],
  unsigned: ['NOT project-signed', 'bad'],
  unknown: ['signature not checked', ''],
});

function Header() {
  const d = data.value;
  const t = d?.trust;
  const [sig, cls] = t ? selfText[t.self.state] || [t.self.state, ''] : ['', ''];
  const stop = () => window.grillConfirm('Stop the AI host app?', 'Running answers fail, and rooms lose their AI until you start it again.', 'Stop')
    .then((yes) => {
      if (yes) return api('POST', '/api/stop').then(() => {
        stopped.value = true;
        banner.value = 'The AI host app stopped. You can close this tab.';
      });
    }).catch((err) => { banner.value = err.message; });
  // On a phone the version and signature go: the Update tab shows both (003b-AC21).
  return html`
    <header class="app-header">
      <img src="/static/logo.svg" alt="" class="logo" width="32" height="32"/>
      <h1><span class="wide-only">grill-room </span>AI host</h1>
      <span id="host" class="muted wide-only">${d ? d.status.host + ' · ' + d.status.version : ''}</span>
      ${t && html`<span id="self-sig" class=${'badge wide-only ' + cls}>${sig}</span>`}
      <span class="spacer"></span>
      ${(d || stopped.value) && html`<span id="state" class=${'status ' + (stopped.value ? 'off' : 'ok')}>${stopped.value ? 'Stopped' : 'Running'}</span>`}
      <button type="button" id="stop" class="btn-secondary" onClick=${stop} disabled=${stopped.value}>Stop</button>
    </header>`;
}

// The page's tabs (005-AC17), each a deep link (#accounts). Rooms comes first to the eye of whoever
// pairs: the daemon opens its page there. The servers live on Rooms now; #instances still opens it
// (003b-AC1).
const tabs = [['rooms', 'Rooms'], ['accounts', 'AI accounts'], ['settings', 'Settings'], ['monitoring', 'Activity'], ['update', 'Update']];
/** @param {string} hash */
const tabOf = (hash) => tabs.find(([id]) => '#' + id === hash)?.[0] || 'rooms';
const tab = signal(tabOf(location.hash));
addEventListener('hashchange', () => { tab.value = tabOf(location.hash); });

// Each alert kind belongs to the tab that resolves it: logins near expiry, unsigned binaries, full
// contexts and stuck queues (003-AC29).
const alertTab = /** @type {Record<string, string>} */ ({ account: 'accounts', unsigned: 'rooms', context: 'rooms', queue: 'settings' });
/** @param {string} id */
const alertsOf = (id) => (data.value?.alerts || []).filter((a) => (alertTab[a.kind] || 'monitoring') === id);

// TabBar stays pinned at the top; a tab with alerts, or an update waiting, shows their count.
function TabBar() {
  const nav = useRef(/** @type {HTMLElement | null} */ (null));
  useEffect(() => { window.grillShowTab(nav.current); }, [tab.value]);
  return html`<nav id="tabs" ref=${nav} class="tabs sticky" aria-label="Sections">${tabs.map(([id, name]) => {
    const n = alertsOf(id).length + (id === 'update' && data.value?.status.update?.available ? 1 : 0);
    return html`<a key=${id} id=${'tab-' + id} href=${'#' + id} aria-current=${tab.value === id ? 'page' : undefined}>${name}${n
      ? html` <span class="tab-badge" data-tip=${n + (n === 1 ? ' alert' : ' alerts')}>${n}</span>` : ''}</a>`;
  })}</nav>`;
}

/** @param {{id: string}} p */
function Alerts({ id }) {
  const list = alertsOf(id);
  if (!list.length) return null;
  return html`<ul id=${'alerts-' + id} class="alerts" role="alert">${list.map((a) => html`<li class=${'alert-' + a.kind}>${a.text}</li>`)}</ul>`;
}

/** @param {{id: string, children?: any}} p */
const Panel = ({ id, children }) => html`<div id=${'panel-' + id} class="panel" hidden=${tab.value !== id}><${Alerts} id=${id} />${children}</div>`;

function RelaxBanner() {
  const relaxed = (data.value?.instances || []).filter((i) => i.accept_unsigned);
  if (!relaxed.length) return null;
  return html`<p id="relax-banner" class="danger" role="alert">
    Unsigned binaries are accepted for: ${relaxed.map((i) => i.url).join(', ')}. Switch it off in the server's menu once you are done testing.
  </p>`;
}

// UpdateOffer is the supervisor's own update, applied only on approval (003-AC5).
function UpdateOffer() {
  const u = data.value?.status.update;
  const [busy, setBusy] = useState(false);
  const [text, setText] = useState('');
  if (!u?.available) return html`<p class="hint">${u?.running ? 'Downloading…' : 'No update waiting.'}</p>`;
  const apply = () => {
    setBusy(true);
    setText('Downloading…');
    updatingFrom = data.value?.status.version || '';
    api('POST', '/api/update').then(() => {
      restarting.value = Date.now();
      banner.value = 'Updating… the AI host app restarts, and this page reconnects by itself.';
    }).catch((err) => { setBusy(false); setText(err.message); });
  };
  return html`<p id="update">
    Version <strong id="update-version">${u.available}</strong> of the AI host app is available from <span id="update-from">${u.from || 'the room server'}</span>.
    <button type="button" id="update-apply" onClick=${apply} disabled=${busy || u.running}>Update and restart</button>
    <span id="update-status" role="status">${text || (u.running ? 'Downloading…' : u.error || '')}</span>
  </p>`;
}

const pairingText = /** @type {Record<string, string>} */ ({
  '': 'Not paired.',
  requesting: 'Asking the room to pair…',
  pending: 'Waiting for your approval in the room.',
  paired: 'Paired.',
});

// AddRoom pairs this computer with a room link, as a second launch would (008-AC15); its status
// line says how the room just added pairs (003b-AC4).
function AddRoom() {
  const s = /** @type {StatusReply} */ (data.value).status;
  const [url, setURL] = useState('');
  const [err, setErr] = useState('');
  let text = s.room ? pairingText[s.pairing] ?? s.pairing : 'No room yet: paste a room link.';
  if (s.room && s.pairing === 'paired') text += ' ' + ({ connected: 'Connected.', connecting: 'Connecting…', offline: 'Offline.' }[s.connection] || '');
  /** @param {Event} e */
  const pair = (e) => {
    e.preventDefault();
    setErr('');
    api('POST', '/api/room', { url }).then(() => { setURL(''); return reload(); }).catch((x) => setErr(x.message));
  };
  return html`<form id="room-form" class="add-room" onSubmit=${pair}>
      <input id="room-url" type="url" required aria-label="Room link" placeholder="Room link: https://…/r/…" value=${url}
        data-tip="Any link inside the room works: the room page or its documents." onInput=${(/** @type {any} */ e) => setURL(e.currentTarget.value)} />
      <button type="submit">Add a room</button>
    </form>
    <p class="pairing-line"><span id="pairing" role="status">${text + (s.room && s.error ? ' ' + s.error : '')}</span>${' '}${s.room && s.pairing === 'pending' && /^https?:\/\//.test(s.approve_url || '') && html`<span id="approve">
        Check that the room page shows the code <strong id="code">${s.code}</strong>, then${' '}<a id="approve-link" href=${s.approve_url} target="_blank" rel="noopener noreferrer">approve in the room</a>.
      </span>`}</p>
    <p id="room-error" class="bad" role="alert">${err}</p>`;
}

/**
 * binaryText is what a server's app runs, in the page's words (003-AC2, 003-AC4).
 * @param {Instance} i
 */
function binaryText(i) {
  if (i.unsigned) return 'an unsigned binary';
  if (i.trust === "the supervisor's own binary") return "this app's own binary";
  return i.trust || '';
}

/**
 * AccountSelect picks a server's AI account, or one room's (003-AC13); a room's first option goes
 * back to its server's.
 * @param {{label: string, value: string, roomDefault?: string, id?: string, onPick: (v: string) => void}} p
 */
function AccountSelect({ label, value, roomDefault, id, onPick }) {
  const accounts = data.value?.accounts || [];
  return html`<select id=${id} aria-label=${label} value=${value} onChange=${(/** @type {any} */ e) => onPick(e.currentTarget.value)}>
    ${roomDefault !== undefined && html`<option value="">${"server's (" + roomDefault + ')'}</option>`}
    ${accounts.map((a) => html`<option value=${a.name}>${a.name}</option>`)}
  </select>`;
}

/** @param {string} instance @param {string} room @param {string} account */
const setAccount = (instance, room, account) =>
  api('POST', '/api/instance-account', { instance, room, account }).then(reload).catch((err) => { banner.value = err.message; });

/** @param {number} s */
const uptime = (s) => (s < 120 ? s + ' s' : s < 7200 ? Math.round(s / 60) + ' min' : Math.round(s / 3600) + ' h');

/**
 * cutOff stops a server's app from reaching the AI, once confirmed, or lets it again (003-AC15).
 * @param {Instance} i
 */
function cutOff(i) {
  const go = () => api('POST', '/api/cut-off', { instance: i.id, cut_off: !i.cut_off }).then(reload).catch((err) => { banner.value = err.message; });
  if (i.cut_off) return go();
  return window.grillConfirm('Cut ' + hostOf(i.url) + ' off from the AI?', 'Its running answers end and none starts until you let it again.', 'Cut it off')
    .then((yes) => { if (yes) go(); });
}

/**
 * serverPill is a server's state: its app's, then how its rooms reach it, the best one first.
 * @param {Instance} i
 */
function serverPill(i) {
  if (i.state !== 'running') return { word: i.state, look: i.state === 'starting' ? 'busy' : 'failed', tip: i.error };
  const rs = i.room_states || [];
  if (!rs.length) return { word: 'running', look: 'idle' };
  if (rs.some((r) => r.connection === 'connected')) return { word: 'connected', look: 'ok' };
  if (rs.some((r) => r.connection === 'connecting' || r.pairing === 'pending' || r.pairing === 'requesting')) return { word: 'connecting', look: 'busy' };
  return { word: 'offline', look: 'failed' };
}

/**
 * Relax is the "accept unsigned binaries" form, behind the server's menu: the host types the
 * server's host to switch it on (003-AC4, 003b-AC19).
 * @param {{i: Instance, onDone: () => void}} p
 */
function Relax({ i, onDone }) {
  const [host, setHost] = useState('');
  const [text, run] = useStatus();
  /** @param {Event} e */
  const submit = (e) => {
    e.preventDefault();
    window.grillConfirm('Accept unsigned binaries from ' + host + '?', 'This server could then run any program on this computer. Switch it off once you are done testing.', 'Accept')
      .then((yes) => { if (yes) run(api('POST', '/api/relax', { instance: i.id, enabled: true, host }), 'Unsigned binaries accepted.').then((ok) => { if (ok) onDone(); }); });
  };
  return html`<form class="relax" onSubmit=${submit}>
    <label>To accept its unsigned binaries, type its host <input class="relax-host" required size="24" autocomplete="off" spellcheck=${false}
      value=${host} onInput=${(/** @type {any} */ e) => setHost(e.currentTarget.value)} /></label>
    <button type="submit" class="btn-danger-outline">Accept unsigned binaries</button>
    <button type="button" class="btn-ghost" onClick=${onDone}>Cancel</button>
    <${Said} text=${text} />
  </form>`;
}

/**
 * roomPill is a room's state (003b-AC3): what this computer does for it, colour by kind.
 * @param {RoomStatus | undefined} st @param {boolean} paused
 * @returns {{word: string, look: string, tip?: string}}
 */
function roomPill(st, paused) {
  if (!st) return { word: 'not served', look: 'off' };
  if (st.pairing === 'pending') return { word: 'waiting for approval', look: 'busy' };
  if (st.pairing === 'requesting') return { word: 'asking to pair', look: 'busy' };
  if (st.pairing !== 'paired') return { word: 'not paired', look: 'failed', tip: st.error };
  if (st.connection === 'connecting') return { word: 'connecting', look: 'busy' };
  if (st.connection !== 'connected') return { word: 'disconnected', look: 'failed', tip: st.error };
  if (paused) return { word: 'paused', look: 'off', tip: 'Another computer can run the AI for this room meanwhile.' };
  if (!st.can_host) return { word: "can't run the AI", look: 'failed', tip: st.reason };
  if (st.host === false) return { word: 'standby', look: 'idle', tip: 'Another computer runs the AI for this room; this one takes over if it goes.' };
  return { word: 'runs the AI', look: 'ok', tip: st.host ? 'Parallel answers ' + (st.parallel_turns ? 'on' : 'off') + '.' : undefined };
}

/**
 * removeRoom takes a room off this computer, once confirmed; a server that does not answer gets a
 * second question (003b-AC6).
 * @param {string} room @param {string} name
 */
async function removeRoom(room, name) {
  if (!await window.grillConfirm('Remove ' + name + ' from this computer?',
    'This computer stops running its AI and forgets it. To serve it again, add its link and approve the pairing in the room.', 'Remove')) return;
  try {
    await api('POST', '/api/room-remove', { room });
  } catch (err) {
    const msg = /** @type {Error} */ (err).message;
    // Only a server that cannot hear of it, or an app that does not run, is worth removing here alone.
    if (!/did not answer|too old to forget|is not running/.test(msg)) { banner.value = msg; return; }
    if (!await window.grillConfirm('Remove it here anyway?', sentence(msg) + '. The room keeps listing this computer until someone removes it there.', 'Remove anyway')) return;
    try {
      await api('POST', '/api/room-remove', { room, force: true });
    } catch (x) {
      banner.value = /** @type {Error} */ (x).message;
      return;
    }
  }
  await reload();
  window.grillToast('Removed ' + name);
}

/**
 * RoomRow is one room as a list row (003b-AC2): its name linked, its state, its AI account, Pause
 * or Connect, and a ⋯ with Remove. The room's plugin requests and options follow it (003b-AC8).
 * @param {{i: Instance, room: string}} p
 */
function RoomRow({ i, room }) {
  const d = /** @type {StatusReply} */ (data.value);
  const st = (i.room_states || []).find((x) => x.room === room);
  const paused = (i.paused || []).includes(room);
  const name = st?.name || roomID(room);
  const pill = roomPill(st, paused);
  // Connect for a room not paired, or paired but neither connected nor connecting; Pause otherwise.
  const down = !st || st.pairing === '' || (st.pairing === 'paired' && st.connection !== 'connected' && st.connection !== 'connecting');
  const act = (/** @type {Promise<unknown>} */ p, /** @type {string} */ ok) => p.then(reload).then(() => window.grillToast(ok)).catch((err) => { banner.value = err.message; });
  const pause = () => act(api('POST', '/api/room-pause', { instance: i.id, room, paused: !paused }),
    paused ? 'Resumed: this computer can run the AI for ' + name : 'Paused: ' + name + ' runs its AI elsewhere, or waits');
  const connect = () => act(api('POST', '/api/room', { url: room }), 'Connecting to ' + name);
  const extra = st && (st.fetches?.length || st.plugins);
  return html`<li class="list-row room-row" data-room=${room}>
      <div class="main"><a class="name room-name" href=${room} target="_blank" rel="noopener noreferrer" data-tip=${room}>${name}</a>
        <${Pill} ...${pill} />${st?.running ? html` <span class="meta">${st.running} answering</span>` : ''}${st?.queued ? html` <span class="meta">${st.queued} waiting</span>` : ''}</div>
      <div class="controls">
        ${d.accounts && html`<${AccountSelect} label=${'AI account of ' + room} value=${(i.room_accounts || {})[room] || ''} roomDefault=${i.account}
          onPick=${(/** @type {string} */ a) => setAccount(i.id, room, a)} />`}
        ${down ? html`<button type="button" class="btn-secondary" aria-label=${'Connect ' + name} onClick=${connect}>Connect</button>`
          : html`<button type="button" class="btn-secondary" aria-label=${(paused ? 'Resume ' : 'Pause hosting ') + name} onClick=${pause}>${paused ? 'Resume' : 'Pause hosting'}</button>`}
      </div>
      <div class="actions"><${More} id=${'room-menu-' + i.id + '-' + roomID(room)} label=${'More actions on ' + name} items=${[
        ['Copy the link', () => navigator.clipboard.writeText(room).then(() => window.grillToast('Link copied'))],
        ...(down ? [] : [[paused ? 'Resume' : 'Pause hosting', pause]]),
        ['Remove this room', () => removeRoom(room, name), 'danger'],
      ]} /></div>
    </li>
    ${extra ? html`<li class="room-extra"><${RoomPlugins} i=${i} room=${room} st=${/** @type {RoomStatus} */ (st)} /></li>` : ''}`;
}

/**
 * Server is one room server as a section: its host name, state, a ⋯ with the app's details and
 * the risky switches, then its rooms (003b-AC1).
 * @param {{i: Instance}} p
 */
function Server({ i }) {
  const [details, setDetails] = useState(false);
  const [relax, setRelax] = useState(false);
  const [text, run] = useStatus();
  const m = i.metrics;
  const pill = serverPill(i);
  const items = /** @type {[string, () => void, string?][]} */ ([[details ? 'Hide the app details' : 'App details', () => setDetails(!details)]]);
  items.push(i.accept_unsigned ? ['Stop accepting unsigned binaries', () => run(api('POST', '/api/relax', { instance: i.id, enabled: false, host: '' }))]
    : ['Accept unsigned binaries…', () => setRelax(true)]);
  if (data.value?.accounts) items.push([i.cut_off ? 'Let it use the AI again' : 'Cut it off from the AI', () => cutOff(i), i.cut_off ? '' : 'danger']);
  return html`<section class="server" id=${'server-' + i.id} aria-labelledby=${'server-title-' + i.id}>
    <div class="server-head">
      <h3 id=${'server-title-' + i.id} data-tip=${i.url} tabindex="0">${hostOf(i.url)}</h3>
      <${Pill} ...${pill} />
      ${i.version && html`<span class="muted">${i.version}</span>`}
      ${i.accept_unsigned && html`<${Badge} text="accepts unsigned binaries" cls="danger" />`}
      ${i.cut_off && html`<${Badge} text="cut off: its answers end and none starts" cls="danger" />`}
      <span class="spacer"></span>
      <${More} id=${'server-menu-' + i.id} label=${'More actions on ' + hostOf(i.url)} items=${items} />
    </div>
    ${details && html`<p class="hint server-details">${'App binary: ' + binaryText(i) + '.'}
      ${i.state === 'running' && ' Process ' + i.pid + ', protocol ' + i.proto + (m ? ', up ' + uptime(m.uptime_seconds) + ', ' +
        plural(m.turns, 'answer') + (m.failed_turns ? ' (' + m.failed_turns + ' failed)' : '') : '') + '.'}
      ${i.restarts ? ' ' + plural(i.restarts, 'restart') + '.' : ''} ${data.value?.accounts ? plural(i.requests, 'request') + ' to the AI.' : ''}</p>`}
    ${i.state !== 'running' && i.error && html`<p class="bad server-error">${sentence(i.error)}</p>`}
    ${relax && html`<${Relax} i=${i} onDone=${() => setRelax(false)} />`}
    <${Said} text=${text} />
    <ul class="list-rows rooms">
      ${!i.rooms.length && html`<li class="empty">No room on this server.</li>`}
      ${i.rooms.map((r) => html`<${RoomRow} key=${r} i=${i} room=${r} />`)}
    </ul>
  </section>`;
}

function Rooms() {
  const d = /** @type {StatusReply} */ (data.value);
  const c = d.concurrency;
  const secs = (/** @type {number} */ ms) => (ms / 1000).toFixed(1) + ' s';
  let conc = '';
  if (c) {
    conc = 'Now ' + c.running.length + ' answering, ' + c.queued.length + ' waiting. Since start: ' + plural(c.granted, 'answer');
    if (c.waited) conc += ', ' + c.waited + ' waited for a slot, ' + secs(c.wait_total_ms / c.waited) + ' on average, ' + secs(c.wait_max_ms) + ' at worst';
    conc += '.';
  }
  return html`<${Section} id="rooms" title="Rooms">
    <${AddRoom} />
    ${conc && html`<p class="hint" id="concurrency">${conc}</p>`}
    ${d.instances && html`<div id="instances">${d.instances.map((i) => html`<${Server} key=${i.id} i=${i} />`)}</div>`}
  <//>`;
}

// optionsWarning is the form's warning, word for word from 009-AC23.
const optionsWarning = 'The AI can read these values during a turn. Anyone in the room can ask it to show them.';

/**
 * FetchRequest is a room's request that this computer fetch a repo the room server cannot read
 * (009-AC19): who asked, the link and ref, Fetch or Decline. Nothing runs git before Fetch.
 * @param {{i: Instance, room: string, f: import('./api.js').HostFetch}} p
 */
function FetchRequest({ i, room, f }) {
  const [text, run] = useStatus();
  /** @param {boolean} fetch */
  const answer = (fetch) => run(api('POST', '/api/plugin-fetch', { instance: i.id, room, id: f.id, fetch }),
    fetch ? 'Fetching: the room gets the plugin, or why not, in a moment.' : 'Declined.');
  const what = f.plugin_name ? 'an update of the plugin ' + f.plugin_name : f.entries?.length ? 'the plugins ' + f.entries.join(', ') : 'a plugin';
  return html`<li class="fetch-request" data-fetch=${f.id}>
    <div>${f.requested_by} asks for ${what}, <${Ago} t=${f.created_at} />.</div>
    <div class="hint"><code class="fetch-url">${f.url}</code>, ${f.ref ? html`ref <code>${f.ref}</code>` : 'default branch'}</div>
    <div class="row">
      <button type="button" class="btn-primary" disabled=${f.fetching} aria-label=${'Fetch ' + f.url} onClick=${() => answer(true)}>${f.fetching ? 'Fetching…' : 'Fetch'}</button>
      <button type="button" class="btn-secondary" disabled=${f.fetching} aria-label=${'Decline ' + f.url} onClick=${() => answer(false)}>Decline</button>
      <${Said} text=${text} />
    </div>
  </li>`;
}

/**
 * optionInput is one option's field: a checkbox for yes or no, a number, or text; a sensitive one
 * is masked and starts empty, its value never shown again (009-AC22).
 * @param {{o: import('./api.js').PluginOption, id: string, value: unknown, set: boolean, onValue: (v: unknown) => void}} p
 */
function OptionInput({ o, id, value, set, onValue }) {
  const label = o.title || o.key;
  if (o.type === 'boolean') {
    return html`<label class="option"><input id=${id} type="checkbox" checked=${value === true}
      onChange=${(/** @type {any} */ e) => onValue(e.currentTarget.checked)} /> ${label}</label>`;
  }
  const kind = o.sensitive ? 'password' : o.type === 'number' ? 'number' : 'text';
  const hint = o.sensitive ? (set ? 'set: type to replace it' : 'not set') : o.default !== undefined ? 'default: ' + String(o.default) : '';
  return html`<label class="option">${label}${o.required ? ' (required)' : ''}
    <input id=${id} type=${kind} autocomplete=${o.sensitive ? 'new-password' : 'off'} spellcheck=${false} placeholder=${hint}
      value=${value === undefined || value === null ? '' : String(value)}
      onInput=${(/** @type {any} */ e) => onValue(kind === 'number' && e.currentTarget.value !== '' ? Number(e.currentTarget.value) : e.currentTarget.value)} /></label>`;
}

/**
 * PluginSetup is one room plugin's options form, and the websites its MCP servers name with an
 * "Add to allowlist" button for each one off the allowlist (009-AC22, 009-AC24, 009-AC25).
 * @param {{i: Instance, room: string, p: import('./api.js').HostPlugin, allowlist: string[]}} props
 */
function PluginSetup({ i, room, p, allowlist }) {
  const opts = p.options || [];
  const [draft, setDraft] = useState(/** @type {Record<string, unknown>} */ ({}));
  const [text, run] = useStatus();
  const [listText, runList] = useStatus();
  const set = p.set || [];
  /** @param {Event} e */
  const save = (e) => {
    e.preventDefault();
    run(api('POST', '/api/plugin-options', { instance: i.id, room, plugin: p.id, values: draft }), 'Saved.').then((ok) => { if (ok) setDraft({}); });
  };
  /** @param {string} key */
  const forget = (key) => run(api('POST', '/api/plugin-options', { instance: i.id, room, plugin: p.id, values: {}, clear: [key] }), 'Cleared.');
  /** @param {string} dom */
  const allow = (dom) => runList(api('POST', '/api/allowlist', { instance: i.id, allowlist: [...allowlist, dom] }), dom + ' added to the allowlist.');
  const base = 'opt-' + p.name + '-';
  return html`<div class="plugin-setup" data-plugin=${p.name}>
    <h4>${p.name} <span class="muted">v${p.version}</span></h4>
    ${p.needs_setup && html`<p class="bad needs-setup">Needs host setup: it does not load until its required options are set.</p>`}
    ${opts.length ? html`<form class="plugin-options" onSubmit=${save}>
      <p class="hint warn-line">${optionsWarning}</p>
      ${opts.map((o) => html`<div class="option-row">
        <${OptionInput} o=${o} id=${base + o.key} set=${set.includes(o.key)}
          value=${o.key in draft ? draft[o.key] : o.sensitive ? '' : p.values?.[o.key] ?? o.default}
          onValue=${(/** @type {unknown} */ v) => setDraft({ ...draft, [o.key]: v })} />
        ${o.sensitive && set.includes(o.key) && html`<button type="button" class="btn-secondary" aria-label=${'Clear ' + (o.title || o.key)}
          onClick=${() => forget(o.key)}>Clear</button>`}
        ${o.description && html`<span class="hint">${o.description}</span>`}
      </div>`)}
      <div class="row"><button type="submit">Save</button><${Said} text=${text} /></div>
    </form>` : ''}
    ${p.domains?.length ? html`<p class="hint">Websites its MCP servers reach:</p>
      <ul class="plugin-domains">${p.domains.map((dom) => {
        const off = p.off_allowlist?.includes(dom);
        return html`<li><code>${dom}</code> <span class=${off ? 'bad' : 'muted'}>${off ? 'not on the allowlist' : 'on the allowlist'}</span>
          ${off && html` <button type="button" aria-label=${'Add ' + dom + ' to the allowlist'} onClick=${() => allow(dom)}>Add to allowlist</button>`}</li>`;
      })}</ul><${Said} text=${listText} />` : ''}
  </div>`;
}

/**
 * RoomPlugins is what a room this computer hosts asks of it: the fetches waiting for it, shown, and
 * the plugins to set up, behind a disclosure (003b-AC8). Empty for a room another computer hosts.
 * @param {{i: Instance, room: string, st: RoomStatus}} p
 */
function RoomPlugins({ i, room, st }) {
  const fetches = st.fetches || [];
  const plugins = (st.plugins || []).filter((p) => p.options?.length || p.domains?.length);
  const setup = plugins.some((p) => p.needs_setup);
  return html`<div class="room-plugins">
    ${fetches.length ? html`<h4>Asked to fetch</h4>
      <p class="hint">Fetch runs this computer's git, with your ssh keys and git logins, outside every sandbox.</p>
      <ul class="fetches">${fetches.map((f) => html`<${FetchRequest} key=${f.id} i=${i} room=${room} f=${f} />`)}</ul>` : ''}
    ${st.plugins && html`<${Plugins} setup=${setup} count=${plugins.length}>
      ${plugins.length ? plugins.map((p) => html`<${PluginSetup} key=${p.name} i=${i} room=${room} p=${p} allowlist=${i.status?.allowlist || []} />`)
        : html`<p class="hint">No plugin of this room has options or reaches a website.</p>`}
    <//>`}
  </div>`;
}

/**
 * Plugins is the room's plugins folded away; a plugin needing setup opens it, and only the host
 * closes it again: saving the options must not fold the form being filled.
 * @param {{setup: boolean, count: number, children?: any}} p
 */
function Plugins({ setup, count, children }) {
  const [open, setOpen] = useState(setup);
  useEffect(() => { if (setup) setOpen(true); }, [setup]);
  return html`<details class="plugins" open=${open} onToggle=${(/** @type {any} */ e) => setOpen(e.currentTarget.open)}>
    <summary>Plugins${count ? ' (' + count + ')' : ''}</summary>${children}</details>`;
}

// An account's state: its words and its pill kind.
const stateText = /** @type {Record<string, [string, string]>} */ ({
  valid: ['valid', 'ok'], expired: ['expired', 'failed'], refused: ['refused', 'failed'], unknown: ['not checked yet', 'idle'], error: ['unreadable', 'failed'],
});

/** @param {{a: Account, onError: (t: string) => void}} p */
function AccountItem({ a, onError }) {
  const [text, look] = stateText[a.state] || [a.state, 'idle'];
  const kind = a.kind === 'setup_token' ? 'setup token, in the ' + (a.storage === 'keyring' ? 'OS keyring' : 'owner-only file')
    : 'config folder ' + (a.display_dir || a.config_dir || '~/.claude');
  const total = Object.values(a.requests || {}).reduce((n, x) => n + x, 0);
  const remove = () => {
    window.grillConfirm('Remove the AI account ' + a.name + '?', 'Rooms that use it stop reaching the AI until they get another account.', 'Remove')
      .then((yes) => { if (yes) api('POST', '/api/accounts/remove', { name: a.name }).then(reload).catch((err) => onError(err.message)); });
  };
  const items = /** @type {[string, () => void, string?][]} */ ([]);
  if (a.kind === 'setup_token') items.push(['Check now', () => api('POST', '/api/accounts/probe', { name: a.name }).then(reload).catch((err) => onError(err.message))]);
  items.push(['Remove', remove, 'danger']);
  // Every account is a Claude login today; the badge leaves room for other providers (003b-AC12).
  return html`<li class="list-row">
    <div class="main"><span class="name">${a.name}</span> <${Badge} text="Claude" cls="provider" /> <${Pill} word=${text} look=${look} tip=${a.detail} />
      <div class="meta">${kind}</div>
      <div class="meta">${a.expires_at && html`${a.expiry_estimated ? 'expires about ' : 'access token expires '}<${Ago} t=${a.expires_at} />${' · '}`}last use <${Ago} t=${a.last_use} />${' · ' + plural(total, 'request')}${a.checked_at && html`${' · checked '}<${Ago} t=${a.checked_at} />`}</div></div>
    <div class="actions"><${More} id=${'account-menu-' + a.name} label=${'Actions on ' + a.name} items=${items} /></div>
  </li>`;
}

// Accounts lists the AI accounts with their status, expiry and use, and adds one: a setup token is
// typed once, then never shown again (003-AC12).
function Accounts() {
  const accounts = data.value?.accounts;
  const [name, setName] = useState('');
  const [kind, setKind] = useState('config_dir');
  const [dir, setDir] = useState('');
  const [tok, setTok] = useState('');
  const [text, run] = useStatus();
  const [listErr, setListErr] = useState('');
  if (!accounts) return null;
  /** @param {Event} e */
  const add = (e) => {
    e.preventDefault();
    run(api('POST', '/api/accounts', { name: name.trim(), kind, config_dir: kind === 'config_dir' ? dir.trim() : '', token: kind === 'setup_token' ? tok : '' }),
      'Added.').then((ok) => { if (ok) { setName(''); setDir(''); } });
    // The token is never shown again, not even in this field.
    setTok('');
  };
  /** @param {(v: string) => void} set */
  const on = (set) => (/** @type {any} */ e) => set(e.currentTarget.value);
  return html`<${Section} id="accounts" title="AI accounts">
    <p class="hint">Each account has its own proxy in this app: the rooms' sandboxes get its address, never a token. Rooms see none of this page.</p>
    <ul id="accounts" class="list-rows">${accounts.map((a) => html`<${AccountItem} key=${a.name} a=${a} onError=${setListErr} />`)}</ul>
    <p class="bad" role="alert">${listErr}</p>
    <h3>Add an account</h3>
    <form id="account-form" class="stack-form" onSubmit=${add}>
      <label>Name <input id="account-name" required size="16" autocomplete="off" spellcheck=${false} pattern="[A-Za-z0-9][A-Za-z0-9._\\-]{0,63}"
        value=${name} onInput=${on(setName)} /></label>
      <label>Kind <select id="account-kind" value=${kind} onChange=${on(setKind)}>
        <option value="config_dir">a Claude Code config folder</option>
        <option value="setup_token">a setup token</option>
      </select></label>
      ${kind === 'config_dir'
        ? html`<label>Config folder <input id="account-dir" size="30" autocomplete="off" spellcheck=${false} placeholder="~/.claude-work, empty for ~/.claude"
            value=${dir} onInput=${on(setDir)} /></label>`
        : html`<label>Token <input id="account-token" type="password" required size="30" autocomplete="off" spellcheck=${false}
            placeholder="from claude setup-token" value=${tok} onInput=${on(setTok)} /></label>`}
      <button type="submit">Add the account</button>
    </form>
    <p class="hint">A config folder is read only: Claude Code, run by you, refreshes its login. A setup token goes to the OS keyring, or to a file only you can read; it is not shown again. <span id="account-status" role="status">${text}</span></p>
  <//>`;
}

/** @param {{t: import('./api.js').Trust}} p */
function TrustFacts({ t }) {
  const [sig] = selfText[t.self.state] || [t.self.state];
  const r = t.root;
  let root = /** @type {Record<string, string>} */ ({ tuf: 'refreshed through TUF', disk: 'last good copy on disk', embedded: 'embedded in this binary' })[r.source] || 'not loaded yet';
  if (r.source && known(r.at)) root += ', ' + window.grillDate(r.at);
  if (r.error) root += ' (TUF unreachable: ' + r.error + ')';
  const d = /** @type {StatusReply} */ (data.value);
  return html`<dl>
    <dt>This app</dt><dd id="self-detail">${d.status.host} · ${d.status.version} <span class=${'status ' + (t.self.state === 'unsigned' ? 'failed' : 'ok')}>${sig}</span>
      ${t.self.detail && (['project', 'dev-key'].includes(t.self.state) ? html` <${Value} text=${t.self.detail} label="signer identity" />` : ' ' + t.self.detail)}</dd>
    <dt>Trusted root</dt><dd id="root">${root}</dd>
    <dt>Last update check</dt><dd id="checked">${(known(t.checked) ? window.grillDate(t.checked) : 'not yet') + (t.check_error ? ': ' + t.check_error : '')}</dd>
  </dl>`;
}

// Trust shows the app's signature and update source, and the trusted dev keys (003-AC4, 003-AC5).
function Trust() {
  const d = /** @type {StatusReply} */ (data.value);
  const t = d.trust;
  const instances = d.instances || [];
  const [channel, setChannel] = useState(t?.channel || 'github');
  const [follow, setFollow] = useState(t?.follow || '');
  const [chText, runCh] = useStatus();
  const [keyName, setKeyName] = useState('');
  const [pem, setPem] = useState('');
  const [keyText, runKey] = useStatus();
  if (!t) return null;
  const followed = follow || instances[0]?.id || '';
  /** @param {Event} e */
  const saveChannel = (e) => {
    e.preventDefault();
    runCh(api('POST', '/api/update-channel', { channel, instance: channel === 'instance' ? followed : '' }), 'Saved. Checking for an update…');
  };
  /** @param {Event} e */
  const addKey = (e) => {
    e.preventDefault();
    runKey(api('POST', '/api/dev-keys', { name: keyName.trim(), pem }), 'Trusted.').then((ok) => { if (ok) { setKeyName(''); setPem(''); } });
  };
  return html`
    <h3>Signature</h3>
    <${TrustFacts} t=${t} />
    <h3>Update source</h3>
    <form id="channel-form" class="row" onSubmit=${saveChannel}>
      <select id="channel" aria-label="Update from" value=${channel} onChange=${(/** @type {any} */ e) => setChannel(e.currentTarget.value)}>
        <option value="github">GitHub releases (default)</option>
        <option value="instance">the version a server hands out</option>
      </select>
      ${channel === 'instance' && html`<select id="channel-instance" aria-label="Server to follow" value=${followed}
        onChange=${(/** @type {any} */ e) => setFollow(e.currentTarget.value)}>
        ${instances.map((i) => html`<option value=${i.id}>${hostOf(i.url)}</option>`)}
      </select>`}
      <button type="submit">Save</button>
      <span id="channel-status" role="status">${chText}</span>
    </form>
    <h3>Trusted dev keys</h3>
    <p class="hint">Binaries signed with one of these cosign public keys run like project releases. Keys also load from the dev-keys folder of this app's config.</p>
    <ul id="dev-keys" class="list-rows">
      ${!t.dev_keys.length && html`<li class="empty">No dev key yet.</li>`}
      ${t.dev_keys.map((k) => html`<li class="list-row"><div class="main"><span class="name">${k.name}</span> <${Value} text=${k.fingerprint} label=${'fingerprint of ' + k.name} /></div>
        <div class="actions"><button type="button" class="btn-icon btn-ghost" aria-label=${'Remove ' + k.name} dangerouslySetInnerHTML=${icon('trash')}
        onClick=${() => window.grillConfirm('Remove the key ' + k.name + '?', 'Binaries it signed are refused from now on.', 'Remove')
          .then((yes) => { if (yes) runKey(api('POST', '/api/dev-keys/remove', { name: k.name })); })}></button></div></li>`)}
    </ul>
    <form id="key-form" class="stack-form" onSubmit=${addKey}>
      <label>Name <input id="key-name" required size="16" autocomplete="off" spellcheck=${false} pattern="[A-Za-z0-9][A-Za-z0-9._\\-]{0,63}"
        value=${keyName} onInput=${(/** @type {any} */ e) => setKeyName(e.currentTarget.value)} /></label>
      <label class="wide">Public key <textarea id="key-pem" required rows="4" spellcheck=${false} placeholder="-----BEGIN PUBLIC KEY-----"
        value=${pem} onInput=${(/** @type {any} */ e) => setPem(e.currentTarget.value)}></textarea></label>
      <button type="submit">Add the key</button>
    </form>
    <p><span id="key-status" role="status">${keyText}</span></p>`;
}

/** @param {StatusReply} d */
const currentInstance = (d) => {
  const instances = d.instances || [];
  const id = instances.some((i) => i.id === picked.value) ? picked.value : d.instance || instances[0]?.id || '';
  return instances.find((i) => i.id === id);
};

// MachineStrip is this computer's readiness in one line: Docker, the sandbox image, whether it can
// run the AI (003b-AC13).
function MachineStrip() {
  const d = /** @type {StatusReply} */ (data.value);
  const i = currentInstance(d);
  const st = i?.status;
  if (!st) return null;
  const img = st.image;
  const imageText = /** @type {Record<string, string | undefined>} */ ({
    '': 'waiting for the room server',
    pending: 'waiting for Docker',
    checking: 'checking…',
    pulling: 'pulling: ' + (img.progress || ''),
    building: 'building: ' + (img.progress || ''),
    ready: 'ready',
    failed: img.error,
  })[img.state] || img.state;
  return html`<div class="machine-strip" aria-label="This computer">
    <span>Docker <${Pill} id="docker" word=${st.docker.ok ? 'running ' + st.docker.version : st.docker.reason || 'not running'} look=${st.docker.ok ? 'ok' : 'failed'} /></span>
    <span class="strip-image">Sandbox image <${Pill} id="image" word=${imageText} look=${img.state === 'ready' ? 'ok' : img.state === 'failed' ? 'failed' : 'busy'} />
      ${img.state === 'ready' && img.ref && html`<${Value} text=${img.ref} label="sandbox image" />`}</span>
    <span>Running the AI <${Pill} id="can-host" word=${st.can_host ? 'available' : 'unavailable'} look=${st.can_host ? 'ok' : 'failed'} tip=${st.reason} /></span>
    <button type="button" id="recheck" class="btn-secondary" onClick=${() => api('POST', '/api/recheck', { instance: i.id }).then(reload).catch((err) => { banner.value = err.message; })}>Check again</button>
  </div>`;
}

/** @type {['cpus', 'memory', 'egress_cpus', 'egress_memory']} */
const limitKeys = ['cpus', 'memory', 'egress_cpus', 'egress_memory'];

// Machine holds what applies to the whole computer, one Save for all of it; Start at login applies
// at once (003b-AC14). The limits are stored per server: they show the first server's and save to
// every one.
function Machine() {
  const d = /** @type {StatusReply} */ (data.value);
  const instances = d.instances || [];
  const saved = {
    max_turns: String(d.concurrency?.max_turns ?? ''),
    retention: String(d.retention_days || 30),
    ...(instances[0]?.limits || { cpus: '', memory: '', egress_cpus: '', egress_memory: '' }),
  };
  const [draft, setDraft] = useState(/** @type {Record<string, string>} */ ({}));
  const [text, run] = useStatus();
  const [busy, setBusy] = useState(false);
  const v = { ...saved, ...draft };
  const changed = Object.keys(draft).filter((k) => draft[k] !== /** @type {Record<string, string>} */ (saved)[k]);
  /** @param {string} k */
  const field = (k) => ({ value: v[/** @type {keyof typeof v} */ (k)], onInput: (/** @type {any} */ e) => setDraft({ ...draft, [k]: e.currentTarget.value }) });
  /** @param {Event} e */
  const save = async (e) => {
    e.preventDefault();
    const limits = changed.some((k) => /** @type {string[]} */ (limitKeys).includes(k));
    const answering = instances.some((i) => (i.room_states || []).some((r) => r.running || r.queued));
    if (limits && answering && !await window.grillConfirm('Apply the limits now?', 'An answer is running: restarting its server connection fails it.', 'Apply now')) return;
    const l = Object.fromEntries(limitKeys.map((k) => [k, v[k]]));
    // Chrome fails a request in flight when Docker adds or removes the sandboxes' networks, as the
    // restart does (ERR_NETWORK_CHANGED): the limits may have landed all the same.
    /** @param {Instance} i */
    const post = (i) => api('POST', '/api/limits', { instance: i.id, ...l }).catch(async (err) => {
      if (!(err instanceof TypeError)) throw err;
      const now = (await api('GET', '/api/status')).instances?.find((/** @type {Instance} */ x) => x.id === i.id)?.limits;
      if (!now || limitKeys.some((k) => now[k] !== l[k])) throw err;
    });
    const steps = (async () => {
      if (changed.includes('max_turns')) await api('POST', '/api/max-turns', { max_turns: Number(v.max_turns) });
      if (changed.includes('retention')) await api('POST', '/api/retention', { days: Number(v.retention) });
      if (limits) for (const i of instances) await post(i);
    })();
    setBusy(true);
    if (await run(steps, limits ? 'Saved: the servers reconnect with the new limits.' : 'Saved.')) setDraft({});
    setBusy(false);
  };
  /** @param {string} id @param {string} k @param {string} label @param {string} hint */
  const limit = (id, k, label, hint) => html`<label for=${id}>${label}</label><div class="control"><input id=${id} size="6" autocomplete="off"
    spellcheck=${false} placeholder=${hint} ...${field(k)} /></div>`;
  return html`<h3 id="machine-title">This machine</h3>
    <form id="machine-form" class="settings-group" aria-labelledby="machine-title" onSubmit=${save}>
      ${d.concurrency && html`<label for="max-turns">Answers at once</label><div class="control">
        <input id="max-turns" type="number" inputmode="numeric" min="1" max="16" required ...${field('max_turns')} /><span class="hint">across every server and room</span></div>`}
      <label for="retention">Days of history</label><div class="control">
        <input id="retention" type="number" inputmode="numeric" min="1" max="3650" required ...${field('retention')} /><span class="hint">of the answers listed on Activity</span></div>
      <${Autostart} />
      ${instances.length ? html`
        ${limit('limit-cpus', 'cpus', 'Sandbox CPUs', '2')}${limit('limit-memory', 'memory', 'Sandbox memory', '4g')}
        ${limit('limit-egress-cpus', 'egress_cpus', 'Proxy CPUs', '0.5')}${limit('limit-egress-memory', 'egress_memory', 'Proxy memory', '256m')}
        <p class="hint">Each sandbox and its web proxy run within these; 0 means no limit. Saving them reconnects every server.</p>` : ''}
      <div class="settings-foot">
        <span id="machine-status" role="status">${text}</span>
        ${changed.length > 0 && html`<span class="dirty">${changed.length === 1 ? '1 unsaved change' : changed.length + ' unsaved changes'}</span>`}
        <button type="submit" disabled=${!changed.length || busy}>Save</button>
      </div>
    </form>`;
}

// Autostart is a toggle: it applies at once and says so in a toast (docs/design.md §5).
function Autostart() {
  const on = data.value?.autostart;
  const [busy, setBusy] = useState(false);
  const [text, setText] = useState('');
  if (on === undefined) return null;
  /** @param {any} e */
  const change = (e) => {
    const want = e.currentTarget.checked;
    setBusy(true);
    setText('');
    api('POST', '/api/autostart', { enabled: want }).then(reload)
      .then(() => window.grillToast(want ? 'Starts at your next login' : 'No longer starts at login'))
      .catch((err) => setText(sentence(err.message))).finally(() => setBusy(false));
  };
  return html`<label class="full"><input id="autostart" type="checkbox" checked=${on} disabled=${busy} onChange=${change} /> Start at login${' '}<span id="autostart-status" class="bad" role="status">${text}</span></label>`;
}

// ServerSettings is one server's settings, its picker at the top: its AI account and model limits
// (one Save), approve-all (at once), then its websites (003b-AC15, 003b-AC16).
function ServerSettings() {
  const d = /** @type {StatusReply} */ (data.value);
  const inst = currentInstance(d);
  if (!inst) return html`<h3>Per server</h3><p class="hint">Server settings show once you add a room.</p>`;
  // Keyed: a draft belongs to its server and never follows the picker to another one.
  return html`<h3 id="server-settings-title">Per server</h3>
    <${ServerForm} key=${inst.id} inst=${inst} />
    ${inst.status && html`<${Egress} key=${inst.id} i=${inst} st=${inst.status} />`}`;
}

/** @param {{inst: Instance}} p */
function ServerForm({ inst }) {
  const d = /** @type {StatusReply} */ (data.value);
  const instances = d.instances || [];
  const st = inst.status;
  // draft holds only the fields the host changed: the others follow the server's values.
  const [draft, setDraft] = useState(/** @type {Record<string, string>} */ ({}));
  const [busy, setBusy] = useState(false);
  const [text, run] = useStatus();
  const saved = { max_model: st?.settings.max_model || '', max_effort: st?.settings.max_effort || '', account: inst.account };
  const v = { ...saved, ...draft };
  const changed = /** @type {const} */ (['account', 'max_model', 'max_effort']).filter((k) => k in draft && draft[k] !== saved[k]);
  /** @param {string} k */
  const pick = (k) => (/** @type {any} */ e) => setDraft({ ...draft, [k]: e.currentTarget.value });
  /** @param {Event} e */
  const save = async (e) => {
    e.preventDefault();
    const steps = (async () => {
      if (changed.includes('account')) await api('POST', '/api/instance-account', { instance: inst.id, room: '', account: v.account });
      if (st && (changed.includes('max_model') || changed.includes('max_effort'))) {
        await api('POST', '/api/settings', { instance: inst.id, max_model: v.max_model, max_effort: v.max_effort, approve_all: st.settings.approve_all });
      }
    })();
    setBusy(true);
    if (await run(steps, 'Saved.')) setDraft({});
    setBusy(false);
  };
  /** @param {any} e */
  const approveAll = (e) => {
    const on = e.currentTarget.checked;
    api('POST', '/api/settings', { instance: inst.id, max_model: saved.max_model, max_effort: saved.max_effort, approve_all: on }).then(reload)
      .then(() => window.grillToast(on ? 'New AI personas and changes are approved at once' : 'New AI personas and changes wait for your approval'))
      .catch((err) => { banner.value = err.message; });
  };
  return html`<form id="settings-form" class="settings-group" aria-labelledby="server-settings-title" onSubmit=${save}>
      <label for="settings-instance">Server</label><div class="control">
        <select id="settings-instance" value=${inst.id} disabled=${instances.length < 2}
          onChange=${(/** @type {any} */ e) => { picked.value = e.currentTarget.value; }}>
          ${instances.map((i) => html`<option value=${i.id}>${hostOf(i.url)}</option>`)}
        </select></div>
      ${d.accounts && html`<label for="server-account">AI account</label><div class="control">
        <${AccountSelect} id="server-account" label="AI account" value=${v.account} onPick=${(/** @type {string} */ a) => setDraft({ ...draft, account: a })} />
        <span class="hint">for its rooms that do not pick their own</span></div>`}
      ${st ? html`<label for="max-model">Max model</label><div class="control"><select id="max-model" value=${v.max_model} onChange=${pick('max_model')}>
          <option value="">no cap</option>${d.models.map((m) => html`<option value=${m}>${m}</option>`)}</select></div>
        <label for="max-effort">Max effort</label><div class="control"><select id="max-effort" value=${v.max_effort} onChange=${pick('max_effort')}>
          <option value="">no cap</option>${d.efforts.map((m) => html`<option value=${m}>${m}</option>`)}</select></div>
        <p class="hint">An AI persona asking for more runs at this limit until you approve it in the room.</p>
        <label class="full"><input id="approve-all" type="checkbox" checked=${st.settings.approve_all} onChange=${approveAll} /> Approve every new AI persona and change</label>`
      : html`<p class="hint">The app for ${hostOf(inst.url)} is ${inst.state}${inst.error ? ': ' + inst.error : ''}. Its limits show once it runs.</p>`}
      <div class="settings-foot">
        <span id="settings-status" role="status">${text}</span>
        ${changed.length > 0 && html`<span class="dirty">${changed.length === 1 ? '1 unsaved change' : changed.length + ' unsaved changes'}</span>`}
        <button type="submit" disabled=${!changed.length || busy}>Save</button>
      </div>
    </form>`;
}

/**
 * hitsFor sums the hits of the hosts an allowlist entry covers: a leading dot also covers the
 * subdomains.
 * @param {Record<string, number>} hits @param {string} entry
 */
const hitsFor = (hits, entry) => Object.entries(hits).reduce((n, [h, c]) => {
  const e = entry.toLowerCase();
  const host = h.toLowerCase();
  return n + (host === e.replace(/^\./, '') || (e.startsWith('.') && host.endsWith(e)) ? c : 0);
}, 0);

// Egress is the server's allowlist as a table with its hits, and the requests its proxy refused
// (002-AC34 to 002-AC36, 003b-AC16).
/** @param {{i: Instance, st: import('./api.js').Status}} p */
function Egress({ i, st }) {
  const [domain, setDomain] = useState('');
  const [text, run] = useStatus();
  const [log, setLog] = useState(/** @type {{denied: import('./api.js').Denied[], hits: Record<string, number> | null} | null} */ (null));
  const [logErr, setLogErr] = useState('');
  useEffect(() => {
    let timer = 0;
    // live stops a load already in flight from starting the next one once the server changes.
    let live = true;
    const load = () => api('GET', '/api/denied?instance=' + encodeURIComponent(i.id))
      .then((r) => { if (live) { setLog({ denied: r.denied, hits: r.hits || null }); setLogErr(''); } })
      .catch((err) => { if (live) setLogErr(err.message); })
      .finally(() => { if (live && !stopped.value) timer = setTimeout(load, 5000); });
    load();
    return () => { live = false; clearTimeout(timer); };
  }, [i.id]);
  /** @param {string[]} list @param {string} ok */
  const save = (list, ok) => run(api('POST', '/api/allowlist', { instance: i.id, allowlist: list }), ok);
  /** @param {Event} e */
  const add = (e) => {
    e.preventDefault();
    const dom = domain.trim();
    save([...st.allowlist, dom], 'Saved.').then((ok) => { if (ok) setDomain(''); });
  };
  const hits = log?.hits;
  return html`
    <h4 id="allowlist-title">Websites the AI may reach</h4>
    <p class="hint">The sandbox reaches these domains, and nothing else, over HTTPS only. A leading dot, as in .example.com, also allows every subdomain. Anthropic's hosts stay blocked whatever the list says.</p>
    ${!st.allowlist.length ? html`<p class="empty" id="allowlist-empty">No domain: the sandbox reaches nothing.</p>`
      : html`<table id="allowlist" class="allowlist" aria-labelledby="allowlist-title">
      <thead><tr><th scope="col">Domain</th><th scope="col" class="num"><span tabindex="0" data-tip="Requests let through, from the running sandboxes' recent proxy log. A restart resets them.">Hits</span></th><th scope="col"><span class="sr-only">Remove</span></th></tr></thead>
      <tbody>${st.allowlist.map((dom) => html`<tr>
        <td><code class="domain">${dom}</code></td>
        <td class="num">${hits ? hitsFor(hits, dom) : '–'}</td>
        <td class="act"><button type="button" class="btn-icon btn-ghost" aria-label=${'Remove ' + dom} dangerouslySetInnerHTML=${icon('trash')}
          onClick=${() => save(st.allowlist.filter((x) => x !== dom), 'Saved.')}></button></td>
      </tr>`)}</tbody></table>`}
    <form id="allowlist-form" class="add-room" onSubmit=${add}>
      <input id="allowlist-add" required aria-label="Domain" placeholder="example.com" autocomplete="off" spellcheck=${false}
        value=${domain} onInput=${(/** @type {any} */ e) => setDomain(e.currentTarget.value)} />
      <button type="submit">Add</button>
    </form>
    <p><span id="allowlist-status" role="status">${text}</span></p>
    <h4 id="denied-title">Blocked websites</h4>
    <p class="hint">What the proxy refused in this server's sandboxes, newest first.</p>
    ${log && log.denied.length ? html`<div class="scroll"><table id="denied" aria-labelledby="denied-title">
      <thead><tr><th scope="col">Time</th><th scope="col">Host</th></tr></thead>
      <tbody>${log.denied.map((x) => html`<tr><td>${window.grillDate(x.time)}</td><td>${x.host}</td></tr>`)}</tbody>
    </table></div>` : html`<p id="denied-empty">${logErr || 'None yet.'}</p>`}`;
}

// Settings: this computer's state, then its own settings, then one server's (003b-AC13 to AC17).
function Settings() {
  return html`<${Section} id="settings" title="Settings">
    <${MachineStrip} />
    <div class="settings-cols">
      <div class="settings-col"><${Machine} /></div>
      <div class="settings-col"><${ServerSettings} /></div>
    </div>
  <//>`;
}

// The log levels the filter keeps: a console line names its level in upper case, a JSON one in "level".
const levels = /** @type {Record<string, RegExp | null>} */ ({ all: null, warn: /\b(WARN|ERROR|FATAL)\b|"level":"(warn|error|fatal)"/, error: /\b(ERROR|FATAL)\b|"level":"(error|fatal)"/ });

function Logs() {
  const ref = useRef(/** @type {HTMLPreElement | null} */ (null));
  const [level, setLevel] = useState('all');
  const re = levels[level];
  const lines = (data.value?.logs || []).filter((l) => !re || re.test(l));
  const atBottom = useRef(true);
  const el = ref.current;
  if (el) atBottom.current = el.scrollTop + el.clientHeight >= el.scrollHeight - 20;
  useEffect(() => {
    if (ref.current && atBottom.current) ref.current.scrollTop = ref.current.scrollHeight;
  });
  return html`<${Section} id="logs" title="Logs">
    <p class="row"><select id="log-level" aria-label="Log level" value=${level} onChange=${(/** @type {any} */ e) => setLevel(e.currentTarget.value)}>
      <option value="all">Every line</option><option value="warn">Warnings and errors</option><option value="error">Errors</option>
    </select></p>
    <pre id="logs" ref=${ref}>${lines.join('\n')}</pre><//>`;
}

function App() {
  const d = data.value;
  const live = d && !stopped.value;
  return html`
    <${Header} />
    ${banner.value && html`<p id="banner" role="alert">${sentence(banner.value)}</p>`}
    ${live && html`<${RelaxBanner} />`}
    ${live && html`<main id="main" class=${tab.value === 'monitoring' ? 'page-wide' : 'page'}>
      <${TabBar} />
      <${Panel} id="rooms"><${Rooms} /><//>
      <${Panel} id="accounts"><${Accounts} /><//>
      <${Panel} id="settings"><${Settings} /><//>
      <${Panel} id="monitoring">${d.instances && html`<${Monitoring} instances=${d.instances} />`}<${Logs} /><//>
      <${Panel} id="update"><${Section} id="update" title="Update"><${UpdateOffer} /><${Trust} /><//><//>
    </main>`}`;
}

render(html`<${App} />`, /** @type {HTMLElement} */ (document.getElementById('app')));

startSession().then(poll).catch((err) => { banner.value = err.message; });
