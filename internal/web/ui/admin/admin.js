// The server admin page (004-AC1 to 004-AC6, 004-AC9, 004-AC19, 004b): Preact, htm and signals,
// vendored, no build step. The session cookie is HttpOnly: the page never sees a token after the
// login form.
import { render } from 'preact';
import { useState, useEffect, useRef } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';
import { RoomPanel, AdminRows, RowMenu, Meter, TabBar, fetchJSON, roomTabs, size, when, ago, onInput, formAction, icon, uaName } from './room-panel.js';

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
 * @typedef {{rooms_per_profile: number, storage_quota_bytes: number, room_creation: string, purge_empty_profiles: boolean}} Settings
 * @typedef {{id: number, uuid: string, name: string, email: string}} MergeSide
 * @typedef {{keep: number, merge: number}} MergeCount
 * @typedef {{keep: MergeSide, merge: MergeSide, rooms: {room_id: string, room_name: string, keep: boolean, merge: boolean}[],
 *   skills: {keep: string[], merge: string[], renamed: {from: string, to: string}[]}, seats: MergeCount, shared_skills: MergeCount, templates: MergeCount,
 *   starters: MergeCount, rooms_created: MergeCount, room_admin: {keep: string[], merge: string[]},
 *   over_limits: {kind: string, after: number, limit: number}[], email_after: string, api_tokens: number}} MergePreview
 * @typedef {import('./room-panel.js').Member} Participant
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
const views = [['rooms', 'Rooms'], ['profiles', 'Profiles'], ['libraries', 'Libraries'], ['pronunciation', 'Pronunciation'], ['instance', 'Server'],
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

// The room panel says what it did through a toast (011-AC20), the kit's, as on every page.
addEventListener('grill:toast', (e) => window.grillToast(/** @type {CustomEvent<string>} */ (e).detail));

/** @param {string} text */
const toast = (text) => dispatchEvent(new CustomEvent('grill:toast', { detail: text }));

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
  const revoke = (s) => api('DELETE', `/api/admin/sessions/${s.id}`).then(load).catch(show);
  return html`
    <h2>Sessions</h2>
    <p class="hint">Browsers signed in with the admin token, for 12 hours each. Revoking one signs it out at its next request. Sign out of this one from the header.</p>
    ${list && html`<div class="cols" id="sessions">
      <div class="list-row head"><span>Browser</span><span>Signed in</span><span>Last used</span><span>Expires</span><span></span></div>
      <ul class="list-rows">${list.map((s) => html`<li class="list-row" id=${'session-' + s.id} key=${s.id}>
        <div class="main"><span class="name" data-tip=${s.user_agent}>${uaName(s.user_agent)}</span>${s.current && html` <span class="badge">this browser</span>`}
          <div class="meta">${s.ip}</div></div>
        <div class="cells"><span class="cell signed-in" data-label="Signed in">${ago(s.created_at)}</span><span class="cell" data-label="Last used">${ago(s.last_used_at)}</span>
          <span class="cell" data-label="Expires">${when(s.expires_at)}</span></div>
        <div class="actions">${!s.current && html`<${RowMenu} id=${'session-menu-' + s.id} label=${'More actions on the session ' + s.id}
          items=${[['Revoke', () => revoke(s), 'danger', 'Revoke the session of ' + uaName(s.user_agent) + ' at ' + s.ip]]}/>`}</div>
      </li>`)}</ul></div>`}`;
}

/**
 * actionLabels name the audit actions in words (docs/design.md §8); an action without one shows
 * its code. A test checks every action of the server has one.
 * @type {Record<string, string>}
 */
const actionLabels = {
  'room.create': 'Room created', 'room.rename': 'Room renamed', 'room.delete': 'Room deleted', 'room.quota': 'Room quota set', 'room.speech': 'Call voices changed',
  'room.export': 'Room exported', 'room.import': 'Room imported', 'room.host_transfer': 'AI host changed',
  'invite.create': 'Invite created', 'invite.revoke': 'Invite revoked', 'room_admin.add': 'Room admin added', 'room_admin.remove': 'Room admin removed',
  'room_skill.remove': 'Room skill removed', 'participant.rename': 'Participant renamed', 'document.delete': 'Document deleted',
  'participant.remove': 'Participant removed', 'participant.unblock': 'Participant unblocked',
  'document.restore': 'Document restored', 'profile.delete': 'Profile deleted', 'profile.recovery_link': 'Recovery link made',
  'profile.recovery_link_used': 'Recovery link used', 'profile.merge': 'Profiles merged', 'profile.purge': 'Empty profiles deleted',
  'profile.max_rooms': 'Room limit set', 'template.remove': 'Persona template removed', 'skill.remove': 'Shared skill removed',
  'starter.publish': 'Starter shared', 'starter.delete': 'Starter removed', 'instance.settings': 'Server settings changed',
  'tts.lexicon.add': 'Pronunciation added', 'tts.lexicon.edit': 'Pronunciation changed', 'tts.lexicon.delete': 'Pronunciation removed',
  'api_token.create': 'API token created', 'api_token.revoke': 'API token revoked', 'persona.remove': 'Persona removed',
  'egress.allow': 'Website allowed', 'egress.revoke': 'Website removed', 'egress.delegate': 'Website rights of room admins changed',
  'connector.enable': 'Connector turned on', 'connector.disable': 'Connector turned off', 'connector.actions': 'Connector actions changed',
  'admin.login': 'Admin signed in', 'admin.logout': 'Admin signed out', 'admin.session_revoke': 'Admin session revoked',
};
/** @param {string} a */
const actionLabel = (a) => actionLabels[a] || a;

/**
 * detailText reads an entry's details as "key: value" pairs, not JSON.
 * @param {Record<string, unknown>} d
 */
const detailText = (d) => Object.entries(d || {}).map(([k, v]) => k.replace(/_/g, ' ') + ': ' + (v !== null && typeof v === 'object' ? JSON.stringify(v) : String(v))).join(' · ');

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
  const pager = html`<div class="pager">
      <button type="button" class="btn-secondary" onClick=${back} disabled=${!newer.length}>Newer</button>
      <button type="button" class="btn-secondary" onClick=${older} disabled=${!page?.next}>Older</button></div>`;
  return html`
    <h2>Audit log</h2>
    <p class="hint">Every admin action, kept a year. Read only.</p>
    <div class="row filters">
      <label>Action
        <select id="audit-action" onChange=${filter(setAction)}>
          <option value="">Any action</option>
          ${(page?.actions || []).map((a) => html`<option value=${a} selected=${a === action}>${actionLabel(a)}</option>`)}
        </select>
      </label>
      <label>Target (id or name)
        <input id="audit-target" type="search" onChange=${filter(setTarget)}/>
      </label>
      <span class="spacer"></span>${pager}
    </div>
    ${page && (page.entries.length ? html`<div class="cols" id="audit">
      <div class="list-row head"><span>Action</span><span>When</span><span>Who</span><span>Details</span></div>
      <ul class="list-rows">${page.entries.map((e) => html`<li class="list-row" id=${'audit-' + e.id} key=${e.id}>
        <div class="main"><span class="name" data-tip=${e.action}>${actionLabel(e.action)}</span>
          <div class="meta">${e.target_kind.replace(/_/g, " ")} ${e.target_name && html`<strong>${e.target_name}</strong>`} <span data-tip=${e.target_id}>${e.target_id.length > 12 ? e.target_id.slice(0, 8) + '…' : e.target_id}</span></div></div>
        <div class="cells"><span class="cell" data-label="When">${ago(e.at)}</span><span class="cell" data-label="Who">${actor(e)}</span>
          <span class="cell details" data-tip=${detailText(e.details)}>${detailText(e.details)}</span></div>
      </li>`)}</ul></div>${pager}` : html`<p class="empty">No entry matches.</p>`)}`;
}

/** @type {[string, string, (r: Room) => string | number | boolean][]} */
const roomCols = [
  ['name', 'Name', (r) => r.name.toLowerCase()],
  ['activity', 'Last activity', (r) => r.last_activity],
  ['participants', 'People', (r) => r.participants],
  ['personas', 'Personas', (r) => r.personas],
  ['host', 'AI host', (r) => r.host_online],
  ['quota', 'Storage', (r) => r.used / r.quota],
];

/** @param {Room} r */
const hostPill = (r) => (!r.host_participant_id ? html`<span class="status off">No AI host</span>`
  : r.host_online ? html`<span class="status ok">Online</span>` : html`<span class="status idle">Offline</span>`);

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
      <p><a href="#rooms" id="back-to-rooms" class="back" dangerouslySetInnerHTML=${{ __html: window.grillIconHTML('back') + 'All rooms' }}></a></p>
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
  /** @param {Room} r @param {string} t */
  const go = (r, t) => () => { location.hash = `#rooms/${encodeURIComponent(r.id)}/${t}`; };
  return html`
    <div class="list-head">
      <h2>Rooms (${q ? rooms.length + ' of ' + page.rooms.length : page.rooms.length})</h2>
      <input type="search" id="room-search" aria-label="Search rooms" placeholder="Search rooms…" value=${search} onInput=${onInput(setSearch)}/>
      <${ImportRoom} reload=${load}/>
    </div>
    <p class="hint">A room's quota counts its uploads and attachments; the default is ${size(page.default_quota)}.</p>
    ${page.rooms.length ? html`<div class="cols" id="rooms">
      <div class="list-row head">${roomCols.map(([id, label]) => html`<span aria-sort=${sort.col === id ? (sort.desc ? 'descending' : 'ascending') : undefined}>
        <button type="button" class="sort" onClick=${() => by(id)}>${label}</button></span>`)}<span></span></div>
      <ul class="list-rows">${rooms.map((r) => html`<li class="list-row" id=${'room-' + r.id} key=${r.id}>
        <div class="main"><a class="name" href=${'#rooms/' + encodeURIComponent(r.id)} aria-label=${'Manage ' + r.name}>${r.name}</a>
          <div class="meta">created ${ago(r.created_at)}</div></div>
        <div class="cells"><span class="cell" data-label="Last activity">${ago(r.last_activity)}</span>
          <span class="cell" data-label="People">${r.participants}</span><span class="cell" data-label="Personas">${r.personas}</span>
          <span class="cell">${hostPill(r)}</span>
          <span class="cell storage"><${Meter} used=${r.used} max=${r.quota} label=${size(r.used) + ' of ' + size(r.quota)}/> ${size(r.used)} / ${size(r.quota)}</span></div>
        <div class="actions"><${RowMenu} id=${'room-menu-' + r.id} label=${'More actions on ' + r.name} items=${[
          ['Manage', go(r, 'general')], ['Invites', go(r, 'invites')], ['Admins', go(r, 'admins')], ['Export', go(r, 'export')]]}/></div>
      </li>`)}</ul></div>${q && !rooms.length && html`<p class="hint" id="rooms-none">No room matches "${search.trim()}".</p>`}` : html`<p class="empty">No room yet.</p>`}`;
}

/**
 * RoomAdmins appoints and removes room admins among the room's participants with a profile: a room
 * the super-admin created starts with none (004-AC8). Same rows as the room's own Admins tab.
 * @param {{base: string}} props
 */
function RoomAdmins({ base }) {
  const [list, setList] = useState(/** @type {Participant[] | null} */ (null));
  const load = () => api('GET', base + '/participants').then(setList).catch(show);
  useEffect(() => { load(); }, [base]);
  /** @param {Participant} p */
  const makeHost = (p) => {
    window.grillConfirm(`Make ${p.name} the AI host?`, `The personas run on ${p.name}'s computer from the next turn.`, 'Make AI host')
      .then((yes) => { if (yes) api('PUT', base + '/host', { participant_id: p.id }).then(load).catch(show); });
  };
  return html`
    <p class="hint">Room admins rename the room and its participants, manage its invites, appoint other admins, export and delete it. Only participants with a profile can be appointed. Make AI host, in a row's ⋯, shows for those whose AI host app is connected and can host.</p>
    ${list && (list.length ? html`<${AdminRows} people=${list} base=${base} api=${api} show=${show} done=${load} makeHost=${makeHost}/>`
      : html`<p class="empty">No participant yet.</p>`)}`;
}

/** @type {[string, string][]} */
const importKinds = [['participants', 'Participants'], ['messages', 'Lines'], ['message_versions', 'Earlier versions'],
  ['documents', 'Documents'], ['revisions', 'Revisions'], ['uploads', 'Uploads'], ['annotations', 'Annotations'],
  ['annotation_messages', 'Thread messages'], ['attachments', 'Attachments'], ['mentions', 'Mentions'], ['personas', 'Personas'], ['skills', 'Skills'],
  ['plugins', 'Plugins'], ['sessions', 'Agent sessions']];

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
      <button type="button" class="btn-icon btn-ghost" aria-label="Close" onClick=${() => sheet.current?.close()} dangerouslySetInnerHTML=${icon('close')}></button></div>
    <p class="hint">An export from this server or another one becomes a new room. Nothing is created until you confirm the preview.</p>
    <form class="row" id="import-form" onSubmit=${formAction(() => send(true))}>
      <label>Archive (.tar.gz)<input id="import-file" type="file" accept=".gz,.tgz,application/gzip" required onChange=${pick}/></label>
      <button type="submit" disabled=${!file || busy}>Preview</button>
    </form>
    <p id="import-status" class="said" role="status">${said}</p>
    ${p && html`<section id="import-preview" aria-label="Import preview">
      <dl>
        <div><dt>Room</dt><dd id="import-name">${p.name}</dd></div>
        <div><dt>Format</dt><dd>${p.format}</dd></div>
        <div><dt>Exported by</dt><dd id="import-version">${p.instance_version || 'unknown'}${p.same_instance ? ', this server' : ', another server'}</dd></div>
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
  const [merging, setMerging] = useState(0);
  const [limiting, setLimiting] = useState(0);
  const [recovering, setRecovering] = useState(0);
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
  /** @param {Profile} p */
  const limit = (p) => (p.max_rooms === null ? `${perProfile}, the server's` : p.max_rooms === -1 ? 'unlimited' : String(p.max_rooms));
  const byID = (/** @type {number} */ id) => list?.find((p) => p.id === id);
  return html`
    <h2>Profiles</h2>
    <p class="hint">Emails show here and nowhere else. A profile's name is the one it chose in a room, else the one it last joined a room with.</p>
    ${list && (list.length ? html`<div class="cols" id="profiles">
      <div class="list-row head"><span>Name</span><span>Rooms</span><span>Skills</span><span>Last activity</span><span>Rooms created</span><span></span></div>
      <ul class="list-rows">${list.map((p) => {
        const who = p.name || p.uuid;
        return html`<li class="list-row" id=${'profile-' + p.id} key=${p.id}>
        <div class="main"><span class="name" data-tip=${(p.name ? p.name + ' · ' : '') + 'Profile ' + p.uuid}>${p.name || 'no name'}</span><div class="meta">${p.email || 'no email'}</div></div>
        <div class="cells"><span class="cell rooms" data-tip=${p.rooms.map((r) => r.room_name).join(', ')}>${p.rooms.length} ${p.rooms.length === 1 ? 'room' : 'rooms'}</span>
          <span class="cell" data-label="Skills">${p.skills}</span><span class="cell" data-label="Last activity">${ago(p.last_activity)}</span>
          <span class="cell" data-label="Rooms created">${p.rooms_created} of ${limit(p)}</span></div>
        <div class="actions"><${RowMenu} id=${'profile-menu-' + p.id} label=${'More actions on ' + who} items=${[
          ['Set the room limit…', () => setLimiting(p.id), '', 'Set the room limit of ' + who],
          ['Generate a recovery link…', () => setRecovering(p.id), '', 'Generate a recovery link for ' + who],
          ['Merge into another profile…', () => setMerging(p.id), '', 'Merge ' + who + ' into another profile'],
          ['Delete…', () => remove(p), 'danger', 'Delete the profile of ' + who]]}/></div>
      </li>`;
      })}</ul></div>` : html`<p class="empty">No profile yet.</p>`)}
    ${list && merging > 0 && html`<${MergeSheet} key=${merging} profiles=${list} from=${merging} done=${() => { setMerging(0); load(); }}/>`}
    ${limiting > 0 && byID(limiting) && html`<${RoomLimit} key=${limiting} p=${/** @type {Profile} */ (byID(limiting))} perProfile=${perProfile}
      done=${() => setLimiting(0)} reload=${load}/>`}
    ${recovering > 0 && byID(recovering) && html`<${RecoveryLink} key=${recovering} p=${/** @type {Profile} */ (byID(recovering))} done=${() => setRecovering(0)}/>`}`;
}

/**
 * Sheet is a small sheet open while mounted; done runs once it closes, by Close, Escape or a click
 * outside.
 * @param {{id: string, title: string, done: () => void, children?: import('preact').ComponentChildren}} props
 */
function Sheet({ id, title, done, children }) {
  const ref = useRef(/** @type {HTMLDialogElement | null} */ (null));
  useEffect(() => { ref.current?.showModal(); }, []);
  /** @param {MouseEvent} e */
  const outside = (e) => {
    const d = /** @type {HTMLDialogElement} */ (e.currentTarget);
    const r = d.getBoundingClientRect();
    if (e.target === d && (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom)) d.close();
  };
  return html`<dialog ref=${ref} id=${id} class="sheet" aria-labelledby=${id + '-title'} onClose=${done} onClick=${outside}>
    <div class="sheet-head"><h2 id=${id + '-title'}>${title}</h2>
      <button type="button" class="btn-icon btn-ghost" aria-label="Close" onClick=${() => ref.current?.close()} dangerouslySetInnerHTML=${icon('close')}></button></div>
    ${children}
  </dialog>`;
}

/** @param {Profile | MergeSide} p */
const label = (p) => `${p.name || '—'}${p.email ? ' · ' + p.email : ''} · ${p.uuid.slice(0, 8)}`;
/** @param {string[]} names */
const listed = (names) => (names.length ? new Intl.ListFormat('en').format(names) : 'none');
/** @type {[string, string, string][]} */
const mergeKinds = [['seats', 'seat', 'seats'], ['skills', 'personal skill', 'personal skills'], ['shared_skills', 'shared skill', 'shared skills'],
  ['templates', 'persona template', 'persona templates'], ['starters', 'starter', 'starters'], ['rooms_created', 'room created', 'rooms created'],
  ['room_admins', 'room-admin role', 'room-admin roles'], ['sessions', 'sign-in', 'sign-ins']];
/** @type {[string, string, string][]} */
const revokedKinds = [['api_tokens_revoked', 'API token', 'API tokens']];
/** @type {Record<string, string>} */
const limitKinds = { skills: 'Personal skills', templates: 'Persona templates', starters: 'Starters', rooms_created: 'Rooms created' };

/**
 * MergeSheet merges profile B into profile A (011-AC10): it previews what moves and what A ends
 * with, swaps the two, and refuses one profile into itself. B defaults to the row it opened from.
 * @param {{profiles: Profile[], from: number, done: () => void}} props
 */
function MergeSheet({ profiles, from, done }) {
  const ref = useRef(/** @type {HTMLDialogElement | null} */ (null));
  const [keep, setKeep] = useState(0);
  const [merge, setMerge] = useState(from);
  const [preview, setPreview] = useState(/** @type {MergePreview | null} */ (null));
  const [busy, setBusy] = useState(false);
  const [said, setSaid] = useState('');
  // inflight, unlike busy, is current inside the close handler of the render that sent the merge.
  const inflight = useRef(false);
  useEffect(() => { ref.current?.showModal(); }, []);
  useEffect(() => {
    setPreview(null);
    setSaid('');
    if (!keep || !merge || keep === merge) return;
    // A slower answer to an older pair must not overwrite the current one.
    let current = true;
    api('GET', `/api/admin/profiles/merge-preview?keep=${keep}&merge=${merge}`)
      .then((p) => { if (current) setPreview(p); })
      .catch((/** @type {Error} */ err) => { if (current) setSaid(err.message); });
    return () => { current = false; };
  }, [keep, merge]);
  // A merge in flight keeps the sheet open: its error would have nowhere to show.
  const close = () => { if (!busy) ref.current?.close(); };
  // A second Escape closes a dialog whatever cancel does: reopen it until the answer comes.
  const closed = () => {
    if (inflight.current) ref.current?.showModal();
    else done();
  };
  const run = () => {
    inflight.current = true;
    setBusy(true);
    setSaid('');
    api('POST', '/api/admin/profiles/merge', { keep, merge }).then((/** @type {{keep: {name: string}, moved: Record<string, number>}} */ r) => {
      inflight.current = false;
      const count = (/** @type {[string, string, string][]} */ kinds) => kinds.filter(([k]) => r.moved?.[k]).map(([k, one, many]) => `${r.moved[k]} ${r.moved[k] === 1 ? one : many}`);
      const moved = count(mergeKinds);
      const revoked = count(revokedKinds);
      toast(`Merged into ${r.keep.name || 'the kept profile'}. ${moved.length ? 'Moved ' + listed(moved) + '.' : 'Nothing to move.'}${revoked.length ? ' Revoked ' + listed(revoked) + '.' : ''}`);
      ref.current?.close();
    }).catch((/** @type {Error} */ err) => {
      inflight.current = false;
      setSaid(err.message);
      setBusy(false);
    });
  };
  /** @param {MouseEvent} e */
  const outside = (e) => {
    const d = /** @type {HTMLDialogElement} */ (e.currentTarget);
    const r = d.getBoundingClientRect();
    if (!busy && e.target === d && (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom)) d.close();
  };
  /** @param {string} id @param {string} text @param {number} value @param {(v: number) => void} set */
  const pick = (id, text, value, set) => html`<label>${text}
    <select id=${id} value=${String(value)} disabled=${busy} onChange=${(/** @type {Event} */ e) => set(Number(/** @type {HTMLSelectElement} */ (e.currentTarget).value))}>
      <option value="0">Pick a profile</option>
      ${profiles.map((p) => html`<option value=${String(p.id)}>${label(p)}</option>`)}
    </select></label>`;
  const p = preview;
  const same = keep > 0 && keep === merge;
  return html`<dialog ref=${ref} id="merge-sheet" class="sheet wide full" aria-labelledby="merge-title" onClose=${closed} onClick=${outside}
    onCancel=${(/** @type {Event} */ e) => { if (busy) e.preventDefault(); }}>
    <div class="sheet-head"><h2 id="merge-title">Merge two profiles?</h2>
      <button type="button" class="btn-icon btn-ghost" aria-label="Close" disabled=${busy} onClick=${close} dangerouslySetInnerHTML=${icon('close')}></button></div>
    <p class="hint">Everything of the merged profile moves to the kept one, then the merged profile is deleted. This cannot be undone.</p>
    <div class="row merge-pick">
      ${pick('merge-keep', 'Keep', keep, setKeep)}
      <button type="button" id="merge-swap" class="btn-secondary" disabled=${busy} onClick=${() => { setKeep(merge); setMerge(keep); }}>Swap</button>
      ${pick('merge-from', 'Merge into it', merge, setMerge)}
    </div>
    ${same && html`<p id="merge-same" class="said bad">A profile cannot be merged into itself.</p>`}
    ${p && html`<section id="merge-preview" aria-label="Merge preview">
      <dl>
        <div><dt>Keep</dt><dd id="merge-keep-who">${p.keep.name || '—'} · ${p.keep.email || 'no email'}</dd></div>
        <div><dt>Merge into it</dt><dd id="merge-from-who">${p.merge.name || '—'} · ${p.merge.email || 'no email'}</dd></div>
        <div><dt>Email after the merge</dt><dd id="merge-email">${p.email_after || 'none'}</dd></div>
      </dl>
      <h3>Rooms</h3>
      ${p.rooms.length ? html`<ul id="merge-rooms">${p.rooms.map((r) => html`<li key=${r.room_id}>${r.room_name}${r.keep && r.merge
        ? html` <span class="badge">both</span>` : ''}</li>`)}</ul>` : html`<p>No room.</p>`}
      <h3>Personal skills</h3>
      <dl id="merge-skills">
        <div><dt>Kept</dt><dd>${listed(p.skills.keep)}</dd></div>
        <div><dt>Moved</dt><dd>${listed(p.skills.merge)}</dd></div>
      </dl>
      ${p.skills.renamed.length > 0 && html`<ul id="merge-renamed">${p.skills.renamed.map((x) => html`<li>${x.from} becomes ${x.to}</li>`)}</ul>`}
      <h3>The rest</h3>
      <dl id="merge-counts">
        ${[['Seats', p.seats], ['Shared skills', p.shared_skills], ['Persona templates', p.templates], ['Starters', p.starters], ['Rooms created', p.rooms_created]]
          .map(([k, n]) => html`<div><dt>${k}</dt><dd>${(/** @type {MergeCount} */ (n)).keep} + ${(/** @type {MergeCount} */ (n)).merge}</dd></div>`)}
        <div><dt>Room admin, kept</dt><dd>${listed(p.room_admin.keep)}</dd></div>
        <div><dt>Room admin, moved</dt><dd>${listed(p.room_admin.merge)}</dd></div>
        <div><dt>API tokens, revoked</dt><dd id="merge-tokens">${p.api_tokens}</dd></div>
      </dl>
      ${p.over_limits.length > 0 && html`<ul id="merge-limits" class="warn">${p.over_limits.map((o) => html`<li>
        ${limitKinds[o.kind] || o.kind}: ${o.after} after the merge, over the limit of ${o.limit}. The profile keeps them all and adds no more until under it.</li>`)}</ul>`}
    </section>`}
    <p class="said error" role="alert">${said}</p>
    <div class="actions">
      <button type="button" class="btn-secondary" disabled=${busy} onClick=${close}>Cancel</button>
      <button type="button" id="merge-confirm" class="btn-danger" disabled=${busy || !p || same} onClick=${run}>Merge</button>
    </div>
  </dialog>`;
}

/**
 * RecoveryLink makes a one-time recovery link for a profile as it opens and shows it once, to copy
 * and hand over (011-AC1): the server keeps only its hash. Another one voids it.
 * @param {{p: Profile, done: () => void}} props
 */
function RecoveryLink({ p, done }) {
  const [link, setLink] = useState(/** @type {{url: string, expires_at: string} | null} */ (null));
  const [said, setSaid] = useState('');
  const field = useRef(/** @type {HTMLInputElement | null} */ (null));
  const who = p.name || p.uuid;
  useEffect(() => {
    api('POST', `/api/admin/profiles/${p.id}/recovery-link`).then(setLink).catch((/** @type {Error} */ err) => setSaid(err.message));
  }, []);
  // A refused clipboard leaves the link selected for a manual copy.
  const copy = () => {
    if (link) navigator.clipboard.writeText(link.url).then(() => toast('Copied'), () => field.current?.select());
  };
  return html`<${Sheet} id="recovery-sheet" title=${'Recovery link for ' + who} done=${done}>
    <p class="hint">It signs in as this profile once. Hand it over yourself; a new one voids it.</p>
    ${link && html`<label class="link">Recovery link<input ref=${field} readonly value=${link.url} aria-label=${'Recovery link for ' + who}/></label>
      <p class="hint">Expires ${when(link.expires_at)}.</p>
      <div class="actions"><button type="button" class="btn-primary" onClick=${copy}>Copy</button></div>`}
    <p class="said error" role="alert">${said}</p>
  <//>`;
}

/**
 * RoomLimit sets a profile's own limit on the rooms it creates: a number, unlimited, or the
 * server's (004-AC7).
 * @param {{p: Profile, perProfile: number, reload: () => void, done: () => void}} props
 */
function RoomLimit({ p, perProfile, reload, done }) {
  const [n, setN] = useState(p.max_rooms && p.max_rooms > 0 ? String(p.max_rooms) : '');
  const ref = useRef(/** @type {HTMLFormElement | null} */ (null));
  const who = p.name || p.uuid;
  /** @param {number | null} v */
  const set = (v) => api('PUT', `/api/admin/profiles/${p.id}/max_rooms`, { max_rooms: v }).then(() => {
    reload();
    toast('Room limit of ' + who + ' set');
    ref.current?.closest('dialog')?.close();
  }).catch(show);
  const now = p.max_rooms === null ? `the server's (${perProfile})` : p.max_rooms === -1 ? 'unlimited' : String(p.max_rooms);
  return html`<${Sheet} id="limit-sheet" title=${'Rooms ' + who + ' may create'} done=${done}>
    <p class="hint">${p.rooms_created} created so far; the limit is ${now}.</p>
    <form ref=${ref} class="limit" onSubmit=${formAction(() => set(Number(n)))}>
      <label>Limit<input type="number" min="1" max="1000" step="1" required value=${n} onInput=${onInput(setN)} aria-label=${'Rooms ' + who + ' may create'}/></label>
      <div class="actions">
        <button type="button" class="btn-secondary" onClick=${() => set(null)} disabled=${p.max_rooms === null}>Use the server's</button>
        <button type="button" class="btn-secondary" onClick=${() => set(-1)} disabled=${p.max_rooms === -1}>Unlimited</button>
        <button type="submit">Set</button>
      </div>
    </form>
  <//>`;
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
  /** @param {string} id @param {string} what @param {string} path */
  const menu = (id, what, path) => html`<${RowMenu} id=${id} label=${'More actions on ' + what}
    items=${[['Remove…', () => remove(what, path), 'danger', 'Remove ' + what]]}/>`;
  return html`
    <h2>Persona templates</h2>
    ${templates && (templates.length ? html`<div class="cols" id="templates">
      <div class="list-row head"><span>Title</span><span>Author</span><span>Version</span><span>Updated</span><span></span></div>
      <ul class="list-rows">${templates.map((t) => html`<li class="list-row" id=${'template-' + t.id} key=${t.id}>
        <div class="main"><span class="name">${t.title}</span>${t.preset && html` <span class="badge" data-tip="Ships with grill-room: it cannot be removed.">shipped</span>`}</div>
        <div class="cells"><span class="cell" data-label="Author">${t.author}</span><span class="cell" data-label="Version">v${t.version}</span>
          <span class="cell" data-label="Updated">${ago(t.created_at)}</span></div>
        <div class="actions">${!t.preset && menu('template-menu-' + t.id, `the template ${t.title}`, '/api/admin/templates/' + t.id)}</div>
      </li>`)}</ul></div>` : html`<p class="empty">No persona template yet.</p>`)}
    <h2>Shared skills</h2>
    ${skills && (skills.length ? html`<div class="cols" id="skills">
      <div class="list-row head"><span>Name</span><span>Author</span><span>Version</span><span>Size</span><span>Updated</span><span></span></div>
      <ul class="list-rows">${skills.map((k) => html`<li class="list-row" id=${'skill-' + k.id} key=${k.id}>
        <div class="main"><span class="name">${k.name}</span></div>
        <div class="cells"><span class="cell" data-label="Author">${k.author}</span><span class="cell" data-label="Version">v${k.version}</span>
          <span class="cell" data-label="Size">${size(k.size)}</span><span class="cell" data-label="Updated">${ago(k.updated_at)}</span></div>
        <div class="actions">${menu('skill-menu-' + k.id, `the skill ${k.name}`, '/api/admin/skills/' + k.id)}</div>
      </li>`)}</ul></div>` : html`<p class="empty">No shared skill yet.</p>`)}
    <h2>Shared room starters</h2>
    ${starters && (starters.length ? html`<div class="cols" id="starters">
      <div class="list-row head"><span>Title</span><span>Author</span><span>Versions</span><span>Size</span><span>Updated</span><span></span></div>
      <ul class="list-rows">${starters.map((st) => html`<li class="list-row" id=${'starter-' + st.id} key=${st.id}>
        <div class="main"><span class="name">${st.title}</span></div>
        <div class="cells"><span class="cell" data-label="Author">${st.author}</span><span class="cell" data-label="Versions">${st.versions}</span>
          <span class="cell" data-label="Size">${size(st.size)}</span><span class="cell" data-label="Updated">${ago(st.updated_at)}</span></div>
        <div class="actions">${menu('starter-menu-' + st.id, `the starter ${st.title}`, '/api/admin/starters/' + st.id)}</div>
      </li>`)}</ul></div>` : html`<p class="empty">No shared starter yet.</p>`)}`;
}

/** @typedef {{term: string, style: 'en' | 'fr', fr_en: string, fr_fr: string, en: string}} LexTerm */
/** @typedef {LexTerm & {id: number, by: string, at: string}} LexEntry */
/** @type {Record<string, string>} */
const styleName = { en: 'English', fr: 'French' };
/** @param {LexTerm} t */
const termBody = (t) => ({ term: t.term, style: t.style, fr_en: t.fr_en, fr_fr: t.fr_fr, en: t.en });

/** Pronunciation is the call voices' lexicon (013-AC31): how a term is said, English- or French-style. */
function Pronunciation() {
  const [list, setList] = useState(/** @type {{entries: LexEntry[], max: number} | null} */ (null));
  const [q, setQ] = useState('');
  // editing: 0 closed, -1 a new term, else the entry's id.
  const [editing, setEditing] = useState(0);
  const load = () => api('GET', '/api/admin/pronunciations').then(setList).catch(show);
  useEffect(() => { load(); }, []);
  /** @param {LexEntry} e */
  const flip = (e) => api('PUT', '/api/admin/pronunciations/' + e.id, { ...termBody(e), style: e.style === 'en' ? 'fr' : 'en' })
    .then(() => { load(); toast(`${e.term} is said ${e.style === 'en' ? 'French' : 'English'}-style`); }).catch(show);
  /** @param {LexEntry} e */
  const remove = (e) => {
    window.grillConfirm(`Remove ${e.term} from the lexicon?`, 'The voices then read it the usual way: as written, an acronym letter by letter.', 'Remove')
      .then((yes) => { if (yes) api('DELETE', '/api/admin/pronunciations/' + e.id).then(load).catch(show); });
  };
  const needle = q.trim().toLowerCase();
  const shown = list?.entries.filter((e) => !needle || [e.term, e.fr_en, e.fr_fr, e.en].some((v) => v.toLowerCase().includes(needle))) ?? [];
  // A phone shows the French form the term's style says, the one a French voice uses.
  /** @param {string} label @param {string} v @param {boolean} used */
  const form = (label, v, used) => html`<span class=${used ? 'cell' : 'cell aside'} data-label=${label}>${v || html`<span class="meta">usual</span>`}</span>`;
  const full = !!list && list.entries.length >= list.max;
  return html`
    <h2>Pronunciation</h2>
    <p class="hint">How the call voices say a term. English-style says it the English way in a French sentence, French-style the French way. A form left empty reads the term the usual way: as written, an acronym letter by letter.</p>
    ${list && html`<div class="row filters">
      <label>Search<input id="term-search" type="search" value=${q} onInput=${onInput(setQ)}/></label>
      <span class="meta">${list.entries.length} of ${list.max} terms</span><span class="spacer"></span>
      <button type="button" id="add-term" onClick=${() => setEditing(-1)} disabled=${full} data-tip=${full ? 'The lexicon is full: remove a term first' : null}>Add a term</button>
    </div>`}
    ${list && (shown.length ? html`<div class="cols" id="pronunciations">
      <div class="list-row head"><span>Term</span><span>Style</span><span>French, English-style</span><span>French, French-style</span><span>English</span><span></span></div>
      <ul class="list-rows">${shown.map((e) => html`<li class="list-row" id=${'term-' + e.id} key=${e.id}>
        <div class="main"><span class="name">${e.term}</span><div class="meta">${e.by === 'seed' ? 'shipped' : ago(e.at)}</div></div>
        <div class="cells"><span class="cell" data-label="Style"><button type="button" class="btn-secondary style-flip" onClick=${() => flip(e)}
            aria-label=${`${e.term}: ${styleName[e.style]}-style, switch to ${styleName[e.style === 'en' ? 'fr' : 'en']}-style`}>${styleName[e.style]}</button></span>
          ${form('French, English-style', e.fr_en, e.style === 'en')}${form('French, French-style', e.fr_fr, e.style === 'fr')}${form('English', e.en, false)}</div>
        <div class="actions"><${RowMenu} id=${'term-menu-' + e.id} label=${'More actions on ' + e.term} items=${[
          ['Edit…', () => setEditing(e.id), '', 'Edit ' + e.term], ['Remove…', () => remove(e), 'danger', 'Remove ' + e.term]]}/></div>
      </li>`)}</ul></div>` : html`<p class="empty">${needle ? 'No term matches.' : 'No term yet.'}</p>`)}
    ${editing !== 0 && html`<${TermSheet} key=${editing} entry=${list?.entries.find((e) => e.id === editing)} reload=${load} done=${() => setEditing(0)}/>`}`;
}

/** @param {{entry?: LexEntry, reload: () => void, done: () => void}} props */
function TermSheet({ entry, reload, done }) {
  const [t, setT] = useState(/** @type {LexTerm} */ (entry ? termBody(entry) : { term: '', style: 'en', fr_en: '', fr_fr: '', en: '' }));
  const [err, setErr] = useState('');
  const ref = useRef(/** @type {HTMLFormElement | null} */ (null));
  /** @param {keyof LexTerm} k */
  const field = (k) => onInput((/** @type {string} */ v) => setT({ ...t, [k]: v }));
  const save = () => (entry ? api('PUT', '/api/admin/pronunciations/' + entry.id, t) : api('POST', '/api/admin/pronunciations', t)).then(() => {
    reload();
    toast(`${t.term} ${entry ? 'saved' : 'added'}`);
    ref.current?.closest('dialog')?.close();
  }).catch((/** @type {Error} */ e) => setErr(e.message));
  return html`<${Sheet} id="term-sheet" title=${entry ? 'Edit ' + entry.term : 'Add a term'} done=${done}>
    <form ref=${ref} class="term-form" onSubmit=${formAction(save)}>
      <label>Term<input required maxlength="40" value=${t.term} onInput=${field('term')}/></label>
      <fieldset><legend>Style</legend>
        ${/** @type {const} */ (['en', 'fr']).map((s) => html`<label><input type="radio" name="style" value=${s} checked=${t.style === s}
          onChange=${() => setT({ ...t, style: s })}/> ${styleName[s]}-style</label>`)}</fieldset>
      <label>French, English-style<input maxlength="80" value=${t.fr_en} onInput=${field('fr_en')} placeholder="guite-heub"/></label>
      <label>French, French-style<input maxlength="80" value=${t.fr_fr} onInput=${field('fr_fr')}/></label>
      <label>English<input maxlength="80" value=${t.en} onInput=${field('en')} placeholder="Live Kit"/></label>
      <p class="hint">Set at least one form. A French voice says the form of the term's style.</p>
      ${err && html`<p class="said bad" role="alert">${err}</p>`}
      <div class="actions"><button type="submit">${entry ? 'Save' : 'Add'}</button></div>
    </form>
  <//>`;
}

function InstanceView() {
  const [it, setIt] = useState(/** @type {Instance | null} */ (null));
  useEffect(() => { api('GET', '/api/admin/instance').then(setIt).catch(show); }, []);
  if (!it) return html`<h2>Server</h2>`;
  const versions = Object.entries(it.daemons.versions).map(([v, n]) => `${v || 'unknown'} × ${n}`).join(', ');
  const rows = [
    ['Version', it.version || 'unknown'],
    ['Database', size(it.db_bytes)],
    ['Disk', it.disk_free < 0 ? 'unknown' : `${size(it.disk_free)} free of ${size(it.disk_total)}`],
    ['Speech to text', !it.stt.voice ? 'off: no audio' : !it.stt.configured ? 'off: no STT token'
      : `${it.stt.workers} transcriber${it.stt.workers === 1 ? '' : 's'} up`],
    ['AI host apps', `${it.daemons.connected} connected${versions ? ': ' + versions : ''}`],
    ['Email (SMTP)', it.smtp ? 'configured' : 'not configured'],
  ];
  return html`
    <h2>Server</h2>
    <dl id="instance">${rows.map(([k, v]) => html`<div><dt>${k}</dt><dd>${v}</dd></div>`)}</dl>
    <${ServerSettings}/>`;
}

/**
 * ServerSettings is one settings group (docs/design.md §5): the guardrails on rooms profiles create
 * from their profile page (004-AC7), saved together by one Save, and the hourly removal of empty
 * profiles (011-AC17), a toggle that applies at once and says so.
 */
function ServerSettings() {
  const [saved, setSaved] = useState(/** @type {Settings | null} */ (null));
  const [per, setPer] = useState('');
  const [quota, setQuota] = useState('');
  const [mode, setMode] = useState('anyone');
  const [purge, setPurge] = useState(false);
  const [busy, setBusy] = useState(false);
  /** @param {Settings} v */
  const fill = (v) => {
    setSaved(v);
    setPer(String(v.rooms_per_profile));
    setQuota(v.storage_quota_bytes ? String(Math.round(v.storage_quota_bytes / 1048576)) : '');
    setMode(v.room_creation);
    setPurge(v.purge_empty_profiles);
  };
  useEffect(() => { api('GET', '/api/admin/settings').then(fill).catch(show); }, []);
  if (!saved) return null;
  const quotaBytes = Math.round(Number(quota || 0) * 1048576);
  const dirty = [mode !== saved.room_creation, per !== String(saved.rooms_per_profile), quotaBytes !== (saved.storage_quota_bytes || 0)].filter(Boolean).length;
  const save = () => api('PUT', '/api/admin/settings', { rooms_per_profile: Number(per), storage_quota_bytes: quotaBytes, room_creation: mode })
    .then((/** @type {Settings} */ v) => { fill(v); toast('Saved'); }).catch(show);
  /** @param {Event} e */
  const togglePurge = (e) => {
    const want = /** @type {HTMLInputElement} */ (e.currentTarget).checked;
    // The box shows the choice while it saves, and the stored value again on a refusal.
    setPurge(want);
    setBusy(true);
    api('PUT', '/api/admin/settings', { purge_empty_profiles: want })
      .then((/** @type {Settings} */ v) => { setPurge(v.purge_empty_profiles); setSaved({ ...saved, purge_empty_profiles: v.purge_empty_profiles });
        toast(v.purge_empty_profiles ? 'Empty profiles will be deleted' : 'Empty profiles are kept'); })
      .catch((err) => { setPurge(!want); show(err); }).finally(() => setBusy(false));
  };
  return html`
    <h2>Room creation and profiles</h2>
    <p class="hint">Profiles create rooms from their profile page and administer them. These limits apply to them, not to you.</p>
    <form class="settings-group" id="room-creation" onSubmit=${formAction(save)}>
      <label for="room-creation-mode">Who creates rooms</label>
      <div class="control"><select id="room-creation-mode" value=${mode} onChange=${onInput(setMode)}>
        <option value="anyone">Anyone with a profile</option>
        <option value="super-admin">The server admin only</option>
      </select></div>
      <label for="rooms-per-profile">Rooms per profile</label>
      <div class="control"><input id="rooms-per-profile" type="number" min="1" max="1000" step="1" required value=${per} onInput=${onInput(setPer)}/></div>
      <label for="storage-quota">Storage quota (MB)</label>
      <div class="control"><input id="storage-quota" type="number" min="1" step="1" placeholder="none" value=${quota} onInput=${onInput(setQuota)}/></div>
      <label class="full"><input type="checkbox" id="purge-empty" checked=${purge} disabled=${busy} onChange=${togglePurge}/>
        Delete empty profiles after 24 h of inactivity</label>
      <p class="hint">An empty profile has no email, no room it can still enter, nothing in the libraries and no room-admin role. Nobody can reach it again. This switch applies at once.</p>
      <div class="settings-foot"><span class="dirty">${dirty ? dirty + ' unsaved change' + (dirty > 1 ? 's' : '') : 'No unsaved changes'}</span>
        <button type="submit" disabled=${!dirty}>Save</button></div>
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
    <main class="page-wide">
      <${TabBar} tabs=${views} active=${view} href=${(/** @type {string} */ id) => '#' + id} label="Views" cls="views sticky" header/>
      ${notice.value && html`<p class="said bad" role="alert">${notice.value}</p>`}
      ${{ rooms: html`<${Rooms}/>`, profiles: html`<${Profiles}/>`, libraries: html`<${Libraries}/>`,
        pronunciation: html`<${Pronunciation}/>`, instance: html`<${InstanceView}/>`,
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
