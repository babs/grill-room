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

/** @type {import('@preact/signals').Signal<StatusReply | null>} */
const data = signal(null);
const banner = signal('');
const stopped = signal(false);
// picked is the instance the per-instance settings show; empty follows the page's current one.
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
const when = (t) => (t && !t.startsWith('0001') ? new Date(t).toLocaleString() : 'never');
/** @param {number} n @param {string} word */
const plural = (n, word) => n + ' ' + word + (n === 1 ? '' : 's');

/** @param {{text: string, cls?: string}} p */
const Badge = ({ text, cls = '' }) => html`<span class=${'badge ' + cls}>${text}</span>`;

/** @param {{text: string}} p */
const Said = ({ text }) => html`<span class="said" role="status">${text}</span>`;

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
  return html`
    <header class="app-header">
      <img src="/static/logo.svg" alt="" class="logo" width="32" height="32"/>
      <h1>grill-room AI host</h1>
      <span id="host" class="muted">${d ? d.status.host + ' · ' + d.status.version : ''}</span>
      ${t && html`<span id="self-sig" class=${'badge ' + cls}>${sig}</span>`}
      <span class="spacer"></span>
      <span id="state" class="muted">${stopped.value ? '○ Stopped' : d ? '● Running' : ''}</span>
      <button type="button" id="stop" class="btn-secondary" onClick=${stop} disabled=${stopped.value}>Stop</button>
    </header>`;
}

// The page's tabs (005-AC17), each a deep link (#accounts). Rooms comes first to the eye of whoever
// pairs: the daemon opens its page there.
// Labels in plain words (005-AC42); the ids, and so the deep links, stay.
const tabs = [['rooms', 'Rooms'], ['instances', 'Servers'], ['accounts', 'Accounts'], ['settings', 'Settings'],
  ['monitoring', 'Activity'], ['update', 'Update']];
/** @param {string} hash */
const tabOf = (hash) => tabs.find(([id]) => '#' + id === hash)?.[0] || 'rooms';
const tab = signal(tabOf(location.hash));
addEventListener('hashchange', () => { tab.value = tabOf(location.hash); });

// Each alert kind belongs to the tab that resolves it: logins near expiry, unsigned workers, full
// contexts and stuck queues (003-AC29).
const alertTab = /** @type {Record<string, string>} */ ({ account: 'accounts', unsigned: 'instances', context: 'rooms', queue: 'settings' });
/** @param {string} id */
const alertsOf = (id) => (data.value?.alerts || []).filter((a) => (alertTab[a.kind] || 'monitoring') === id);

// TabBar stays pinned at the top; a tab with alerts, or an update waiting, shows their count.
function TabBar() {
  return html`<nav id="tabs" aria-label="Sections">${tabs.map(([id, name]) => {
    const n = alertsOf(id).length + (id === 'update' && data.value?.status.update?.available ? 1 : 0);
    return html`<a key=${id} id=${'tab-' + id} href=${'#' + id} aria-current=${tab.value === id ? 'page' : undefined}>${name}${n
      ? html` <span class="tab-badge" title=${n + (n === 1 ? ' alert' : ' alerts')}>${n}</span>` : ''}</a>`;
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
    Unsigned worker binaries are accepted for: ${relaxed.map((i) => i.url).join(', ')}. Switch it off on the instance once you are done testing.
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
    Version <strong id="update-version">${u.available}</strong> of the supervisor is available from <span id="update-from">${u.from || 'the room server'}</span>.
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

// Room pairs this machine with a room link, then waits for the approval in the room. "Add a room"
// stays there once a room is connected: the link goes to the supervisor as a second launch's does
// (008-AC15), and the status then shows that room's pairing.
function Room() {
  const s = /** @type {StatusReply} */ (data.value).status;
  const [url, setURL] = useState('');
  const [err, setErr] = useState('');
  let text = pairingText[s.pairing] ?? s.pairing;
  if (s.pairing === 'paired') text += ' ' + ({ connected: 'Connected.', connecting: 'Connecting…', offline: 'Offline.' }[s.connection] || '');
  /** @param {Event} e */
  const pair = (e) => {
    e.preventDefault();
    setErr('');
    api('POST', '/api/room', { url }).then(() => { setURL(''); return reload(); }).catch((x) => setErr(x.message));
  };
  return html`<${Section} id="room" title="Room">
    <p id="room">${s.room || 'No room yet: paste the room link below.'}</p>
    <p id="pairing" role="status">${text + (s.error ? ' ' + s.error : '')}</p>
    ${s.pairing === 'pending' && /^https?:\/\//.test(s.approve_url || '') && html`<p id="approve">
      Check that the room page shows the code <strong id="code">${s.code}</strong>, then
      <a id="approve-link" href=${s.approve_url} target="_blank" rel="noopener noreferrer">approve in the room</a>.
    </p>`}
    <form id="room-form" class="row" onSubmit=${pair}>
      <label>Room link <input id="room-url" type="url" required placeholder="https://.../r/..." value=${url}
        onInput=${(/** @type {any} */ e) => setURL(e.currentTarget.value)} /></label>
      <button type="submit">Add a room</button>
    </form>
    <p class="hint">Any link inside the room works: the room page or its documents.</p>
    <p id="room-error" class="bad" role="alert">${err}</p>
  <//>`;
}

/**
 * sigBadge is a worker binary's signature state (003-AC2, 003-AC4).
 * @param {Instance} i
 */
function sigBadge(i) {
  const t = i.trust || '';
  if (i.unsigned) return html`<${Badge} text="unsigned" cls="danger" />`;
  if (i.state === 'refused') return html`<${Badge} text="refused" cls="bad" />`;
  if (t.startsWith('signed by the project')) return html`<${Badge} text="project-signed" cls="ok" />`;
  if (t.startsWith('signed with the dev key')) return html`<${Badge} text="dev key" cls="ok" />`;
  if (t) return html`<${Badge} text="the supervisor's binary" />`;
  return null;
}

/**
 * accountSelect picks an account for an instance, or for one of its rooms (003-AC13); a room's
 * first option goes back to its instance's account.
 * @param {{label: string, value: string, roomDefault?: string, onPick: (v: string) => void}} p
 */
function AccountSelect({ label, value, roomDefault, onPick }) {
  const accounts = data.value?.accounts || [];
  return html`<select aria-label=${label} value=${value} onChange=${(/** @type {any} */ e) => onPick(e.currentTarget.value)}>
    ${roomDefault !== undefined && html`<option value="">${"the instance's (" + roomDefault + ')'}</option>`}
    ${accounts.map((a) => html`<option value=${a.name}>${a.name}</option>`)}
  </select>`;
}

/** @param {string} instance @param {string} room @param {string} account */
const setAccount = (instance, room, account) =>
  api('POST', '/api/instance-account', { instance, room, account }).then(reload).catch((err) => { banner.value = err.message; });

/** @param {number} s */
const uptime = (s) => (s < 120 ? s + ' s' : s < 7200 ? Math.round(s / 60) + ' min' : Math.round(s / 3600) + ' h');

/**
 * cutOff stops a server's worker from reaching Claude, once confirmed, or lets it again (003-AC15).
 * @param {Instance} i
 */
function cutOff(i) {
  const go = () => api('POST', '/api/cut-off', { instance: i.id, cut_off: !i.cut_off }).then(reload).catch((err) => { banner.value = err.message; });
  if (i.cut_off) return go();
  return window.grillConfirm('Cut ' + i.url + ' off from Claude?', 'Its running answers end and none starts until you let it again.', 'Cut it off')
    .then((yes) => { if (yes) go(); });
}

/** @param {{i: Instance, current: boolean}} p */
function InstanceCard({ i, current }) {
  const [host, setHost] = useState('');
  const [relaxText, run] = useStatus();
  /** @param {boolean} on @param {string} h */
  const relax = (on, h) => run(api('POST', '/api/relax', { instance: i.id, enabled: on, host: h }), on ? 'Unsigned binaries accepted.' : '');
  let text = ' · ' + i.state;
  if (i.version) text += ', ' + i.version;
  if (i.connection) text += ', ' + i.connection;
  if (i.restarts) text += ', ' + plural(i.restarts, 'restart');
  if (i.error) text += ': ' + i.error;
  const m = i.metrics;
  const accounts = data.value?.accounts;
  return html`<li class=${'instance' + (current ? ' current' : '')}>
    <div class="row"><code>${i.url}</code>${text} ${sigBadge(i)}</div>
    ${i.trust && html`<div class="hint">Worker binary: ${i.trust}</div>`}
    ${i.state === 'running' && html`<div class="hint">Worker pid ${i.pid}, protocol ${i.proto}${m ? ', up ' + uptime(m.uptime_seconds) + ', ' +
      plural(m.turns, 'turn') + (m.failed_turns ? ' (' + m.failed_turns + ' failed)' : '') : ''}</div>`}
    ${i.accept_unsigned ? html`<div class="row relax">
        <${Badge} text="accepts unsigned binaries" cls="danger" />
        <button type="button" onClick=${() => relax(false, '')}>Stop accepting unsigned binaries</button>
      </div>` : html`<form class="row relax" onSubmit=${(/** @type {Event} */ e) => {
        e.preventDefault();
        window.grillConfirm('Accept unsigned binaries from ' + host + '?', 'This server could then run any program on this computer. Switch it off once you are done testing.', 'Accept')
          .then((yes) => { if (yes) relax(true, host).then((ok) => { if (ok) setHost(''); }); });
      }}>
        <label>To accept its unsigned binaries, type its host <input class="relax-host" required size="24" autocomplete="off" spellcheck=${false}
          value=${host} onInput=${(/** @type {any} */ e) => setHost(e.currentTarget.value)} /></label>
        <button type="submit">Accept unsigned binaries</button>
      </form>`}
    <${Said} text=${relaxText} />
    ${accounts && html`<div class="row account-map">
      Claude account <${AccountSelect} label=${'Claude account of ' + i.url} value=${i.account} onPick=${(/** @type {string} */ a) => setAccount(i.id, '', a)} />
      ${plural(i.requests, 'request')} through the proxies
      <button type="button" class=${i.cut_off ? 'btn-secondary' : 'btn-danger-outline'} onClick=${() => cutOff(i)}>${i.cut_off ? 'Let it use Claude again' : 'Cut it off from Claude'}</button>
      ${i.cut_off && html`<${Badge} text="cut off: its turns end and none starts" cls="danger" />`}
    </div>`}
  </li>`;
}

function Instances() {
  const d = /** @type {StatusReply} */ (data.value);
  if (!d.instances) return null;
  return html`<${Section} id="instances" title="Servers">
    <p class="hint">One worker per room server, running the version that server hands out. The Rooms tab shows the last one you paired with.</p>
    <ul id="instances">${d.instances.map((i) => html`<${InstanceCard} key=${i.id} i=${i} current=${i.id === d.instance} />`)}</ul>
  <//>`;
}

/**
 * roomText says how a worker serves a room: its connection, whether it hosts the agent, its turns
 * running or waiting for a slot under the global cap, and the room's parallel mode when this
 * machine hosts it (003-AC18, 003-AC20).
 * @param {import('./api.js').RoomStatus | undefined} st
 */
function roomText(st) {
  if (!st) return 'not served';
  const parts = [st.pairing === 'pending' ? 'waiting for your approval in the room' : st.connection];
  if (st.host === true) parts.push('runs the AI', 'parallel answers ' + (st.parallel_turns ? 'on' : 'off'));
  if (st.host === false) parts.push('another computer runs the AI');
  if (st.running) parts.push(st.running + ' running');
  if (st.queued) parts.push(st.queued + ' waiting for a slot');
  return parts.join(', ');
}

function Rooms() {
  const d = /** @type {StatusReply} */ (data.value);
  const c = d.concurrency;
  if (!d.instances) return null;
  const secs = (/** @type {number} */ ms) => (ms / 1000).toFixed(1) + ' s';
  let conc = '';
  if (c) {
    conc = 'Now ' + c.running.length + ' running, ' + c.queued.length + ' waiting. Since start: ' + plural(c.granted, 'turn');
    if (c.waited) conc += ', ' + c.waited + ' waited for a slot, ' + secs(c.wait_total_ms / c.waited) + ' on average, ' + secs(c.wait_max_ms) + ' at worst';
    conc += '.';
  }
  return html`<${Section} id="rooms" title="Rooms">
    <p class="hint" id="concurrency">${conc}</p>
    ${d.instances.map((i) => html`<div class="room-group">
      <h3><code>${i.url}</code></h3>
      <ul class="rooms">${i.rooms.map((r) => {
        const st = (i.room_states || []).find((x) => x.room === r);
        // The room's name when its server sends one (005-AC42), the link otherwise.
        return html`<li class="card room-card">
          <strong class="room-name">${st?.name || r}</strong> <span class="room-state">${roomText(st)}</span>
          ${st?.name && html`<div class="hint"><code class="room-link">${r}</code></div>`}
          ${d.accounts && html`<div class="row">Claude account <${AccountSelect} label=${'Claude account of ' + r} value=${(i.room_accounts || {})[r] || ''} roomDefault=${i.account}
            onPick=${(/** @type {string} */ a) => setAccount(i.id, r, a)} /></div>`}
        </li>`;
      })}</ul>
    </div>`)}
  <//>`;
}

const stateText = /** @type {Record<string, [string, string]>} */ ({
  valid: ['valid', 'ok'], expired: ['expired', 'bad'], refused: ['refused', 'bad'], unknown: ['not checked yet', ''], error: ['unreadable', 'bad'],
});

/** @param {{a: Account, onError: (t: string) => void}} p */
function AccountItem({ a, onError }) {
  const [text, cls] = stateText[a.state] || [a.state, ''];
  const kind = a.kind === 'setup_token' ? 'setup token, in the ' + (a.storage === 'keyring' ? 'OS keyring' : 'owner-only file')
    : 'config dir ' + (a.display_dir || a.config_dir || '~/.claude');
  const total = Object.values(a.requests || {}).reduce((n, x) => n + x, 0);
  const info = (a.expires_at ? (a.expiry_estimated ? 'expires about ' : 'access token expires ') + when(a.expires_at) + ' · ' : '') +
    'last use ' + when(a.last_use) + ' · ' + plural(total, 'request') + (a.checked_at ? ' · checked ' + when(a.checked_at) : '') +
    (a.detail ? ' · ' + a.detail : '');
  const remove = () => {
    window.grillConfirm('Remove the Claude account ' + a.name + '?', 'Rooms that use it stop reaching Claude until they get another account.', 'Remove')
      .then((yes) => { if (yes) api('POST', '/api/accounts/remove', { name: a.name }).then(reload).catch((err) => onError(err.message)); });
  };
  return html`<li>
    <code>${a.name}</code> · ${kind} <${Badge} text=${text} cls=${cls} />
    <div class="hint">${info}</div>
    ${a.kind === 'setup_token' && html`<button type="button" aria-label=${'Check ' + a.name + ' now'}
      onClick=${() => api('POST', '/api/accounts/probe', { name: a.name }).then(reload).catch((err) => onError(err.message))}>Check now</button> `}
    <button type="button" aria-label=${'Remove ' + a.name} onClick=${remove}>Remove</button>
  </li>`;
}

// Accounts lists the Claude accounts with their status, expiry and use, and adds one: a setup
// token is typed once, then never shown again (003-AC12).
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
  return html`<${Section} id="accounts" title="Claude accounts">
    <p class="hint">Each account has its own proxy in this supervisor: the workers and their sandboxes get its address, never a token. Rooms see none of this page.</p>
    <ul id="accounts">${accounts.map((a) => html`<${AccountItem} key=${a.name} a=${a} onError=${setListErr} />`)}</ul>
    <p class="bad" role="alert">${listErr}</p>
    <form id="account-form" class="row" onSubmit=${add}>
      <label>Name <input id="account-name" required size="16" autocomplete="off" spellcheck=${false} pattern="[A-Za-z0-9][A-Za-z0-9._\\-]{0,63}"
        value=${name} onInput=${on(setName)} /></label>
      <label>Kind <select id="account-kind" value=${kind} onChange=${on(setKind)}>
        <option value="config_dir">a Claude Code config dir</option>
        <option value="setup_token">a setup token</option>
      </select></label>
      ${kind === 'config_dir'
        ? html`<label>Config dir <input id="account-dir" size="30" autocomplete="off" spellcheck=${false} placeholder="~/.claude-work, empty for ~/.claude"
            value=${dir} onInput=${on(setDir)} /></label>`
        : html`<label>Token <input id="account-token" type="password" required size="30" autocomplete="off" spellcheck=${false}
            placeholder="from claude setup-token" value=${tok} onInput=${on(setTok)} /></label>`}
      <button type="submit">Add the account</button>
      <span id="account-status" role="status">${text}</span>
    </form>
    <p class="hint">A config dir is read only: Claude Code, run by you, refreshes its login. A setup token goes to the OS keyring, or to a file only you can read; it is not shown again.</p>
  <//>`;
}

// NumberForm saves one number setting: the global cap, or the history's retention.
/** @param {{id: string, label: string, value: number, min: number, max: number, button: string, path: string, field: string}} p */
function NumberForm({ id, label, value, min, max, button, path, field }) {
  const [v, setV] = useState(String(value));
  const [text, run] = useStatus();
  const focused = useRef(false);
  // The server's value shows, unless the host is typing another.
  useEffect(() => { if (!focused.current) setV(String(value)); }, [value]);
  /** @param {Event} e */
  const save = (e) => {
    e.preventDefault();
    run(api('POST', path, { [field]: Number(v) }), 'Saved.');
  };
  return html`<form id=${id + '-form'} class="row" onSubmit=${save}>
    <label>${label} <input id=${id} type="number" min=${min} max=${max} required value=${v}
      onFocus=${() => { focused.current = true; }} onBlur=${() => { focused.current = false; }}
      onInput=${(/** @type {any} */ e) => setV(e.currentTarget.value)} /></label>
    <button type="submit">${button}</button>
    <span id=${id + '-status'} role="status">${text}</span>
  </form>`;
}

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
      .then(() => setText(want ? 'Starts at your next login.' : 'Removed.'))
      .catch((err) => setText(err.message)).finally(() => setBusy(false));
  };
  return html`<p class="row"><label><input id="autostart" type="checkbox" checked=${on} disabled=${busy} onChange=${change} /> Start at login</label>
    <span id="autostart-status" role="status">${text}</span></p>`;
}

// Trust shows the supervisor's signature and update channel, and the trusted dev keys (003-AC4,
// 003-AC5).
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
  const [sig] = selfText[t.self.state] || [t.self.state];
  const r = t.root;
  let root = /** @type {Record<string, string>} */ ({ tuf: 'refreshed through TUF', disk: 'last good copy on disk', embedded: 'embedded in this binary' })[r.source] || 'not loaded yet';
  if (r.source && r.at && !r.at.startsWith('0001')) root += ', ' + new Date(r.at).toLocaleString();
  if (r.error) root += ' (TUF unreachable: ' + r.error + ')';
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
    <h3>Signatures and updates</h3>
    <dl>
      <dt>This supervisor</dt><dd id="self-detail" class=${t.self.state === 'unsigned' ? 'bad' : 'ok'}>${sig + (t.self.detail ? ': ' + t.self.detail : '')}</dd>
      <dt>Trusted root</dt><dd id="root">${root}</dd>
      <dt>Last update check</dt><dd id="checked">${(t.checked && !t.checked.startsWith('0001') ? new Date(t.checked).toLocaleString() : 'not yet') +
        (t.check_error ? ': ' + t.check_error : '')}</dd>
    </dl>
    <form id="channel-form" class="row" onSubmit=${saveChannel}>
      <label>Update from <select id="channel" value=${channel} onChange=${(/** @type {any} */ e) => setChannel(e.currentTarget.value)}>
        <option value="github">GitHub releases (default)</option>
        <option value="instance">the version an instance serves</option>
      </select></label>
      ${channel === 'instance' && html`<select id="channel-instance" aria-label="Instance to follow" value=${followed}
        onChange=${(/** @type {any} */ e) => setFollow(e.currentTarget.value)}>
        ${instances.map((i) => html`<option value=${i.id}>${i.url}</option>`)}
      </select>`}
      <button type="submit">Save</button>
      <span id="channel-status" role="status">${chText}</span>
    </form>
    <h3>Trusted dev keys</h3>
    <p class="hint">Worker binaries signed with one of these cosign public keys run like project releases. Keys also load from the dev-keys folder of the supervisor's config.</p>
    <ul id="dev-keys">
      ${!t.dev_keys.length && html`<li>None.</li>`}
      ${t.dev_keys.map((k) => html`<li><code>${k.name}</code> · ${k.fingerprint} <button type="button" aria-label=${'Remove ' + k.name}
        onClick=${() => window.grillConfirm('Remove the key ' + k.name + '?', 'Worker binaries it signed are refused from now on.', 'Remove')
          .then((yes) => { if (yes) runKey(api('POST', '/api/dev-keys/remove', { name: k.name })); })}>Remove</button></li>`)}
    </ul>
    <form id="key-form" class="row" onSubmit=${addKey}>
      <label>Name <input id="key-name" required size="16" autocomplete="off" spellcheck=${false} pattern="[A-Za-z0-9][A-Za-z0-9._\\-]{0,63}"
        value=${keyName} onInput=${(/** @type {any} */ e) => setKeyName(e.currentTarget.value)} /></label>
      <label>Public key <textarea id="key-pem" required rows="4" spellcheck=${false} placeholder="-----BEGIN PUBLIC KEY-----"
        value=${pem} onInput=${(/** @type {any} */ e) => setPem(e.currentTarget.value)}></textarea></label>
      <button type="submit">Add the key</button>
      <span id="key-status" role="status">${keyText}</span>
    </form>`;
}

/** @param {{ok: boolean, text: string, id: string}} p */
const Mark = ({ ok, text, id }) => html`<dd id=${id} class=${ok ? 'ok' : 'bad'}>${text}</dd>`;

// Machine is the instance worker's readiness: Docker, the sandbox image, hosting.
/** @param {{i: Instance, st: import('./api.js').Status}} p */
function Machine({ i, st }) {
  const img = st.image;
  const imageText = /** @type {Record<string, string | undefined>} */ ({
    '': 'waiting for the room server',
    pending: 'waiting for Docker',
    checking: 'checking…',
    pulling: "pulling from the room server's registry: " + (img.progress || ''),
    building: 'building on this machine: ' + (img.progress || ''),
    ready: 'ready (' + img.ref + ')',
    failed: img.error,
  })[img.state];
  return html`
    <h3>This computer</h3>
    <dl>
      <dt>Docker</dt><${Mark} id="docker" ok=${st.docker.ok} text=${st.docker.ok ? 'running (' + st.docker.version + ')' : st.docker.reason || ''} />
      <dt>Sandbox image</dt><${Mark} id="image" ok=${img.state === 'ready'} text=${imageText || img.state} />
      <dt>Running the AI</dt><${Mark} id="can-host" ok=${st.can_host} text=${st.can_host ? 'available' : 'unavailable: ' + st.reason} />
    </dl>
    <button type="button" id="recheck" onClick=${() => api('POST', '/api/recheck', { instance: i.id }).then(reload).catch((err) => { banner.value = err.message; })}>Check again</button>`;
}

// Cap is the instance's host cap and approval mode (002-AC19, 002-AC21).
/** @param {{i: Instance, st: import('./api.js').Status}} p */
function Cap({ i, st }) {
  const d = /** @type {StatusReply} */ (data.value);
  // The worker's settings show until the host changes one; saved, they follow the worker again.
  const [draft, setSet] = useState(/** @type {import('./api.js').Settings | null} */ (null));
  const set = draft || st.settings;
  const [text, run] = useStatus();
  /** @param {Event} e */
  const save = (e) => {
    e.preventDefault();
    run(api('POST', '/api/settings', { instance: i.id, ...set }), 'Saved.').then((ok) => { if (ok) setSet(null); });
  };
  /** @param {string} k */
  const pick = (k) => (/** @type {any} */ e) => setSet({ ...set, [k]: e.currentTarget.value });
  return html`
    <h3>Limit on models</h3>
    <p class="hint">A persona asking for more runs at this limit until you approve it in the room.</p>
    <form id="settings-form" class="row" onSubmit=${save}>
      <label>Max model <select id="max-model" value=${set.max_model} onChange=${pick('max_model')}>
        <option value="">no cap</option>${d.models.map((m) => html`<option value=${m}>${m}</option>`)}</select></label>
      <label>Max effort <select id="max-effort" value=${set.max_effort} onChange=${pick('max_effort')}>
        <option value="">no cap</option>${d.efforts.map((m) => html`<option value=${m}>${m}</option>`)}</select></label>
      <label><input id="approve-all" type="checkbox" checked=${set.approve_all}
        onChange=${(/** @type {any} */ e) => setSet({ ...set, approve_all: e.currentTarget.checked })} /> Approve every new persona and persona change</label>
      <button type="submit">Save</button>
      <span id="settings-status" role="status">${text}</span>
    </form>`;
}

// Egress is the instance's allowlist and the requests its proxy refused (002-AC34 to 002-AC36).
/** @param {{i: Instance, st: import('./api.js').Status}} p */
function Egress({ i, st }) {
  const [domain, setDomain] = useState('');
  const [text, run] = useStatus();
  const [denied, setDenied] = useState(/** @type {import('./api.js').Denied[] | null} */ (null));
  const [deniedErr, setDeniedErr] = useState('');
  useEffect(() => {
    let timer = 0;
    const load = () => api('GET', '/api/denied?instance=' + encodeURIComponent(i.id))
      .then((r) => { setDenied(r.denied); setDeniedErr(''); })
      .catch((err) => setDeniedErr(err.message))
      .finally(() => { if (!stopped.value) timer = setTimeout(load, 5000); });
    load();
    return () => clearTimeout(timer);
  }, [i.id]);
  /** @param {string[]} list */
  const save = (list) => run(api('POST', '/api/allowlist', { instance: i.id, allowlist: list }), 'Saved.');
  /** @param {Event} e */
  const add = (e) => {
    e.preventDefault();
    save([...st.allowlist, domain.trim()]).then((ok) => { if (ok) setDomain(''); });
  };
  return html`
    <h3>Websites the AI may reach</h3>
    <p class="hint">The sandbox reaches these domains, and nothing else, through its proxy and over HTTPS only. A leading dot, as in .example.com, also allows every subdomain. Anthropic's hosts stay blocked whatever the list says.</p>
    <ul id="allowlist">
      ${!st.allowlist.length && html`<li>No domain: the sandbox reaches nothing.</li>`}
      ${st.allowlist.map((dom) => html`<li><code>${dom}</code> <button type="button" aria-label=${'Remove ' + dom}
        onClick=${() => save(st.allowlist.filter((x) => x !== dom))}>Remove</button></li>`)}
    </ul>
    <form id="allowlist-form" class="row" onSubmit=${add}>
      <label>Domain <input id="allowlist-add" required placeholder="example.com" size="30" autocomplete="off" spellcheck=${false}
        value=${domain} onInput=${(/** @type {any} */ e) => setDomain(e.currentTarget.value)} /></label>
      <button type="submit">Add</button>
      <span id="allowlist-status" role="status">${text}</span>
    </form>
    <h4 id="denied-title">Blocked websites</h4>
    <p class="hint">What the proxy refused in the instance's sandboxes, newest first.</p>
    ${denied && denied.length ? html`<div class="scroll"><table id="denied" aria-labelledby="denied-title">
      <thead><tr><th scope="col">Time</th><th scope="col">Host</th></tr></thead>
      <tbody>${denied.map((x) => html`<tr><td>${new Date(x.time).toLocaleString()}</td><td>${x.host}</td></tr>`)}</tbody>
    </table></div>` : html`<p id="denied-empty">${deniedErr || 'None yet.'}</p>`}`;
}

// LimitsForm sets the instance's sandbox CPUs and memory; the worker restarts with them.
/** @param {{i: Instance}} p */
function LimitsForm({ i }) {
  const [draft, setL] = useState(/** @type {import('./api.js').Limits | null} */ (null));
  const l = draft || i.limits;
  const [text, run] = useStatus();
  const busy = (i.room_states || []).some((r) => r.running || r.queued);
  /** @param {Event} e */
  const save = (e) => {
    e.preventDefault();
    // Chrome fails a request in flight when Docker adds or removes the sandboxes' networks, as the
    // restart does (ERR_NETWORK_CHANGED): the limits may have landed all the same.
    const post = () => api('POST', '/api/limits', { instance: i.id, ...l }).catch(async (err) => {
      if (!(err instanceof TypeError)) throw err;
      const now = (await api('GET', '/api/status')).instances?.find((/** @type {Instance} */ x) => x.id === i.id)?.limits;
      if (!now || Object.entries(l).some(([k, v]) => now[k] !== v)) throw err;
    });
    const apply = () => run(post(), 'Applied: the worker restarts.').then((ok) => { if (ok) setL(null); });
    if (!busy) return apply();
    window.grillConfirm('Apply the limits now?', 'An answer is running on this server: restarting its worker fails it.', 'Apply now')
      .then((yes) => { if (yes) apply(); });
  };
  /** @param {'cpus' | 'memory' | 'egress_cpus' | 'egress_memory'} k @param {string} label @param {string} hint */
  const field = (k, label, hint) => html`<label>${label} <input id=${'limit-' + k.replace('_', '-')} size="6" autocomplete="off" spellcheck=${false}
    placeholder=${hint} value=${l[k]} onInput=${(/** @type {any} */ e) => setL({ ...l, [k]: e.currentTarget.value })} /></label>`;
  return html`
    <h3>CPU and memory limits</h3>
    <p class="hint">Each sandbox and its egress proxy run within these CPUs and memory; 0 means no limit. Applying them restarts the instance's worker.</p>
    <form id="limits-form" class="row" onSubmit=${save}>
      ${field('cpus', 'Sandbox CPUs', '2')} ${field('memory', 'Sandbox memory', '4g')}
      ${field('egress_cpus', 'Egress proxy CPUs', '0.5')} ${field('egress_memory', 'Egress proxy memory', '256m')}
      <button type="submit">Save and restart</button>
      <span id="limits-status" role="status">${text}</span>
    </form>`;
}

// Settings holds the global settings, then one instance's: cap, approval mode, allowlist, limits.
function Settings() {
  const d = /** @type {StatusReply} */ (data.value);
  const instances = d.instances || [];
  const id = instances.some((i) => i.id === picked.value) ? picked.value : d.instance || '';
  const inst = instances.find((i) => i.id === id);
  const st = inst?.status;
  // LimitsForm stays out of the worker's settings: applying limits restarts the worker, and the
  // form's status line must outlive the restart.
  return html`<${Section} id="settings" title="Settings">
    ${d.concurrency && html`<div class="cards"><div class="card"><h3>Answers at once</h3>
      <${NumberForm} id="max-turns" label="Turns at once, across every instance and room" value=${d.concurrency.max_turns}
        min=${1} max=${16} button="Save" path="/api/max-turns" field="max_turns" /></div>
      <div class="card"><h3>History</h3>
      <${NumberForm} id="retention" label="Days the turn history keeps" value=${d.retention_days || 30} min=${1} max=${3650}
        button="Save" path="/api/retention" field="days" />
      </div>
      <div class="card"><h3>Start at login</h3><${Autostart} /></div></div>`}
    ${inst && html`<h3 id="instance-settings-title">Instance</h3>
      <p class="row"><label>Instance <select id="settings-instance" value=${id} onChange=${(/** @type {any} */ e) => { picked.value = e.currentTarget.value; }}>
        ${instances.map((i) => html`<option value=${i.id}>${i.url}</option>`)}
      </select></label></p>`}
    ${!inst ? html`<p class="hint">The instance settings show once you pair with a room.</p>`
      : st ? html`<div key=${id} class="per-instance cards">
        <div class="card"><${Machine} i=${inst} st=${st} /></div>
        <div class="card"><${Cap} i=${inst} st=${st} /></div>
        <div class="card"><${Egress} i=${inst} st=${st} /></div>
      </div>` : html`<p class="hint">The worker for ${inst.url} is ${inst.state}${inst.error ? ': ' + inst.error : ''}. Its settings show once it runs.</p>`}
    ${inst && html`<div class="card"><${LimitsForm} key=${id} i=${inst} /></div>`}
  <//>`;
}

function Logs() {
  const ref = useRef(/** @type {HTMLPreElement | null} */ (null));
  const lines = data.value?.logs || [];
  const atBottom = useRef(true);
  const el = ref.current;
  if (el) atBottom.current = el.scrollTop + el.clientHeight >= el.scrollHeight - 20;
  useEffect(() => {
    if (ref.current && atBottom.current) ref.current.scrollTop = ref.current.scrollHeight;
  });
  return html`<${Section} id="logs" title="Logs"><pre id="logs" ref=${ref}>${lines.join('\n')}</pre><//>`;
}

function App() {
  const d = data.value;
  const live = d && !stopped.value;
  return html`
    <${Header} />
    ${live && html`<${TabBar} />`}
    ${banner.value && html`<p id="banner" role="alert">${sentence(banner.value)}</p>`}
    ${live && html`<${RelaxBanner} />`}
    ${live && html`<main id="main">
      <${Panel} id="instances"><${Instances} /><//>
      <${Panel} id="rooms"><${Room} /><${Rooms} /><//>
      <${Panel} id="accounts"><${Accounts} /><//>
      <${Panel} id="settings"><${Settings} /><//>
      <${Panel} id="monitoring">${d.instances && html`<${Monitoring} instances=${d.instances} />`}<${Logs} /><//>
      <${Panel} id="update"><${Section} id="update" title="Update"><${UpdateOffer} /><${Trust} /><//><//>
    </main>`}`;
}

render(html`<${App} />`, /** @type {HTMLElement} */ (document.getElementById('app')));

startSession().then(poll).catch((err) => { banner.value = err.message; });
