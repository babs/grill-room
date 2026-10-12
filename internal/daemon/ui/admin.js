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

async function poll() {
  try {
    await reload();
  } catch (err) {
    if (!stopped.value) banner.value = /** @type {Error} */ (err).message;
    return;
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
      setText(/** @type {Error} */ (err).message);
      return false;
    }
  };
  return [text, run];
}

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
  const stop = () => api('POST', '/api/stop').then(() => {
    stopped.value = true;
    banner.value = 'The daemon stopped. You can close this tab.';
  }).catch((err) => { banner.value = err.message; });
  return html`
    <header>
      <h1>grill-room daemon</h1>
      <span id="host">${d ? d.status.host + ' · ' + d.status.version : ''}</span>
      ${t && html`<span id="self-sig" class=${'badge ' + cls}>${sig}</span>`}
      <nav aria-label="Sections">
        ${[['room', 'Room'], ['instances', 'Instances'], ['rooms', 'Rooms'], ['accounts', 'Accounts'], ['settings', 'Settings'],
          ['monitoring', 'Monitoring'], ['logs', 'Logs']].map(([id, name]) => html`<a href=${'#' + id + '-section'}>${name}</a>`)}
      </nav>
      <button type="button" id="stop" onClick=${stop} disabled=${stopped.value}>Stop the daemon</button>
    </header>`;
}

// Alerts are the page's warnings: logins near expiry, unsigned workers, full contexts and stuck
// queues (003-AC29).
function Alerts() {
  const list = data.value?.alerts || [];
  if (!list.length) return null;
  return html`<ul id="alerts" role="alert">${list.map((a) => html`<li class=${'alert-' + a.kind}>${a.text}</li>`)}</ul>`;
}

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
  if (!u?.available) return null;
  const apply = () => {
    setBusy(true);
    setText('Downloading…');
    api('POST', '/api/update').then(() => {
      stopped.value = true;
      banner.value = 'Updated. The daemon restarts and opens a new page; you can close this one.';
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

// Room pairs this machine with a room link, then waits for the approval in the room.
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
    api('POST', '/api/room', { url }).then(reload).catch((x) => setErr(x.message));
  };
  return html`<${Section} id="room" title="Room">
    <p id="room">${s.room || 'No room yet: paste the room link below.'}</p>
    <p id="pairing" role="status">${text + (s.error ? ' ' + s.error : '')}</p>
    ${s.pairing === 'pending' && /^https?:\/\//.test(s.approve_url || '') && html`<p id="approve">
      Check that the room page shows the code <strong id="code">${s.code}</strong>, then
      <a id="approve-link" href=${s.approve_url} target="_blank" rel="noopener noreferrer">approve in the room</a>.
    </p>`}
    ${!(s.pairing === 'paired' && s.connection === 'connected') && html`<form id="room-form" class="row" onSubmit=${pair}>
      <label>Room link <input id="room-url" type="url" required placeholder="https://.../r/..." value=${url}
        onInput=${(/** @type {any} */ e) => setURL(e.currentTarget.value)} /></label>
      <button type="submit">Pair with this room</button>
    </form>`}
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
        relax(true, host).then((ok) => { if (ok) setHost(''); });
      }}>
        <label>To accept its unsigned binaries, type its host <input class="relax-host" required size="24" autocomplete="off" spellcheck=${false}
          value=${host} onInput=${(/** @type {any} */ e) => setHost(e.currentTarget.value)} /></label>
        <button type="submit">Accept unsigned binaries</button>
      </form>`}
    <${Said} text=${relaxText} />
    ${accounts && html`<div class="row account-map">
      Claude account <${AccountSelect} label=${'Claude account of ' + i.url} value=${i.account} onPick=${(/** @type {string} */ a) => setAccount(i.id, '', a)} />
      ${plural(i.requests, 'request')} through the proxies
      <button type="button" onClick=${() => api('POST', '/api/cut-off', { instance: i.id, cut_off: !i.cut_off }).then(reload)
        .catch((err) => { banner.value = err.message; })}>${i.cut_off ? 'Let it use Claude again' : 'Cut it off from Claude'}</button>
      ${i.cut_off && html`<${Badge} text="cut off: its turns end and none starts" cls="danger" />`}
    </div>`}
  </li>`;
}

function Instances() {
  const d = /** @type {StatusReply} */ (data.value);
  if (!d.instances) return null;
  return html`<${Section} id="instances" title="Instances">
    <p class="hint">One worker per room server, running the version that server hands out. The Room section above shows the last one you paired with.</p>
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
  if (st.host === true) parts.push('hosts the agent', 'parallel turns ' + (st.parallel_turns ? 'on' : 'off'));
  if (st.host === false) parts.push('not the host');
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
      <ul class="rooms">${i.rooms.map((r) => html`<li>
        <code class="room-link">${r}</code> <span class="room-state">${roomText((i.room_states || []).find((x) => x.room === r))}</span>
        ${d.accounts && html` · <${AccountSelect} label=${'Claude account of ' + r} value=${(i.room_accounts || {})[r] || ''} roomDefault=${i.account}
          onPick=${(/** @type {string} */ a) => setAccount(i.id, r, a)} />`}
      </li>`)}</ul>
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
    : 'config dir ' + (a.config_dir || '~/.claude');
  const total = Object.values(a.requests || {}).reduce((n, x) => n + x, 0);
  const info = (a.expires_at ? (a.expiry_estimated ? 'expires about ' : 'access token expires ') + when(a.expires_at) + ' · ' : '') +
    'last use ' + when(a.last_use) + ' · ' + plural(total, 'request') + (a.checked_at ? ' · checked ' + when(a.checked_at) : '') +
    (a.detail ? ' · ' + a.detail : '');
  const remove = () => {
    if (!confirm('Remove the Claude account ' + a.name + '?')) return;
    api('POST', '/api/accounts/remove', { name: a.name }).then(reload).catch((err) => onError(err.message));
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
        ? html`<label>Config dir <input id="account-dir" size="30" autocomplete="off" spellcheck=${false} placeholder="empty for ~/.claude"
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
      <button type="submit">Use this channel</button>
      <span id="channel-status" role="status">${chText}</span>
    </form>
    <h3>Trusted dev keys</h3>
    <p class="hint">Worker binaries signed with one of these cosign public keys run like project releases. Keys also load from the dev-keys folder of the supervisor's config.</p>
    <ul id="dev-keys">
      ${!t.dev_keys.length && html`<li>None.</li>`}
      ${t.dev_keys.map((k) => html`<li><code>${k.name}</code> · ${k.fingerprint} <button type="button" aria-label=${'Remove ' + k.name}
        onClick=${() => runKey(api('POST', '/api/dev-keys/remove', { name: k.name }))}>Remove</button></li>`)}
    </ul>
    <form id="key-form" class="row" onSubmit=${addKey}>
      <label>Name <input id="key-name" required size="16" autocomplete="off" spellcheck=${false} pattern="[A-Za-z0-9][A-Za-z0-9._\\-]{0,63}"
        value=${keyName} onInput=${(/** @type {any} */ e) => setKeyName(e.currentTarget.value)} /></label>
      <label>Public key <textarea id="key-pem" required rows="4" spellcheck=${false} placeholder="-----BEGIN PUBLIC KEY-----"
        value=${pem} onInput=${(/** @type {any} */ e) => setPem(e.currentTarget.value)}></textarea></label>
      <button type="submit">Trust this key</button>
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
    <h3>This machine</h3>
    <dl>
      <dt>Docker</dt><${Mark} id="docker" ok=${st.docker.ok} text=${st.docker.ok ? 'running (' + st.docker.version + ')' : st.docker.reason || ''} />
      <dt>Sandbox image</dt><${Mark} id="image" ok=${img.state === 'ready'} text=${imageText || img.state} />
      <dt>Hosting the agent</dt><${Mark} id="can-host" ok=${st.can_host} text=${st.can_host ? 'available' : 'unavailable: ' + st.reason} />
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
    <h3>Host cap</h3>
    <p class="hint">A persona asking for more runs at this cap until you approve it in the room.</p>
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
    <h3>Egress allowlist</h3>
    <p class="hint">The sandbox reaches these domains, and nothing else, through its proxy and over HTTPS only. A leading dot, as in .example.com, also allows every subdomain. Anthropic's hosts stay blocked whatever the list says.</p>
    <ul id="allowlist">
      ${!st.allowlist.length && html`<li>No domain: the sandbox reaches nothing.</li>`}
      ${st.allowlist.map((dom) => html`<li><code>${dom}</code> <button type="button" aria-label=${'Remove ' + dom}
        onClick=${() => save(st.allowlist.filter((x) => x !== dom))}>Remove</button></li>`)}
    </ul>
    <form id="allowlist-form" class="row" onSubmit=${add}>
      <label>Domain <input id="allowlist-add" required placeholder="example.com" size="30" autocomplete="off" spellcheck=${false}
        value=${domain} onInput=${(/** @type {any} */ e) => setDomain(e.currentTarget.value)} /></label>
      <button type="submit">Allow</button>
      <span id="allowlist-status" role="status">${text}</span>
    </form>
    <h4 id="denied-title">Denied requests</h4>
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
    if (busy && !confirm('A turn is running on this instance: restarting its worker fails it. Apply the limits now?')) return;
    run(api('POST', '/api/limits', { instance: i.id, ...l }), 'Applied: the worker restarts.').then((ok) => { if (ok) setL(null); });
  };
  /** @param {'cpus' | 'memory' | 'egress_cpus' | 'egress_memory'} k @param {string} label @param {string} hint */
  const field = (k, label, hint) => html`<label>${label} <input id=${'limit-' + k.replace('_', '-')} size="6" autocomplete="off" spellcheck=${false}
    placeholder=${hint} value=${l[k]} onInput=${(/** @type {any} */ e) => setL({ ...l, [k]: e.currentTarget.value })} /></label>`;
  return html`
    <h3>Resource limits</h3>
    <p class="hint">Each sandbox and its egress proxy run within these CPUs and memory; 0 means no limit. Applying them restarts the instance's worker.</p>
    <form id="limits-form" class="row" onSubmit=${save}>
      ${field('cpus', 'Sandbox CPUs', '2')} ${field('memory', 'Sandbox memory', '4g')}
      ${field('egress_cpus', 'Egress proxy CPUs', '0.5')} ${field('egress_memory', 'Egress proxy memory', '256m')}
      <button type="submit">Apply the limits</button>
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
  return html`<${Section} id="settings" title="Settings">
    ${d.concurrency && html`<h3>This supervisor</h3>
      <${NumberForm} id="max-turns" label="Turns at once, across every instance and room" value=${d.concurrency.max_turns}
        min=${1} max=${16} button="Set" path="/api/max-turns" field="max_turns" />
      <${NumberForm} id="retention" label="Days the turn history keeps" value=${d.retention_days || 30} min=${1} max=${3650}
        button="Keep" path="/api/retention" field="days" />
      <${Autostart} />`}
    <${Trust} />
    ${inst && html`<h3 id="instance-settings-title">Instance</h3>
      <p class="row"><label>Instance <select id="settings-instance" value=${id} onChange=${(/** @type {any} */ e) => { picked.value = e.currentTarget.value; }}>
        ${instances.map((i) => html`<option value=${i.id}>${i.url}</option>`)}
      </select></label></p>`}
    ${!inst ? html`<p class="hint">The instance settings show once you pair with a room.</p>`
      : st ? html`<div key=${id} class="per-instance">
        <${Machine} i=${inst} st=${st} />
        <${Cap} i=${inst} st=${st} />
        <${Egress} i=${inst} st=${st} />
        <${LimitsForm} i=${inst} />
      </div>` : html`<p class="hint">The worker for ${inst.url} is ${inst.state}${inst.error ? ': ' + inst.error : ''}. Its settings show once it runs.</p>`}
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
    ${banner.value && html`<p id="banner" role="alert">${banner.value}</p>`}
    ${live && html`<${Alerts} /><${RelaxBanner} /><${UpdateOffer} />`}
    ${live && html`<main id="main">
      <${Room} /><${Instances} /><${Rooms} /><${Accounts} /><${Settings} />
      ${d.instances && html`<${Monitoring} instances=${d.instances} />`}
      <${Logs} />
    </main>`}`;
}

render(html`<${App} />`, /** @type {HTMLElement} */ (document.getElementById('app')));

startSession().then(poll).catch((err) => { banner.value = err.message; });
