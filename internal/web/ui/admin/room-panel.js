// The panel that manages one room, shared by /admin (the super-admin, 004-AC2) and the room page
// (its room admins, 004-AC8), with the helpers both pages format with.
import { useState, useEffect, useRef } from 'preact/hooks';
import { html } from 'htm/preact';

/**
 * @typedef {{uploads: number, revisions: number, attachments: number, agent_sessions: number, transcripts: number,
 *   room_skills: number, total: number}} Volume
 * @typedef {{id: string, name: string, created_at: string, last_activity: string, participants: number, personas: number,
 *   host_participant_id?: number, quota_bytes: number, quota: number, used: number, host_online: boolean, volume: Volume}} Room
 * @typedef {{id: number, created_at: string, created_by?: number, revoked_at?: string}} Invite
 * @typedef {(method: string, path: string, body?: unknown) => Promise<any>} Api
 */

/**
 * fetchJSON sends a request with the page's cookies and returns its JSON answer, {} for none. A
 * refusal throws the server's reason, with the status.
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
  if (!res.ok) throw Object.assign(new Error(data.error || 'HTTP ' + res.status), { status: res.status });
  return data;
}

/** @param {string} t */
export const when = (t) => new Date(t).toLocaleString();
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
 * RoomPanel shows the room's volume against its quota, renames it, lists and revokes its invites,
 * offers the export and deletes it once its name is typed. Only the super-admin sets the quota.
 * byName names an invite's creator; children add sections before the export.
 * @param {{room: Room, base: string, api: Api, show: (err: unknown) => void, reload: () => void, close: () => void,
 *   setQuota?: boolean, byName?: (id: number) => string, children?: import('preact').ComponentChildren}} props
 */
export function RoomPanel({ room, base, api, show, reload, close, setQuota, byName, children }) {
  const [name, setName] = useState(room.name);
  const [quota, setQuota_] = useState(String(Math.round(room.quota / 1048576)));
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
  /** @param {Invite} i */
  const by = (i) => (!i.created_by ? 'admin' : byName ? byName(i.created_by) : 'participant #' + i.created_by);
  return html`
    <section id="room-panel" ref=${panel} aria-label=${'Manage ' + room.name}>
      <h2>${room.name} <code>${room.id}</code></h2>
      <h3>Volume</h3>
      <dl id="volume">${kinds.map(([k, n]) => html`<div><dt>${k}</dt><dd>${size(/** @type {number} */ (n))}</dd></div>`)}
        <div><dt>Total</dt><dd>${size(v.total)}</dd></div></dl>
      <h3>Quota</h3>
      <p class="hint">${size(room.used)} of ${size(room.quota)} used${room.quota_bytes ? '' : ', the instance default'}.${setQuota ? '' : ' Only the instance admin changes it.'}</p>
      ${setQuota && html`<form class="row" onSubmit=${formAction(() => setQuotaTo(Math.round(Number(quota) * 1048576)))}>
        <label>Quota (MB)<input id="quota" type="number" min="1" step="1" required value=${quota} onInput=${onInput(setQuota_)}/></label>
        <button type="submit">Set quota</button>
        <button type="button" disabled=${!room.quota_bytes} onClick=${() => setQuotaTo(0)}>Use the default</button>
      </form>`}
      <h3>Name</h3>
      <form class="row" onSubmit=${formAction(() => api('PATCH', base, { name }).then(reload).catch(show))}>
        <label>Room name<input id="room-name-input" required maxlength="100" value=${name} onInput=${onInput(setName)}/></label>
        <button type="submit" disabled=${name.trim() === room.name}>Rename</button>
      </form>
      <h3>Invites</h3>
      <div class="row"><button type="button" onClick=${invite}>New invite link</button></div>
      ${link && html`<label class="link">Invite link, shown once<input id="invite-link-new" readonly value=${link}/></label>`}
      ${invites && html`<div class="scroll"><table id="invites-all">
        <thead><tr><th>Invite</th><th>Created</th><th>By</th><th>State</th><th></th></tr></thead>
        <tbody>${invites.map((i) => html`<tr id=${'invite-' + i.id}><td>#${i.id}</td><td>${when(i.created_at)}</td>
          <td>${by(i)}</td><td>${i.revoked_at ? 'revoked' : 'valid'}</td>
          <td>${!i.revoked_at && html`<button type="button" onClick=${() => revoke(i)}>Revoke</button>`}</td></tr>`)}</tbody>
      </table></div>`}
      ${children}
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
