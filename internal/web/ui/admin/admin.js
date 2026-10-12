// The instance admin page (004-AC1 to 004-AC6, 004-AC9, 004-AC19): Preact, htm and signals,
// vendored, no build step. The session cookie is HttpOnly: the page never sees a token after the
// login form.
import { render } from 'preact';
import { useState, useEffect, useRef } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';

/**
 * @typedef {{id: number, created_at: string, last_used_at: string, expires_at: string, user_agent: string, ip: string,
 *   current: boolean}} Session
 * @typedef {{id: number, at: string, actor: string, action: string, target_kind: string, target_id: string,
 *   target_name: string, details: Record<string, unknown>}} Entry
 * @typedef {{entries: Entry[], next: number, actions: string[]}} AuditPage
 * @typedef {{uploads: number, revisions: number, attachments: number, agent_sessions: number, transcripts: number,
 *   room_skills: number, total: number}} Volume
 * @typedef {{id: string, name: string, created_at: string, last_activity: string, participants: number, personas: number,
 *   host_participant_id?: number, quota_bytes: number, quota: number, used: number, host_online: boolean, volume: Volume}} Room
 * @typedef {{rooms: Room[], default_quota: number}} RoomsPage
 * @typedef {{id: number, created_at: string, created_by?: number, revoked_at?: string}} Invite
 * @typedef {{room_id: string, room_name: string}} ProfileRoom
 * @typedef {{id: number, uuid: string, name: string, email?: string, created_at: string, last_activity: string,
 *   skills: number, rooms: ProfileRoom[]}} Profile
 * @typedef {{id: number, preset?: string, title: string, version: number, author: string, created_at: string}} Template
 * @typedef {{id: number, name: string, shared: boolean, version: number, author: string, size: number,
 *   updated_at: string}} Skill
 * @typedef {{version: string, db_bytes: number, disk_free: number, disk_total: number,
 *   stt: {voice: boolean, configured: boolean, workers: number},
 *   daemons: {connected: number, versions: Record<string, number>}, smtp: boolean}} Instance
 */

/** @type {import('@preact/signals').Signal<boolean | null>} null until the first answer says. */
const signedIn = signal(null);
const notice = signal('');
const views = [['rooms', 'Rooms'], ['profiles', 'Profiles'], ['libraries', 'Libraries'], ['instance', 'Instance'],
  ['sessions', 'Sessions'], ['audit', 'Audit log']];
/** @param {string} hash */
const viewOf = (hash) => views.find(([id]) => '#' + id === hash)?.[0] || 'rooms';
const view = signal(viewOf(location.hash));
addEventListener('hashchange', () => { view.value = viewOf(location.hash); });

/**
 * api sends a request with the session cookie and returns its JSON answer, {} for none. A 401
 * outside the login form means the session is gone: the page shows the form again.
 * @param {string} method
 * @param {string} path
 * @param {unknown} [body]
 * @returns {Promise<any>}
 */
async function api(method, path, body) {
  /** @type {RequestInit & {headers: Record<string, string>}} */
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const data = res.status === 204 ? {} : await res.json().catch(() => ({}));
  if (res.status === 401 && path !== '/api/admin/login') {
    if (signedIn.value) notice.value = 'Your session ended: sign in again.';
    signedIn.value = false;
  }
  if (!res.ok) throw new Error(data.error || 'HTTP ' + res.status);
  return data;
}

/** @param {unknown} err */
const show = (err) => { if (signedIn.value) notice.value = /** @type {Error} */ (err).message; };
/** @param {string} t */
const when = (t) => new Date(t).toLocaleString();
/** @param {number} n bytes */
function size(n) {
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return (n < 10 ? n.toFixed(1) : Math.round(n)) + ' ' + units[i];
}
/** @param {(v: string) => void} set */
const onInput = (set) => (/** @type {Event} */ e) => set(/** @type {HTMLInputElement} */ (e.currentTarget).value);
/**
 * formAction runs work on a form's submit, without a page load.
 * @param {() => unknown} work
 */
const formAction = (work) => (/** @type {Event} */ e) => { e.preventDefault(); work(); };

function Login() {
  const [token, setToken] = useState('');
  const [said, setSaid] = useState('');
  /** @param {Event} e */
  const submit = async (e) => {
    e.preventDefault();
    setSaid('');
    try {
      await api('POST', '/api/admin/login', { token });
      notice.value = '';
      setToken('');
      signedIn.value = true;
    } catch (err) {
      setSaid(/** @type {Error} */ (err).message);
    }
  };
  return html`
    <main class="login">
      <h1>grill-room admin</h1>
      ${notice.value && html`<p class="said" role="alert">${notice.value}</p>`}
      <form onSubmit=${submit}>
        <label>Admin token
          <input type="password" id="token" autocomplete="current-password" required value=${token}
            onInput=${(/** @type {Event} */ e) => setToken(/** @type {HTMLInputElement} */ (e.currentTarget).value)}/>
        </label>
        <button type="submit">Sign in</button>
        <p id="login-status" class="said bad" role="status">${said}</p>
      </form>
    </main>`;
}

function Sessions() {
  const [list, setList] = useState(/** @type {Session[] | null} */ (null));
  const load = () => api('GET', '/api/admin/sessions').then(setList).catch(show);
  useEffect(() => { load(); }, []);
  /** @param {Session} s */
  const revoke = (s) => api('DELETE', `/api/admin/sessions/${s.id}`)
    .then(() => { if (s.current) signedIn.value = false; else load(); })
    .catch(show);
  return html`
    <h2>Sessions</h2>
    <p class="hint">Browsers signed in with the admin token, for 12 hours each. Revoking one signs it out at its next request.</p>
    ${list && html`<div class="scroll"><table id="sessions">
      <thead><tr><th>Signed in</th><th>Last used</th><th>Expires</th><th>IP</th><th>Browser</th><th></th></tr></thead>
      <tbody>${list.map((s) => html`<tr id=${'session-' + s.id}>
        <td>${when(s.created_at)}${s.current && html` <strong>(this browser)</strong>`}</td>
        <td>${when(s.last_used_at)}</td><td>${when(s.expires_at)}</td><td>${s.ip}</td>
        <td class="wrap">${s.user_agent}</td>
        <td><button type="button" onClick=${() => revoke(s)}>${s.current ? 'Sign out' : 'Revoke'}</button></td>
      </tr>`)}</tbody>
    </table></div>`}`;
}

function Audit() {
  const [action, setAction] = useState('');
  const [target, setTarget] = useState('');
  // newer holds the cursors of the pages before this one, for going back.
  const [newer, setNewer] = useState(/** @type {number[]} */ ([]));
  const [before, setBefore] = useState(0);
  const [page, setPage] = useState(/** @type {AuditPage | null} */ (null));
  useEffect(() => {
    const q = new URLSearchParams();
    if (action) q.set('action', action);
    if (target) q.set('target', target);
    if (before) q.set('before', String(before));
    // A slower answer to an older filter must not overwrite the current one.
    let current = true;
    api('GET', '/api/admin/audit?' + q).then((p) => { if (current) setPage(p); }).catch(show);
    return () => { current = false; };
  }, [action, target, before]);
  /** @param {(v: string) => void} set */
  const filter = (set) => (/** @type {Event} */ e) => {
    set(/** @type {HTMLInputElement} */ (e.currentTarget).value.trim());
    setBefore(0);
    setNewer([]);
  };
  const older = () => {
    if (!page) return;
    setNewer([...newer, before]);
    setBefore(page.next);
  };
  const back = () => {
    setBefore(newer[newer.length - 1]);
    setNewer(newer.slice(0, -1));
  };
  return html`
    <h2>Audit log</h2>
    <p class="hint">Every admin action, kept a year. Read only.</p>
    <div class="row">
      <label>Action
        <select id="audit-action" onChange=${filter(setAction)}>
          <option value="">any</option>
          ${(page?.actions || []).map((a) => html`<option value=${a} selected=${a === action}>${a}</option>`)}
        </select>
      </label>
      <label>Target (id or name)
        <input id="audit-target" type="search" onChange=${filter(setTarget)}/>
      </label>
    </div>
    ${page && (page.entries.length ? html`<div class="scroll"><table id="audit">
      <thead><tr><th>When</th><th>Who</th><th>Action</th><th>Target</th><th>Details</th></tr></thead>
      <tbody>${page.entries.map((e) => html`<tr id=${'audit-' + e.id}>
        <td>${when(e.at)}</td><td>${e.actor}</td><td>${e.action}</td>
        <td class="wrap">${e.target_kind} ${e.target_name && html`<strong>${e.target_name}</strong>`} <code>${e.target_id}</code></td>
        <td class="wrap"><code>${JSON.stringify(e.details)}</code></td>
      </tr>`)}</tbody>
    </table></div>` : html`<p>No entry matches.</p>`)}
    <div class="row">
      <button type="button" onClick=${back} disabled=${!newer.length}>Newer</button>
      <button type="button" onClick=${older} disabled=${!page?.next}>Older</button>
    </div>`;
}

/** @type {[string, string, (r: Room) => string | number | boolean][]} */
const roomCols = [
  ['name', 'Name', (r) => r.name.toLowerCase()],
  ['created', 'Created', (r) => r.created_at],
  ['activity', 'Last activity', (r) => r.last_activity],
  ['participants', 'Participants', (r) => r.participants],
  ['personas', 'Personas', (r) => r.personas],
  ['host', 'Host', (r) => r.host_online],
  ['volume', 'Volume', (r) => r.volume.total],
  ['quota', 'Quota used', (r) => r.used / r.quota],
];

function Rooms() {
  const [page, setPage] = useState(/** @type {RoomsPage | null} */ (null));
  const [sort, setSort] = useState({ col: 'activity', desc: true });
  const [open, setOpen] = useState('');
  const load = () => api('GET', '/api/admin/rooms').then(setPage).catch(show);
  useEffect(() => { load(); }, []);
  if (!page) return html`<h2>Rooms</h2>`;
  const key = (roomCols.find(([id]) => id === sort.col) || roomCols[0])[2];
  const rooms = [...page.rooms].sort((a, b) => {
    const x = key(a), y = key(b);
    return (x < y ? -1 : x > y ? 1 : 0) * (sort.desc ? -1 : 1);
  });
  /** @param {string} col */
  const by = (col) => setSort({ col, desc: sort.col === col ? !sort.desc : col !== 'name' });
  const room = page.rooms.find((r) => r.id === open);
  return html`
    <h2>Rooms</h2>
    <p class="hint">${page.rooms.length} rooms. A room's quota counts its uploads and attachments; the default is ${size(page.default_quota)}.</p>
    ${page.rooms.length ? html`<div class="scroll"><table id="rooms">
      <thead><tr>${roomCols.map(([id, label]) => html`<th aria-sort=${sort.col === id ? (sort.desc ? 'descending' : 'ascending') : undefined}>
        <button type="button" class="sort" onClick=${() => by(id)}>${label}</button></th>`)}<th></th></tr></thead>
      <tbody>${rooms.map((r) => html`<tr id=${'room-' + r.id}>
        <td class="wrap">${r.name}</td><td>${when(r.created_at)}</td><td>${when(r.last_activity)}</td>
        <td>${r.participants}</td><td>${r.personas}</td><td>${!r.host_participant_id ? 'none' : r.host_online ? 'online' : 'offline'}</td><td>${size(r.volume.total)}</td>
        <td><meter min="0" max=${r.quota} value=${r.used} title=${size(r.used) + ' of ' + size(r.quota)}></meter> ${size(r.used)} / ${size(r.quota)}</td>
        <td><button type="button" onClick=${() => setOpen(open === r.id ? '' : r.id)}>${open === r.id ? 'Close' : 'Manage'}</button></td>
      </tr>`)}</tbody>
    </table></div>` : html`<p>No room yet.</p>`}
    ${room && html`<${RoomPanel} key=${room.id} room=${room} reload=${load} close=${() => { setOpen(''); load(); }}/>`}`;
}

/** @param {{room: Room, reload: () => void, close: () => void}} props */
function RoomPanel({ room, reload, close }) {
  const base = '/api/admin/rooms/' + room.id;
  const [name, setName] = useState(room.name);
  const [quota, setQuota] = useState(String(Math.round(room.quota / 1048576)));
  const [confirm, setConfirm] = useState('');
  const [invites, setInvites] = useState(/** @type {Invite[] | null} */ (null));
  const [link, setLink] = useState('');
  const loadInvites = () => api('GET', base + '/invites').then(setInvites).catch(show);
  // The panel opens under the list: a long list would leave it off screen.
  const panel = useRef(/** @type {HTMLElement | null} */ (null));
  useEffect(() => { loadInvites(); panel.current?.scrollIntoView({ block: 'nearest' }); }, []);
  const v = room.volume;
  const kinds = [['Uploads', v.uploads], ['Revisions', v.revisions], ['Attachments', v.attachments],
    ['Agent sessions', v.agent_sessions], ['Transcripts', v.transcripts], ['Room skills', v.room_skills]];
  /** @param {number} bytes */
  const setQuotaTo = (bytes) => api('PUT', base + '/quota', { quota_bytes: bytes }).then(reload).catch(show);
  const invite = () => api('POST', base + '/invites').then((/** @type {{url: string}} */ i) => { setLink(i.url); loadInvites(); }).catch(show);
  /** @param {Invite} i */
  const revoke = (i) => api('DELETE', `${base}/invites/${i.id}`).then(loadInvites).catch(show);
  return html`
    <section id="room-panel" ref=${panel} aria-label=${'Manage ' + room.name}>
      <h2>${room.name} <code>${room.id}</code></h2>
      <h3>Volume</h3>
      <dl id="volume">${kinds.map(([k, n]) => html`<div><dt>${k}</dt><dd>${size(/** @type {number} */ (n))}</dd></div>`)}
        <div><dt>Total</dt><dd>${size(v.total)}</dd></div></dl>
      <h3>Quota</h3>
      <p class="hint">${size(room.used)} of ${size(room.quota)} used${room.quota_bytes ? '' : ', the instance default'}.</p>
      <form class="row" onSubmit=${formAction(() => setQuotaTo(Math.round(Number(quota) * 1048576)))}>
        <label>Quota (MB)<input id="quota" type="number" min="1" step="1" required value=${quota} onInput=${onInput(setQuota)}/></label>
        <button type="submit">Set quota</button>
        <button type="button" disabled=${!room.quota_bytes} onClick=${() => setQuotaTo(0)}>Use the default</button>
      </form>
      <h3>Name</h3>
      <form class="row" onSubmit=${formAction(() => api('PATCH', base, { name }).then(reload).catch(show))}>
        <label>Room name<input id="room-name" required maxlength="100" value=${name} onInput=${onInput(setName)}/></label>
        <button type="submit" disabled=${name.trim() === room.name}>Rename</button>
      </form>
      <h3>Invites</h3>
      <div class="row"><button type="button" onClick=${invite}>New invite link</button></div>
      ${link && html`<label class="link">Invite link, shown once<input id="invite-link" readonly value=${link}/></label>`}
      ${invites && html`<div class="scroll"><table id="invites">
        <thead><tr><th>Invite</th><th>Created</th><th>By</th><th>State</th><th></th></tr></thead>
        <tbody>${invites.map((i) => html`<tr id=${'invite-' + i.id}><td>#${i.id}</td><td>${when(i.created_at)}</td>
          <td>${i.created_by ? 'participant #' + i.created_by : 'admin'}</td><td>${i.revoked_at ? 'revoked' : 'valid'}</td>
          <td>${!i.revoked_at && html`<button type="button" onClick=${() => revoke(i)}>Revoke</button>`}</td></tr>`)}</tbody>
      </table></div>`}
      <h3>Export</h3>
      <div class="row"><button type="button" id="export" disabled>Export</button><span class="hint">Export is not available yet.</span></div>
      <h3>Delete</h3>
      <form class="row" onSubmit=${formAction(() => api('DELETE', base).then(close).catch(show))}>
        <label>Type the room name to delete it, with everything it holds
          <input id="delete-confirm" autocomplete="off" value=${confirm} onInput=${onInput(setConfirm)}/></label>
        <button type="submit" class="danger" disabled=${confirm !== room.name}>Delete the room</button>
      </form>
    </section>`;
}

function Profiles() {
  const [list, setList] = useState(/** @type {Profile[] | null} */ (null));
  const load = () => api('GET', '/api/admin/profiles').then(setList).catch(show);
  useEffect(() => { load(); }, []);
  /** @param {Profile} p */
  const remove = (p) => {
    if (!window.confirm(`Delete the profile of ${p.name || p.uuid}? Its participants stay in their rooms, detached.`)) return;
    api('DELETE', '/api/admin/profiles/' + p.id).then(load).catch(show);
  };
  return html`
    <h2>Profiles</h2>
    <p class="hint">Emails show here and nowhere else. A profile's name is the one it last joined a room with.</p>
    ${list && (list.length ? html`<div class="scroll"><table id="profiles">
      <thead><tr><th>Name</th><th>Email</th><th>Rooms</th><th>Skills</th><th>Last activity</th><th></th></tr></thead>
      <tbody>${list.map((p) => html`<tr id=${'profile-' + p.id}>
        <td>${p.name || '—'} <code title=${p.uuid}>${p.uuid.slice(0, 8)}</code></td><td>${p.email || '—'}</td>
        <td class="wrap">${p.rooms.map((r) => r.room_name).join(', ') || '—'}</td><td>${p.skills}</td><td>${when(p.last_activity)}</td>
        <td><button type="button" onClick=${() => remove(p)}>Delete</button></td>
      </tr>`)}</tbody>
    </table></div>` : html`<p>No profile yet.</p>`)}`;
}

function Libraries() {
  const [templates, setTemplates] = useState(/** @type {Template[] | null} */ (null));
  const [skills, setSkills] = useState(/** @type {Skill[] | null} */ (null));
  const load = () => Promise.all([
    api('GET', '/api/admin/templates').then(setTemplates),
    api('GET', '/api/admin/skills').then((/** @type {Skill[]} */ ks) => setSkills(ks.filter((k) => k.shared))),
  ]).catch(show);
  useEffect(() => { load(); }, []);
  /** @param {string} what @param {string} path */
  const remove = (what, path) => {
    if (window.confirm(`Remove ${what} from the library? Rooms keep the copies they use.`)) api('DELETE', path).then(load).catch(show);
  };
  return html`
    <h2>Persona templates</h2>
    ${templates && html`<div class="scroll"><table id="templates">
      <thead><tr><th>Title</th><th>Author</th><th>Version</th><th>Updated</th><th></th></tr></thead>
      <tbody>${templates.map((t) => html`<tr id=${'template-' + t.id}>
        <td class="wrap">${t.title}</td><td>${t.author}</td><td>v${t.version}</td><td>${when(t.created_at)}</td>
        <td>${t.preset ? html`<span class="hint">shipped</span>`
          : html`<button type="button" onClick=${() => remove(`the template ${t.title}`, '/api/admin/templates/' + t.id)}>Remove</button>`}</td>
      </tr>`)}</tbody>
    </table></div>`}
    <h2>Shared skills</h2>
    ${skills && (skills.length ? html`<div class="scroll"><table id="skills">
      <thead><tr><th>Name</th><th>Author</th><th>Version</th><th>Size</th><th>Updated</th><th></th></tr></thead>
      <tbody>${skills.map((k) => html`<tr id=${'skill-' + k.id}>
        <td class="wrap">${k.name}</td><td>${k.author}</td><td>v${k.version}</td><td>${size(k.size)}</td><td>${when(k.updated_at)}</td>
        <td><button type="button" onClick=${() => remove(`the skill ${k.name}`, '/api/admin/skills/' + k.id)}>Remove</button></td>
      </tr>`)}</tbody>
    </table></div>` : html`<p>No shared skill yet.</p>`)}`;
}

function InstanceView() {
  const [it, setIt] = useState(/** @type {Instance | null} */ (null));
  useEffect(() => { api('GET', '/api/admin/instance').then(setIt).catch(show); }, []);
  if (!it) return html`<h2>Instance</h2>`;
  const versions = Object.entries(it.daemons.versions).map(([v, n]) => `${v || 'unknown'} × ${n}`).join(', ');
  const rows = [
    ['Version', it.version || 'unknown'],
    ['Database', size(it.db_bytes)],
    ['Disk', it.disk_free < 0 ? 'unknown' : `${size(it.disk_free)} free of ${size(it.disk_total)}`],
    ['Speech to text', !it.stt.voice ? 'off: no audio' : !it.stt.configured ? 'off: no STT token'
      : `${it.stt.workers} worker${it.stt.workers === 1 ? '' : 's'} up`],
    ['Daemons', `${it.daemons.connected} connected${versions ? ': ' + versions : ''}`],
    ['Email (SMTP)', it.smtp ? 'configured' : 'not configured'],
  ];
  return html`
    <h2>Instance</h2>
    <dl id="instance">${rows.map(([k, v]) => html`<div><dt>${k}</dt><dd>${v}</dd></div>`)}</dl>`;
}

function Shell() {
  const logout = () => api('POST', '/api/admin/logout').then(() => { signedIn.value = false; }).catch(show);
  /** @param {string} id @param {string} name */
  const link = (id, name) => html`<a href=${'#' + id} aria-current=${view.value === id ? 'page' : undefined}>${name}</a>`;
  return html`
    <header>
      <h1>grill-room admin</h1>
      <nav aria-label="Views">${views.map(([id, name]) => link(id, name))}</nav>
      <button type="button" id="logout" onClick=${logout}>Sign out</button>
    </header>
    <main>
      ${notice.value && html`<p class="said bad" role="alert">${notice.value}</p>`}
      ${{ rooms: html`<${Rooms}/>`, profiles: html`<${Profiles}/>`, libraries: html`<${Libraries}/>`, instance: html`<${InstanceView}/>`,
        sessions: html`<${Sessions}/>`, audit: html`<${Audit}/>` }[view.value]}
    </main>`;
}

function App() {
  if (signedIn.value === null) return html`<p>Loading…</p>`;
  return signedIn.value ? html`<${Shell}/>` : html`<${Login}/>`;
}

api('GET', '/api/admin/sessions').then(() => { signedIn.value = true; }).catch((err) => {
  if (signedIn.value === null) {
    signedIn.value = false;
    notice.value = err.message;
  }
});
render(html`<${App}/>`, /** @type {HTMLElement} */ (document.getElementById('app')));
