// The instance admin page (004-AC1 to 004-AC6, 004-AC9, 004-AC19): Preact, htm and signals,
// vendored, no build step. The session cookie is HttpOnly: the page never sees a token after the
// login form.
import { render } from 'preact';
import { useState, useEffect, useRef } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';
import { RoomPanel, Rename, TabBar, fetchJSON, roomTabs, size, when, ago, onInput, formAction } from './room-panel.js';

/**
 * @typedef {{id: number, created_at: string, last_used_at: string, expires_at: string, user_agent: string, ip: string,
 *   current: boolean}} Session
 * @typedef {{id: number, at: string, actor: string, action: string, target_kind: string, target_id: string,
 *   target_name: string, details: Record<string, unknown>}} Entry
 * @typedef {{entries: Entry[], next: number, actions: string[]}} AuditPage
 * @typedef {import('./room-panel.js').Room} Room
 * @typedef {{rooms: Room[], default_quota: number}} RoomsPage
 * @typedef {{room_id: string, room_name: string}} ProfileRoom
 * @typedef {{id: number, uuid: string, name: string, email?: string, created_at: string, last_activity: string,
 *   skills: number, rooms: ProfileRoom[], max_rooms: number | null, rooms_created: number}} Profile
 * @typedef {{id: number, preset?: string, title: string, version: number, author: string, created_at: string}} Template
 * @typedef {{id: number, name: string, shared: boolean, version: number, author: string, size: number,
 *   updated_at: string}} Skill
 * @typedef {{rooms_per_profile: number, storage_quota_bytes: number, room_creation: string}} Settings
 * @typedef {{join_ip: string, join_ua: string, last_ip: string, last_ua: string, last_seen_at: string | null}} Client
 * @typedef {{id: number, name: string, admin: boolean, has_profile: boolean, client?: Client}} Participant
 * @typedef {{name: string, counts: Record<string, number>, bytes: Record<string, number>, warnings: string[], format: number,
 *   instance_version: string, same_instance: boolean, exported_at: string, archive_bytes: number,
 *   room?: {id: string, name: string}}} ImportReport
 * @typedef {{version: string, db_bytes: number, disk_free: number, disk_total: number,
 *   stt: {voice: boolean, configured: boolean, workers: number},
 *   daemons: {connected: number, versions: Record<string, number>}, smtp: boolean}} Instance
 */

/** @type {import('@preact/signals').Signal<boolean | null>} null until the first answer says. */
const signedIn = signal(null);
const notice = signal('');
const views = [['rooms', 'Rooms'], ['profiles', 'Profiles'], ['libraries', 'Libraries'], ['instance', 'Instance'],
  ['sessions', 'Sessions'], ['audit', 'Audit']];
/**
 * routeOf reads a deep link (005-AC16): #<view>, or #rooms/<room>/<tab> for one room's tab.
 * @param {string} hash
 */
const routeOf = (hash) => {
  const [v, room = '', tab = ''] = hash.slice(1).split('/').map(decodeURIComponent);
  return { view: views.some(([id]) => id === v) ? v : 'rooms', room: v === 'rooms' ? room : '', tab };
};
const route = signal(routeOf(location.hash));
addEventListener('hashchange', () => { route.value = routeOf(location.hash); });
const roomTabList = Object.entries(roomTabs);

/**
 * api is fetchJSON for this page. A 401 outside the login form means the session is gone: the
 * page shows the form again.
 * @type {import('./room-panel.js').Api}
 */
async function api(method, path, body) {
  try {
    return await fetchJSON(method, path, body);
  } catch (err) {
    if (/** @type {{status?: number}} */ (err).status === 401 && path !== '/api/admin/login') {
      if (signedIn.value) notice.value = 'Your session ended: sign in again.';
      signedIn.value = false;
    }
    throw err;
  }
}

/** @param {unknown} err */
const show = (err) => { if (signedIn.value) notice.value = /** @type {Error} */ (err).message; };

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

/**
 * actor names who acted: the super-admin, or a room admin's profile by the name it acted as.
 * @param {Entry} e
 */
const actor = (e) => (e.actor.startsWith('profile:') ? `${e.details.as || 'profile'} (profile ${e.actor.slice(8, 16)})` : e.actor);

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
        <td>${when(e.at)}</td><td>${actor(e)}</td><td>${e.action}</td>
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
  ['host', 'AI host', (r) => r.host_online],
  ['volume', 'Volume', (r) => r.volume.total],
  ['quota', 'Quota used', (r) => r.used / r.quota],
];

function Rooms() {
  const [page, setPage] = useState(/** @type {RoomsPage | null} */ (null));
  const [sort, setSort] = useState({ col: 'activity', desc: true });
  const [search, setSearch] = useState('');
  const load = () => api('GET', '/api/admin/rooms').then(setPage).catch(show);
  useEffect(() => { load(); }, []);
  if (!page) return html`<h2>Rooms</h2>`;
  const { room: open, tab } = route.value;
  if (open) {
    const room = page.rooms.find((r) => r.id === open);
    const base = '/api/admin/rooms/' + encodeURIComponent(open);
    const t = roomTabList.some(([id]) => id === tab) ? tab : 'general';
    return html`
      <p><a href="#rooms" id="back-to-rooms">← All rooms</a></p>
      ${room ? html`<${RoomPanel} key=${room.id} room=${room} base=${base} api=${api} show=${show} setQuota=${true} reload=${load}
        tab=${t} tabs=${roomTabList} href=${(/** @type {string} */ id) => `#rooms/${encodeURIComponent(room.id)}/${id}`}
        close=${() => { location.hash = '#rooms'; load(); }}>${t === 'admins' && html`<${RoomAdmins} base=${base}/>`}<//>`
        : html`<p>No such room.</p>`}`;
  }
  const key = (roomCols.find(([id]) => id === sort.col) || roomCols[0])[2];
  const q = search.trim().toLowerCase();
  const rooms = page.rooms.filter((r) => !q || r.name.toLowerCase().includes(q)).sort((a, b) => {
    const x = key(a), y = key(b);
    return (x < y ? -1 : x > y ? 1 : 0) * (sort.desc ? -1 : 1);
  });
  /** @param {string} col */
  const by = (col) => setSort({ col, desc: sort.col === col ? !sort.desc : col !== 'name' });
  return html`
    <div class="list-head">
      <h2>Rooms (${q ? rooms.length + ' of ' + page.rooms.length : page.rooms.length})</h2>
      <input type="search" id="room-search" aria-label="Search rooms" placeholder="Search rooms…" value=${search} onInput=${onInput(setSearch)}/>
      <${ImportRoom} reload=${load}/>
    </div>
    <p class="hint">A room's quota counts its uploads and attachments; the default is ${size(page.default_quota)}.</p>
    ${page.rooms.length ? html`<div class="scroll"><table id="rooms" class="cards">
      <thead><tr>${roomCols.map(([id, label]) => html`<th aria-sort=${sort.col === id ? (sort.desc ? 'descending' : 'ascending') : undefined}>
        <button type="button" class="sort" onClick=${() => by(id)}>${label}</button></th>`)}<th></th></tr></thead>
      <tbody>${rooms.map((r) => html`<tr id=${'room-' + r.id}>
        <td class="wrap"><strong>${r.name}</strong></td><td data-label="Created">${ago(r.created_at)}</td><td data-label="Last activity">${ago(r.last_activity)}</td>
        <td data-label="People">${r.participants}</td><td data-label="Personas">${r.personas}</td>
        <td data-label="AI host">${!r.host_participant_id ? 'none' : r.host_online ? 'online' : 'offline'}</td><td data-label="Volume">${size(r.volume.total)}</td>
        <td data-label="Quota used"><meter min="0" max=${r.quota} value=${r.used} title=${size(r.used) + ' of ' + size(r.quota)}></meter> ${size(r.used)} / ${size(r.quota)}</td>
        <td><a class="btn btn-secondary" href=${'#rooms/' + encodeURIComponent(r.id)} aria-label=${'Manage ' + r.name}>Manage</a></td>
      </tr>`)}</tbody>
    </table></div>${q && !rooms.length && html`<p class="hint" id="rooms-none">No room matches "${search.trim()}".</p>`}` : html`<p>No room yet.</p>`}`;
}

/**
 * RoomAdmins appoints and removes room admins among the room's participants with a profile: a room
 * the super-admin created starts with none (004-AC8).
 * @param {{base: string}} props
 */
function RoomAdmins({ base }) {
  const [list, setList] = useState(/** @type {Participant[] | null} */ (null));
  const load = () => api('GET', base + '/participants').then(setList).catch(show);
  useEffect(() => { load(); }, [base]);
  /** @param {Participant} p @param {boolean} on */
  const set = (p, on) => api(on ? 'PUT' : 'DELETE', `${base}/admins/${p.id}`).then(load).catch(show);
  return html`
    <p class="hint">Room admins rename the room and its participants, manage its invites, appoint other admins, export and delete it. Only participants with a profile can be appointed.</p>
    ${list && (list.length ? html`<ul id="room-admins">${list.map((p) => html`<li key=${p.id}>
      ${p.name}${p.admin && html` <strong>admin</strong>`}${' '}
      ${!p.has_profile ? html`<span class="hint">no profile</span>`
        : p.admin ? html`<button type="button" class="btn-secondary" onClick=${() => set(p, false)} aria-label=${'Remove ' + p.name + ' from the room admins'}>Remove admin</button>`
        : html`<button type="button" class="btn-secondary" onClick=${() => set(p, true)} aria-label=${'Make ' + p.name + ' a room admin'}>Make admin</button>`}
      ${' '}<${Rename} key=${p.name} name=${p.name} path=${`${base}/participants/${p.id}/name`} api=${api} show=${show} done=${load}/>
      ${p.client && html`<${ClientLine} c=${p.client}/>`}
    </li>`)}</ul>` : html`<p class="hint">No participant yet.</p>`)}`;
}

/**
 * ClientLine shows where a participant connected from, at join and at last activity (005-AC20).
 * The purge leaves the fields empty past their retention.
 * @param {{c: Client}} props
 */
function ClientLine({ c }) {
  const at = (/** @type {string} */ ip, /** @type {string} */ ua) => ip ? `${ip} · ${ua || 'no user agent'}` : 'purged';
  return html`<div class="hint client">Joined from ${at(c.join_ip, c.join_ua)}; last active${c.last_seen_at
    ? ' ' + new Date(c.last_seen_at).toLocaleString() : ''} from ${at(c.last_ip, c.last_ua)}</div>`;
}

/** @type {[string, string][]} */
const importKinds = [['participants', 'Participants'], ['messages', 'Lines'], ['message_versions', 'Earlier versions'],
  ['documents', 'Documents'], ['revisions', 'Revisions'], ['uploads', 'Uploads'], ['annotations', 'Annotations'],
  ['annotation_messages', 'Thread messages'], ['attachments', 'Attachments'], ['mentions', 'Mentions'], ['personas', 'Personas'], ['skills', 'Skills'],
  ['sessions', 'Agent sessions']];

/**
 * ImportRoom uploads an export for a preview of what it holds, which creates nothing, then imports
 * it as a new room (004-AC12, 004-AC15). The file is sent twice: the server keeps nothing between.
 * @param {{reload: () => void}} props
 */
function ImportRoom({ reload }) {
  const sheet = useRef(/** @type {HTMLDialogElement | null} */ (null));
  const [file, setFile] = useState(/** @type {File | null} */ (null));
  const [preview, setPreview] = useState(/** @type {ImportReport | null} */ (null));
  const [said, setSaid] = useState('');
  const [busy, setBusy] = useState(false);
  /** @param {boolean} dry */
  const send = async (dry) => {
    if (!file) return;
    setBusy(true);
    setSaid('');
    try {
      const res = await fetch('/api/admin/rooms/import' + (dry ? '?preview=1' : ''), { method: 'POST', headers: { 'Content-Type': 'application/gzip' }, body: file });
      const data = await res.json().catch(() => ({}));
      if (res.status === 401) signedIn.value = false;
      if (!res.ok) throw new Error(data.error || 'HTTP ' + res.status);
      if (dry) {
        setPreview(data);
      } else {
        setPreview(null);
        setSaid(`Imported as ${data.room.name} (${data.room.id}).`);
        reload();
      }
    } catch (err) {
      if (dry) setPreview(null);
      setSaid(/** @type {Error} */ (err).message);
    } finally {
      setBusy(false);
    }
  };
  /** @param {Event} e */
  const pick = (e) => {
    const f = /** @type {HTMLInputElement} */ (e.currentTarget).files?.[0] || null;
    setFile(f);
    setPreview(null);
    setSaid('');
  };
  const p = preview;
  return html`
    <button type="button" id="import-open" class="btn-secondary" onClick=${() => sheet.current?.showModal()}>Import a room</button>
    <dialog ref=${sheet} id="import-sheet" class="sheet wide" aria-labelledby="import-title">
    <div class="sheet-head"><h2 id="import-title">Import a room</h2>
      <button type="button" class="btn-icon btn-ghost" aria-label="Close" onClick=${() => sheet.current?.close()}>✕</button></div>
    <p class="hint">An export from this instance or another one becomes a new room. Nothing is created until you confirm the preview.</p>
    <form class="row" id="import-form" onSubmit=${formAction(() => send(true))}>
      <label>Archive (.tar.gz)<input id="import-file" type="file" accept=".gz,.tgz,application/gzip" required onChange=${pick}/></label>
      <button type="submit" disabled=${!file || busy}>Preview</button>
    </form>
    <p id="import-status" class="said" role="status">${said}</p>
    ${p && html`<section id="import-preview" aria-label="Import preview">
      <dl>
        <div><dt>Room</dt><dd id="import-name">${p.name}</dd></div>
        <div><dt>Format</dt><dd>${p.format}</dd></div>
        <div><dt>Exported by</dt><dd id="import-version">${p.instance_version || 'unknown'}${p.same_instance ? ', this instance' : ', another instance'}</dd></div>
        <div><dt>Exported</dt><dd>${when(p.exported_at)}</dd></div>
        <div><dt>Archive</dt><dd>${size(p.archive_bytes)}</dd></div>
        <div><dt>Uploads and attachments</dt><dd>${size((p.bytes.uploads || 0) + (p.bytes.attachments || 0))}</dd></div>
        <div><dt>Agent sessions</dt><dd>${size(p.bytes.sessions || 0)}</dd></div>
      </dl>
      <dl id="import-counts">${importKinds.map(([k, label]) => html`<div><dt>${label}</dt><dd>${p.counts[k] || 0}</dd></div>`)}</dl>
      ${p.warnings.length > 0 && html`<ul id="import-warnings" class="warn">${p.warnings.map((w) => html`<li>${w}</li>`)}</ul>`}
      <div class="row"><button type="button" id="import-confirm" disabled=${busy} onClick=${() => send(false)}>Import as a new room</button></div>
    </section>`}
    </dialog>`;
}

function Profiles() {
  const [list, setList] = useState(/** @type {Profile[] | null} */ (null));
  const [perProfile, setPerProfile] = useState(0);
  const load = () => api('GET', '/api/admin/profiles').then(setList).catch(show);
  useEffect(() => {
    load();
    api('GET', '/api/admin/settings').then((/** @type {Settings} */ v) => setPerProfile(v.rooms_per_profile)).catch(show);
  }, []);
  /** @param {Profile} p */
  const remove = (p) => {
    window.grillConfirm(`Delete the profile of ${p.name || p.uuid}?`, 'Its participants stay in their rooms, detached.', 'Delete')
      .then((yes) => { if (yes) api('DELETE', '/api/admin/profiles/' + p.id).then(load).catch(show); });
  };
  return html`
    <h2>Profiles</h2>
    <p class="hint">Emails show here and nowhere else. A profile's name is the one it chose in a room, else the one it last joined a room with.</p>
    ${list && (list.length ? html`<div class="scroll"><table id="profiles">
      <thead><tr><th>Name</th><th>Email</th><th>Rooms</th><th>Skills</th><th>Last activity</th><th>Rooms it may create</th><th></th></tr></thead>
      <tbody>${list.map((p) => html`<tr id=${'profile-' + p.id}>
        <td>${p.name || '—'} <code title=${p.uuid}>${p.uuid.slice(0, 8)}</code></td><td>${p.email || '—'}</td>
        <td class="wrap">${p.rooms.map((r) => r.room_name).join(', ') || '—'}</td><td>${p.skills}</td><td>${when(p.last_activity)}</td>
        <td><${RoomLimit} key=${p.id} p=${p} perProfile=${perProfile} reload=${load}/></td>
        <td><button type="button" onClick=${() => remove(p)}>Delete</button></td>
      </tr>`)}</tbody>
    </table></div>` : html`<p>No profile yet.</p>`)}`;
}

/**
 * RoomLimit sets a profile's own limit on the rooms it creates: a number, unlimited, or the
 * instance's (004-AC7).
 * @param {{p: Profile, perProfile: number, reload: () => void}} props
 */
function RoomLimit({ p, perProfile, reload }) {
  const [n, setN] = useState(p.max_rooms && p.max_rooms > 0 ? String(p.max_rooms) : '');
  /** @param {number | null} v */
  const set = (v) => api('PUT', `/api/admin/profiles/${p.id}/max_rooms`, { max_rooms: v }).then(reload).catch(show);
  const now = p.max_rooms === null ? `instance's (${perProfile})` : p.max_rooms === -1 ? 'unlimited' : String(p.max_rooms);
  return html`<form class="limit" onSubmit=${formAction(() => set(Number(n)))}>
    <span class="hint">${p.rooms_created} created, limit ${now}</span>
    <input type="number" min="1" max="1000" step="1" required value=${n} onInput=${onInput(setN)} aria-label=${'Rooms ' + (p.name || p.uuid) + ' may create'}/>
    <button type="submit">Set</button>
    <button type="button" onClick=${() => set(-1)} disabled=${p.max_rooms === -1}>Unlimited</button>
    <button type="button" onClick=${() => set(null)} disabled=${p.max_rooms === null}>Instance's</button>
  </form>`;
}

function Libraries() {
  const [templates, setTemplates] = useState(/** @type {Template[] | null} */ (null));
  const [skills, setSkills] = useState(/** @type {Skill[] | null} */ (null));
  const [starters, setStarters] = useState(/** @type {import('./starters.js').Starter[] | null} */ (null));
  const load = () => Promise.all([
    api('GET', '/api/admin/starters').then(setStarters),
    api('GET', '/api/admin/templates').then(setTemplates),
    api('GET', '/api/admin/skills').then((/** @type {Skill[]} */ ks) => setSkills(ks.filter((k) => k.shared))),
  ]).catch(show);
  useEffect(() => { load(); }, []);
  /** @param {string} what @param {string} path */
  const remove = (what, path) => {
    window.grillConfirm(`Remove ${what} from the library?`, 'Rooms keep the copies they use.', 'Remove')
      .then((yes) => { if (yes) api('DELETE', path).then(load).catch(show); });
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
    </table></div>` : html`<p>No shared skill yet.</p>`)}
    <h2>Shared room starters</h2>
    ${starters && (starters.length ? html`<div class="scroll"><table id="starters" class="cards">
      <thead><tr><th>Title</th><th>Author</th><th>Versions</th><th>Size</th><th>Updated</th><th></th></tr></thead>
      <tbody>${starters.map((st) => html`<tr id=${'starter-' + st.id}>
        <td class="wrap">${st.title}</td><td data-label="Author">${st.author}</td><td data-label="Versions">${st.versions}</td>
        <td data-label="Size">${size(st.size)}</td><td data-label="Updated">${ago(st.updated_at)}</td>
        <td><button type="button" class="btn-danger-outline" onClick=${() => remove(`the starter ${st.title}`, '/api/admin/starters/' + st.id)}
          aria-label=${'Remove the starter ' + st.title}>Remove</button></td>
      </tr>`)}</tbody>
    </table></div>` : html`<p>No shared starter yet.</p>`)}`;
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
    <dl id="instance">${rows.map(([k, v]) => html`<div><dt>${k}</dt><dd>${v}</dd></div>`)}</dl>
    <${RoomCreation}/>`;
}

// RoomCreation sets the guardrails on rooms profiles create from their profile page (004-AC7).
function RoomCreation() {
  const [per, setPer] = useState('');
  const [quota, setQuota] = useState('');
  const [mode, setMode] = useState('anyone');
  const [said, setSaid] = useState('');
  /** @param {Settings} v */
  const fill = (v) => {
    setPer(String(v.rooms_per_profile));
    setQuota(v.storage_quota_bytes ? String(Math.round(v.storage_quota_bytes / 1048576)) : '');
    setMode(v.room_creation);
  };
  useEffect(() => { api('GET', '/api/admin/settings').then(fill).catch(show); }, []);
  const save = () => api('PUT', '/api/admin/settings', {
    rooms_per_profile: Number(per), storage_quota_bytes: Math.round(Number(quota || 0) * 1048576), room_creation: mode,
  }).then((/** @type {Settings} */ v) => { fill(v); setSaid('Saved.'); }).catch(show);
  return html`
    <h2>Room creation</h2>
    <p class="hint">Profiles create rooms from their profile page and administer them. These limits apply to them, not to you.</p>
    <form class="row" id="room-creation" onSubmit=${formAction(save)}>
      <label>Who creates rooms
        <select id="room-creation-mode" value=${mode} onChange=${onInput(setMode)}>
          <option value="anyone">Anyone with a profile</option>
          <option value="super-admin">The instance admin only</option>
        </select></label>
      <label>Rooms per profile<input id="rooms-per-profile" type="number" min="1" max="1000" step="1" required value=${per} onInput=${onInput(setPer)}/></label>
      <label>Storage quota (MB, empty for none)<input id="storage-quota" type="number" min="1" step="1" value=${quota} onInput=${onInput(setQuota)}/></label>
      <button type="submit">Save</button>
      <span class="said" role="status">${said}</span>
    </form>`;
}

function Shell() {
  const logout = () => api('POST', '/api/admin/logout').then(() => { signedIn.value = false; }).catch(show);
  const view = route.value.view;
  return html`
    <header class="app-header">
      <img src="/static/logo.svg" alt="" class="logo" width="32" height="32"/>
      <h1>grill-room · Admin</h1>
      <button type="button" id="logout" class="btn-secondary" onClick=${logout}>Sign out</button>
    </header>
    <${TabBar} tabs=${views} active=${view} href=${(/** @type {string} */ id) => '#' + id} label="Views" cls="views"/>
    <main>
      ${notice.value && html`<p class="said bad" role="alert">${notice.value}</p>`}
      ${{ rooms: html`<${Rooms}/>`, profiles: html`<${Profiles}/>`, libraries: html`<${Libraries}/>`, instance: html`<${InstanceView}/>`,
        sessions: html`<${Sessions}/>`, audit: html`<${Audit}/>` }[view]}
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
