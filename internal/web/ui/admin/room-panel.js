// The panel that manages one room, shared by /admin (the super-admin, 004-AC2) and the room page
// (its room admins, 004-AC8), with the tab bar and the helpers both pages use.
import { useState, useEffect, useRef } from 'preact/hooks';
import { html } from 'htm/preact';

/**
 * @typedef {{uploads: number, revisions: number, attachments: number, agent_sessions: number, transcripts: number,
 *   room_skills: number, plugins: number, total: number}} Volume
 * @typedef {{id: string, name: string, created_at: string, last_activity: string, participants: number, personas: number,
 *   host_participant_id?: number, quota_bytes: number, quota: number, used: number, host_online: boolean, volume: Volume,
 *   created_from?: {title: string, version: number}, private_plugins?: string[]}} Room
 * @typedef {{id: number, created_at: string, created_by?: number, revoked_at?: string, shareable?: boolean}} Invite
 * @typedef {{participant_id: number, name: string, admin: boolean, you: boolean}} Person
 * @typedef {{id: number, invites: number[], lose: Person[], keep: Person[]}} Preview
 * @typedef {(method: string, path: string, body?: unknown) => Promise<any>} Api
 */

/**
 * fetchJSON sends a request with the page's cookies and returns its JSON answer, {} for none. A
 * refusal throws the server's reason, with the status and the whole answer as data.
 * @param {string} method
 * @param {string} path
 * @param {unknown} [body]
 * @returns {Promise<any>}
 */
export async function fetchJSON(method, path, body) {
  /** @type {RequestInit & {headers: Record<string, string>}} */
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const data = res.status === 204 ? {} : await res.json().catch(() => ({}));
  if (!res.ok) throw Object.assign(new Error(data.error || 'HTTP ' + res.status), { status: res.status, data });
  return data;
}

/** @param {string} t */
export const when = (t) => window.grillDate(t);
/**
 * ago says how long ago t was, coarsely, for a list (005-AC43): the full date shows in the tip.
 * @param {string} t
 */
export function ago(t) {
  return html`<time datetime=${t} data-tip=${when(t)}>${window.grillAgo(t)}</time>`;
}
/**
 * icon is an icon of the shared set (kit/ui.js) as a control's content: the control names itself.
 * @param {string} name
 */
export const icon = (name) => ({ __html: window.grillIconHTML(name) });
/** @param {number} n bytes */
export function size(n) {
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return (n < 10 ? n.toFixed(1) : Math.round(n)) + ' ' + units[i];
}
/** @param {(v: string) => void} set */
export const onInput = (set) => (/** @type {Event} */ e) => set(/** @type {HTMLInputElement} */ (e.currentTarget).value);
/**
 * formAction runs work on a form's submit, without a page load.
 * @param {() => unknown} work
 */
export const formAction = (work) => (/** @type {Event} */ e) => { e.preventDefault(); work(); };

/**
 * TabBar is a row of tab links, each a deep link (005-AC15, 005-AC16): links keep every tab in the
 * keyboard's reach and in the history, with no ARIA tab widget to drive.
 * @param {{tabs: string[][], active: string, href: (id: string) => string, label: string, cls?: string}} props
 */
export function TabBar({ tabs, active, href, label, cls }) {
  const nav = useRef(/** @type {HTMLElement | null} */ (null));
  // A bar that scrolls sideways on a phone keeps its current tab in sight (005-AC35).
  useEffect(() => { nav.current?.querySelector('[aria-current=page]')?.scrollIntoView({ block: 'nearest', inline: 'nearest' }); }, [active]);
  return html`<nav ref=${nav} class=${'tabs' + (cls ? ' ' + cls : '')} aria-label=${label}>${tabs.map(([id, name]) => html`<a key=${id} href=${href(id)}
    id=${'tab-' + id} aria-current=${id === active ? 'page' : undefined}>${name}</a>`)}</nav>`;
}

/** The tabs of a room's management that both pages share; each page adds its own. */
export const roomTabs = { general: 'General', invites: 'Invites', admins: 'Admins', export: 'Export' };

/**
 * RoomPanel manages one room in tabs (005-AC15, 005-AC16). General shows the room's volume against
 * its quota, renames and deletes it once its name is typed; Invites lists and revokes its invites;
 * Export exports it, with a warning on what the archive holds (004-AC11). Only the super-admin sets
 * the quota. tabs and href come from the page; children fill the tabs not built in here. byName
 * names an invite's creator.
 * @param {{room: Room, base: string, api: Api, show: (err: unknown) => void, reload: () => void, close: () => void,
 *   tab: string, tabs: string[][], href: (id: string) => string, setQuota?: boolean, byName?: (id: number) => string, me?: number, label?: string,
 *   general?: import('preact').ComponentChildren, children?: import('preact').ComponentChildren}} props
 */
export function RoomPanel({ room, base, api, show, reload, close, tab, tabs, href, setQuota, byName, me, label = 'Manage', general, children }) {
  return html`
    <section id="room-panel" aria-label=${label + ' ' + room.name}>
      <h2>${room.name}</h2>
      <${TabBar} tabs=${tabs} active=${tab} href=${href} label=${label + ' ' + room.name}/>
      <div class="tab-panel" id=${'panel-' + tab}>
        ${tab === 'general' ? html`<${General} room=${room} base=${base} api=${api} show=${show} reload=${reload} close=${close} setQuota=${setQuota}/>${general}`
          : tab === 'invites' ? html`<${Invites} base=${base} api=${api} show=${show} byName=${byName} me=${me}/>`
          : tab === 'export' ? html`<${Export} base=${base} room=${room}/>`
          : children}
      </div>
    </section>`;
}

/**
 * @param {{room: Room, base: string, api: Api, show: (err: unknown) => void, reload: () => void, close: () => void,
 *   setQuota?: boolean}} props
 */
function General({ room, base, api, show, reload, close, setQuota }) {
  const [name, setName] = useState(room.name);
  const [quota, setQuota_] = useState(String(Math.round(room.quota / 1048576)));
  const [confirm, setConfirm] = useState('');
  const [copied, setCopied] = useState('');
  const v = room.volume;
  const kinds = [['Uploads', v.uploads], ['Revisions', v.revisions], ['Attachments', v.attachments],
    ['AI sessions', v.agent_sessions], ['Transcripts', v.transcripts], ['Room skills', v.room_skills], ['Plugins', v.plugins]];
  /** @param {number} bytes */
  const setQuotaTo = (bytes) => api('PUT', base + '/quota', { quota_bytes: bytes }).then(reload).catch(show);
  const copyID = () => navigator.clipboard.writeText(room.id).then(() => setCopied('Copied.'), () => setCopied(room.id));
  return html`
    <h3>Name</h3>
    <form class="row" onSubmit=${formAction(() => api('PATCH', base, { name }).then(reload).catch(show))}>
      <label>Room name<input id="room-name-input" required maxlength="100" value=${name} onInput=${onInput(setName)}/></label>
      <button type="submit" disabled=${name.trim() === room.name}>Rename</button>
      <button type="button" class="btn-secondary" id="copy-room-id" onClick=${copyID}>Copy room id</button>
      <span class="said" role="status">${copied}</span>
    </form>
    ${room.created_from && html`<p id="created-from">Created from the starter ${room.created_from.title} v${room.created_from.version}.</p>`}
    <h3>Storage</h3>
    <p id="storage">Storage: ${size(room.used)} of ${size(room.quota)}${room.quota_bytes ? '' : ', the instance default'}.${setQuota ? '' : ' Only the instance admin changes the limit.'}</p>
    <details><summary>What takes the space</summary>
      <dl id="volume">${kinds.map(([k, n]) => html`<div><dt>${k}</dt><dd>${size(/** @type {number} */ (n))}</dd></div>`)}
        <div><dt>Total</dt><dd>${size(v.total)}</dd></div></dl>
      <p class="hint">The limit counts uploads, attachments and plugins.</p>
    </details>
    ${setQuota && html`<form class="row" onSubmit=${formAction(() => setQuotaTo(Math.round(Number(quota) * 1048576)))}>
      <label>Quota (MB)<input id="quota" type="number" min="1" step="1" required value=${quota} onInput=${onInput(setQuota_)}/></label>
      <button type="submit">Set quota</button>
      <button type="button" class="btn-secondary" disabled=${!room.quota_bytes} onClick=${() => setQuotaTo(0)}>Use the default</button>
    </form>`}
    <h3>Delete</h3>
    <form class="row" onSubmit=${formAction(() => api('DELETE', base).then(close).catch(show))}>
      <label>Type the room name to delete it, with everything it holds
        <input id="delete-confirm" autocomplete="off" value=${confirm} onInput=${onInput(setConfirm)}/></label>
      <button type="submit" class=${confirm === room.name ? 'btn-danger' : 'btn-danger-outline'} disabled=${confirm !== room.name}>Delete the room</button>
    </form>`;
}

/** @param {string} text */
const toast = (text) => dispatchEvent(new CustomEvent('grill:toast', { detail: text }));

/**
 * Invites lists the room's invites, every one for an admin and a member's own for a member
 * (011-AC27); me marks the viewer's shareable link. A revoke asks first in RevokeSheet (011-AC19).
 * @param {{base: string, api: Api, show: (err: unknown) => void, byName?: (id: number) => string, me?: number}} props
 */
export function Invites({ base, api, show, byName, me }) {
  const [invites, setInvites] = useState(/** @type {Invite[] | null} */ (null));
  const [link, setLink] = useState('');
  const [making, setMaking] = useState(false);
  const [asking, setAsking] = useState(0);
  const [preview, setPreview] = useState(/** @type {Preview | null} */ (null));
  const load = () => api('GET', base + '/invites').then(setInvites).catch(show);
  useEffect(() => { load(); }, [base]);
  const invite = () => {
    setMaking(true);
    api('POST', base + '/invites').then((/** @type {{url: string}} */ i) => { setLink(i.url); load(); }).catch(show).finally(() => setMaking(false));
  };
  const copy = () => navigator.clipboard.writeText(link).then(() => toast('Copied'),
    () => /** @type {HTMLInputElement | null} */ (document.getElementById('invite-link-new'))?.select());
  /** @param {Invite} i */
  const ask = (i) => {
    setAsking(i.id);
    api('GET', `${base}/invites/${i.id}/revoke-preview`).then((/** @type {Omit<Preview, 'id'>} */ p) => setPreview({ ...p, id: i.id }))
      .catch(show).finally(() => setAsking(0));
  };
  /** @param {Invite} i */
  const by = (i) => (!i.created_by ? 'admin' : byName ? byName(i.created_by) : 'participant #' + i.created_by);
  /** @param {Invite} i */
  const mark = (i) => (!i.shareable ? '' : i.created_by === me ? 'Your shareable link' : 'shareable');
  return html`
    <div class="row"><button type="button" class="btn-primary" disabled=${making} onClick=${invite}>New invite link</button></div>
    ${link && html`<div class="row"><label class="link">Invite link, shown once<input id="invite-link-new" readonly value=${link}/></label>
      <button type="button" class="btn-secondary" id="invite-link-copy" onClick=${copy}>Copy</button></div>`}
    ${invites && (invites.length ? html`<div class="scroll"><table id="invites-all" class="cards">
      <thead><tr><th>Invite</th><th>Created</th><th>By</th><th>State</th><th></th></tr></thead>
      <tbody>${invites.map((i) => html`<tr id=${'invite-' + i.id}><td>#${i.id}${mark(i) && html` <span class="badge">${mark(i)}</span>`}</td>
        <td data-label="Created">${when(i.created_at)}</td>
        <td data-label="By">${by(i)}</td><td data-label="State">${i.revoked_at ? 'revoked' : 'valid'}</td>
        <td>${!i.revoked_at && html`<button type="button" class="btn-secondary" disabled=${asking === i.id} onClick=${() => ask(i)}>Revoke</button>`}</td></tr>`)}</tbody>
    </table></div>`
      : html`<p class="empty" id="invites-empty">No invite links yet. New invite link makes one to share.</p>`)}
    ${preview && html`<${RevokeSheet} key=${preview.id} preview=${preview} base=${base} api=${api} done=${() => { setPreview(null); load(); }}/>`}`;
}

/**
 * RevokeSheet confirms a revoke with who loses access and who keeps it (011-AC19), then says who
 * the revoke cut, from its answer: the state may have moved since the preview (011-AC20).
 * @param {{preview: Preview, base: string, api: Api, done: () => void}} props
 */
function RevokeSheet({ preview, base, api, done }) {
  const ref = useRef(/** @type {HTMLDialogElement | null} */ (null));
  const [busy, setBusy] = useState(false);
  const [said, setSaid] = useState('');
  // inflight, unlike busy, is current inside the close handler of the render that sent the DELETE.
  const inflight = useRef(false);
  useEffect(() => { ref.current?.showModal(); }, []);
  // A revoke in flight keeps the sheet open: its error would have nowhere to show.
  const close = () => { if (!busy) ref.current?.close(); };
  // A second Escape closes a dialog whatever cancel does: reopen it until the answer comes.
  const closed = () => {
    if (inflight.current) ref.current?.showModal();
    else done();
  };
  const revoke = () => {
    inflight.current = true;
    setBusy(true);
    setSaid('');
    api('DELETE', `${base}/invites/${preview.id}`).then((/** @type {{lost: {name: string}[]}} */ r) => {
      inflight.current = false;
      const lost = (r.lost || []).map((p) => p.name);
      toast(lost.length ? `Revoked. ${new Intl.ListFormat('en').format(lost)} lost access.` : 'Revoked. Nobody lost access.');
      // The room page reloads its people and its empty-chat card (011-AC20).
      dispatchEvent(new CustomEvent('grill:revoked'));
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
  /** @param {Person[]} people @param {string} id */
  const list = (people, id) => html`<ul id=${id}>${people.map((p) => html`<li key=${p.participant_id}>${p.name}
    ${p.admin && html` <span class="badge role">admin</span>`}${p.you && html` <span class="badge">you</span>`}</li>`)}</ul>`;
  const chained = preview.invites.length - 1;
  return html`<dialog ref=${ref} id="revoke-sheet" class="sheet full" aria-labelledby="revoke-title" onClose=${closed} onClick=${outside}
    onCancel=${(/** @type {Event} */ e) => { if (busy) e.preventDefault(); }}>
    <div class="sheet-head"><h2 id="revoke-title">Revoke this invite?</h2>
      <button type="button" class="btn-icon btn-ghost" aria-label="Close" disabled=${busy} onClick=${close}>✕</button></div>
    ${preview.lose.length > 0 && html`<p>These people lose access:</p>${list(preview.lose, 'revoke-lose')}`}
    ${preview.keep.length > 0 && html`<p>Keep access through another invite:</p>${list(preview.keep, 'revoke-keep')}`}
    ${!preview.lose.length && !preview.keep.length && html`<p id="revoke-nobody">Nobody joined through this invite.</p>`}
    ${chained > 0 && html`<p id="revoke-chain">Also revokes ${chained} ${chained > 1 ? 'invites' : 'invite'} made by people who joined through it.</p>`}
    <p class="said error" role="alert">${said}</p>
    <div class="actions">
      <button type="button" class="btn-secondary" disabled=${busy} onClick=${close}>Cancel</button>
      <button type="button" class="btn-danger" disabled=${busy} onClick=${revoke}>Revoke</button>
    </div>
  </dialog>`;
}

/**
 * Export also names the plugins from private repos: their files go in the archive (009-AC26).
 * @param {{base: string, room: Room}} props
 */
function Export({ base, room }) {
  const priv = room.private_plugins || [];
  return html`
    <p class="warn" id="export-warning">The archive holds the room's AI sessions: everything Claude read in this room, documents and transcripts included. Whoever holds the file can read it all.</p>
    ${priv.length > 0 && html`<p class="warn" id="export-private">${priv.length > 1 ? 'These plugins come' : 'This plugin comes'} from a private repo: ${priv.join(', ')}. The archive holds ${priv.length > 1 ? 'their' : 'its'} files, which whoever holds it can read. Options and secrets stay on the AI host.</p>`}
    <div class="row"><a id="export" class="btn btn-primary" href=${base + '/export'} download>Export the room</a></div>`;
}

/**
 * Rename lets a room admin or the super-admin rename a participant (005-AC21): a button that opens
 * a one-field form in place.
 * @param {{name: string, path: string, api: Api, show: (err: unknown) => void, done: () => void}} props
 */
export function Rename({ name, path, api, show, done }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(name);
  if (!editing) {
    return html`<button type="button" class="btn-secondary" onClick=${() => { setValue(name); setEditing(true); }}
      aria-label=${'Rename ' + name}>Rename</button>`;
  }
  return html`<form class="rename" onSubmit=${formAction(() => api('PUT', path, { name: value }).then(() => { setEditing(false); done(); }).catch(show))}>
    <input required maxlength="40" value=${value} onInput=${onInput(setValue)} aria-label=${'New name for ' + name}/>
    <button type="submit">Save</button>
    <button type="button" class="btn-secondary" onClick=${() => setEditing(false)}>Cancel</button>
  </form>`;
}
