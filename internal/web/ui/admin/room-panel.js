// The panel that manages one room, shared by /admin (the super-admin, 004-AC2) and the room page
// (its room admins, 004-AC8), with the tab bar and the helpers both pages use.
import { useState, useEffect, useRef } from 'preact/hooks';
import { html } from 'htm/preact';

/**
 * @typedef {{uploads: number, revisions: number, attachments: number, agent_sessions: number, transcripts: number,
 *   room_skills: number, plugins: number, total: number}} Volume
 * @typedef {{id: string, name: string, created_at: string, last_activity: string, participants: number, personas: number,
 *   host_participant_id?: number, quota_bytes: number, quota: number, used: number, host_online: boolean, volume: Volume,
 *   created_from?: {title: string, version: number}, proposed_in?: {room?: string, name: string, member?: boolean}, private_plugins?: string[]}} Room
 * @typedef {{id: number, created_at: string, created_by?: number, revoked_at?: string, shareable?: boolean, name?: string, in_room: number}} Invite
 * @typedef {{participant_id: number, name: string, admin: boolean, you: boolean}} Person
 * @typedef {{id: number, invites: number[], lose: Person[], keep: Person[]}} Preview
 * @typedef {{profile_id: number, name: string, created_at: string, by?: string}} Block
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
 * @typedef {{checked?: boolean, disabled?: boolean, reason?: string}} ItemState
 * @typedef {[string, () => void, string?, string?, ItemState?]} MenuItem
 */

/**
 * RowMenu is a row's ⋯ and its menu of actions (docs/design.md §5); each item closes the menu. An
 * item is [text, run, class, aria-label, state]: a toggle shows its box, and a disabled item stays
 * focusable with its reason as its tip, as People's ⋯ (040-AC11).
 * @param {{id: string, label: string, items: MenuItem[]}} props
 */
export function RowMenu({ id, label, items }) {
  return html`<button type="button" class="btn-icon btn-ghost more" popovertarget=${id} aria-label=${label} dangerouslySetInnerHTML=${icon('more')}></button>
    <div id=${id} class="menu row-menu" popover>${items.map(([text, run, cls, aria, st = {}]) => html`<button type="button" class=${cls}
      popovertarget=${st.disabled ? undefined : id} popovertargetaction=${st.disabled ? undefined : 'hide'} aria-label=${aria}
      aria-pressed=${st.checked === undefined ? undefined : String(st.checked)} aria-disabled=${st.disabled ? 'true' : undefined}
      data-tip=${st.disabled ? st.reason : undefined} onClick=${st.disabled ? undefined : run}>${st.checked !== undefined && html`<span class="ico toggle"></span>`}${text}</button>`)}</div>`;
}

/**
 * followFocus puts the focus on sel, or on fallback once sel is gone, once work has settled, and
 * keeps it there while the rows redraw, for 5 s. From the call on, the user's click or key outside a
 * dialog work opened ends it, and so does a focus moved off where the action left it.
 * @param {string} sel
 * @param {string} [fallback]
 * @param {() => unknown} [work]
 */
export function followFocus(sel, fallback, work) {
  const at = document.querySelector(sel);
  // Where the action leaves the focus: its menu item, its ⋯, or the page once a dialog took it.
  const left = new Set([document.activeElement, at]);
  const home = at?.closest('dialog');
  /** @type {Element | null} */
  let ours = null;
  let done = false;
  const follow = () => {
    const l = /** @type {HTMLElement | null} */ (document.querySelector(sel) || (fallback ? document.querySelector(fallback) : null));
    const now = document.activeElement;
    if (done || !l || l === now) return;
    if (now && now !== document.body && now !== ours && !left.has(now) && now.isConnected) {
      stop();
      return;
    }
    l.focus();
    ours = l;
  };
  const watch = new MutationObserver(follow);
  /** @param {Event} e */
  const user = (e) => {
    const d = e.target instanceof Element ? e.target.closest('dialog') : null;
    if (!d || d === home) stop();
  };
  const stop = () => {
    done = true;
    watch.disconnect();
    removeEventListener('pointerdown', user, true);
    removeEventListener('keydown', user, true);
  };
  addEventListener('pointerdown', user, true);
  addEventListener('keydown', user, true);
  Promise.resolve(work?.()).finally(() => {
    if (done) return;
    watch.observe(document.body, { childList: true, subtree: true });
    setTimeout(stop, 5000);
    follow();
  });
}

/** The ⋯ of the RowMenu whose menu is id. @param {string} id */
const moreOf = (id) => `[popovertarget="${id}"]`;

/**
 * inviteLabel is an invite as its rows name it: its name, or "Link <id>" (040-AC15).
 * @param {number} id
 * @param {string} [name]
 */
export const inviteLabel = (id, name) => name || 'Link ' + id;

/**
 * Meter is a share used, as a bar and the words for it (the native meter draws per browser).
 * @param {{used: number, max: number, label: string}} props
 */
export function Meter({ used, max, label }) {
  const pct = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;
  return html`<span class="meter" role="img" aria-label=${label} data-tip=${label}><span style=${{ width: pct + '%' }}></span></span>`;
}

/**
 * uaName words a user agent as people read it, "Chrome 155 on Linux"; the raw string goes in a tip.
 * @param {string} ua
 */
export function uaName(ua) {
  if (!ua) return 'unknown browser';
  const b = ua.match(/(Edg|OPR|Firefox|Chrome|CriOS|FxiOS)\/(\d+)/) || (/Safari\//.test(ua) && ua.match(/(Version)\/(\d+)/));
  const names = /** @type {Record<string, string>} */ ({ Edg: 'Edge', OPR: 'Opera', CriOS: 'Chrome', FxiOS: 'Firefox', Version: 'Safari' });
  const os = ua.match(/Windows|Android|iPhone|iPad|Mac OS X|CrOS|Linux/)?.[0];
  const osNames = /** @type {Record<string, string>} */ ({ 'Mac OS X': 'macOS', CrOS: 'ChromeOS' });
  const name = b ? (names[b[1]] || b[1]) + ' ' + b[2] : ua.split(/[ /]/)[0];
  return os ? name + ' on ' + (osNames[os] || os) : name;
}

/**
 * @typedef {{join_ip: string, join_ua: string, last_ip: string, last_ua: string, last_seen_at: string | null}} Client
 * @typedef {{id: number, name: string, admin: boolean, has_profile?: boolean, no_profile?: boolean, host?: boolean, can_host?: boolean,
 *   can_ask?: boolean, invite_id?: number, invite_name?: string, client?: Client}} Member
 * @typedef {{id: string, label: string, checked?: boolean, disabled?: boolean, reason?: string, run: () => unknown}} MembershipAction
 */

/**
 * AdminRows lists a room's seats, one row each (003), in columns, for /admin and the room's Room
 * settings alike (005-AC21, 004b-AC4, 040-AC10): the name with its badges and where they were last
 * seen, and the invite the seat joined through. The room page passes actions, the membership
 * actions People's ⋯ shows (040-AC11), for the row's ⋯; /admin keeps its admin button and a ⋯
 * with Rename and, where offered, Make AI host. Only a participant with a profile can be made an
 * admin (has_profile, when the page knows it).
 * @param {{people: Member[], base: string, api: Api, show: (err: unknown) => void, done: () => void, me?: number,
 *   makeHost?: (p: Member) => void, actions?: (p: Member) => MembershipAction[]}} props
 */
export function AdminRows({ people, base, api, show, done, me, makeHost, actions }) {
  return html`<div class=${'cols admin-cols' + (actions ? ' shared' : '')}><ul id="room-admins" class="list-rows">${people.map((p) => html`<${AdminRow} key=${p.id} p=${p}
    base=${base} api=${api} show=${show} done=${done} me=${me} makeHost=${makeHost} actions=${actions}/>`)}</ul></div>`;
}

/** @param {{p: Member, base: string, api: Api, show: (err: unknown) => void, done: () => void, me?: number, makeHost?: (p: Member) => void,
 *   actions?: (p: Member) => MembershipAction[]}} props */
function AdminRow({ p, base, api, show, done, me, makeHost, actions }) {
  const [renaming, setRenaming] = useState(false);
  const menu = 'admin-menu-' + p.id;
  // A row gone, its person removed, hands the focus to the list's first ⋯; the list gone, the viewer
  // stepped down, to Room settings' tab shown.
  /** @param {() => unknown} [work] */
  const back = (work) => followFocus(moreOf(menu), '#room-admins .more, #manage:not(:has(#room-admins .more)) nav.tabs > [aria-current=page]', work);
  /** @param {Member} p */
  const remove = async (p) => {
    const box = document.createElement('label');
    const block = document.createElement('input');
    block.type = 'checkbox';
    block.disabled = p.has_profile === false || !!p.no_profile;
    box.append(block, block.disabled ? ' Block from rejoining: no profile, so no block' : ' Block from rejoining');
    const note = window.grillReadersNote ? await window.grillReadersNote(p) : '';
    if (!(await window.grillConfirm(`Remove ${p.name} from the room?`, 'Their messages stay.' + note, 'Remove', false, box))) return;
    api('DELETE', `${base}/participants/${p.id}`, { block: block.checked }).then(done).catch(show);
  };
  /** @param {boolean} on */
  const set = (on) => api(on ? 'PUT' : 'DELETE', `${base}/admins/${p.id}`).then(done).catch(show);
  const c = p.client;
  const seen = c && c.last_ip ? [c.last_seen_at ? 'last active ' + window.grillAgo(c.last_seen_at) : '', uaName(c.last_ua)].filter(Boolean).join(' · ') : '';
  const tip = c && c.last_ip ? `Joined from ${c.join_ip || 'unknown'}, ${c.join_ua || 'no user agent'}. Last from ${c.last_ip}, ${c.last_ua || 'no user agent'}.` : '';
  /** @type {MenuItem[]} */
  const items = actions ? actions(p).map((a) => /** @type {MenuItem} */ ([a.label, () => back(a.run), '', undefined, a]))
    : [['Rename…', () => setRenaming(true), '', 'Rename ' + p.name]];
  if (!actions && makeHost && !p.host && p.can_host) items.push(['Make AI host', () => makeHost(p), '', 'Make ' + p.name + ' the AI host']);
  // 016-AC6, Q2: a room admin steps down before anyone removes them.
  if (!actions && p.id !== me && !p.admin) items.push(['Remove from room…', () => remove(p), 'danger', 'Remove ' + p.name + ' from the room']);
  const close = () => { setRenaming(false); back(); };
  return html`<li class="list-row">
    <div class="main">${renaming ? html`<${Rename} name=${p.name} path=${`${base}/participants/${p.id}/name`} api=${api} show=${show}
        done=${done} cancel=${close}/>`
      : html`<span class="name who">${p.name}</span>${p.admin && html` <span class="badge role">room admin</span>`}${p.host && html` <span class="badge">AI host</span>`}${p.can_ask && html` <span class="badge">can ask the AI</span>`}
        ${seen && html`<div class="meta" data-tip=${tip}>${seen}</div>`}`}</div>
    <div class="cells">
      <span class="cell invite" data-label="Invite" data-tip=${p.invite_id ? inviteLabel(p.invite_id, p.invite_name) : undefined}>${p.invite_id ? inviteLabel(p.invite_id, p.invite_name) : ''}</span>
      ${!actions && html`<span class="cell">${p.has_profile === false ? html`<span class="hint" data-tip="Only a participant with a profile can be an admin.">no profile</span>`
        : p.admin ? html`<button type="button" class="btn-secondary admin-toggle" onClick=${() => set(false)}
          aria-label=${p.id === me ? 'Step down as a room admin' : 'Remove ' + p.name + ' from the room admins'}>${p.id === me ? 'Step down' : 'Remove admin'}</button>`
        : html`<button type="button" class="btn-secondary admin-toggle" onClick=${() => set(true)} aria-label=${'Make ' + p.name + ' a room admin'}>Make admin</button>`}</span>`}
    </div>
    <div class="actions">${items.length > 0 && html`<${RowMenu} id=${menu} label=${'More actions on ' + p.name} items=${items}/>`}</div>
  </li>`;
}

/**
 * TabBar is a row of tab links, each a deep link (005-AC15, 005-AC16): links keep every tab in the
 * keyboard's reach and in the history, with no ARIA tab widget to drive.
 * header folds a page's own bar, when it overflows, into its app header's ☰ (kit/ui.js).
 * @param {{tabs: string[][], active: string, href: (id: string) => string, label: string, cls?: string, header?: boolean}} props
 */
export function TabBar({ tabs, active, href, label, cls, header }) {
  const nav = useRef(/** @type {HTMLElement | null} */ (null));
  // A bar that overflows folds into its menu, named after the current tab (005-AC35, kit/ui.js).
  // A tab added later, Glossary once its engine shows, may make the bar overflow.
  useEffect(() => { window.grillShowTab(nav.current); }, [active, tabs.length]);
  return html`<nav ref=${nav} class=${'tabs' + (cls ? ' ' + cls : '')} data-header=${header ? '' : undefined} aria-label=${label}>${tabs.map(([id, name]) => html`<a key=${id} href=${href(id)}
    id=${'tab-' + id} aria-current=${id === active ? 'page' : undefined}>${name}</a>`)}</nav>`;
}

/** The tabs of a room's management that both pages share; each page adds its own. */
export const roomTabs = { general: 'General', invites: 'Invites', admins: 'Members and admins', export: 'Export' };

/**
 * RoomPanel manages one room in tabs (005-AC15, 005-AC16). General shows the room's volume against
 * its quota, renames and deletes it once its name is typed; Invites lists and revokes its invites;
 * Export exports it, with a warning on what the archive holds (004-AC11). Only the super-admin sets
 * the quota. tabs and href come from the page; children fill the tabs not built in here. byName
 * names an invite's creator.
 * @param {{room: Room, base: string, api: Api, show: (err: unknown) => void, reload: () => void, close: () => void,
 *   tab: string, tabs: string[][], href: (id: string) => string, setQuota?: boolean, byName?: (id: number) => string, me?: number, label?: string, heading?: boolean,
 *   general?: import('preact').ComponentChildren, children?: import('preact').ComponentChildren}} props
 */
export function RoomPanel({ room, base, api, show, reload, close, tab, tabs, href, setQuota, byName, me, label = 'Manage', heading = true, general, children }) {
  return html`
    <section id="room-panel" aria-label=${label + ' ' + room.name}>
      ${heading && html`<h2>${room.name}</h2>`}
      <${TabBar} tabs=${tabs} active=${tab} href=${href} label=${label + ' ' + room.name}/>
      <div class="tab-panel" id=${'panel-' + tab}>
        ${tab === 'general' ? html`<${General} room=${room} base=${base} api=${api} show=${show} reload=${reload} close=${close} setQuota=${setQuota} extra=${general}/>`
          : tab === 'invites' ? html`<${Invites} base=${base} api=${api} show=${show} byName=${byName} me=${me} cut/>`
          : tab === 'export' ? html`<${Export} base=${base} room=${room}/>`
          : children}
      </div>
    </section>`;
}

/**
 * General: the name, the storage, what the page adds (extra), then Delete, last.
 * @param {{room: Room, base: string, api: Api, show: (err: unknown) => void, reload: () => void, close: () => void,
 *   setQuota?: boolean, extra?: import('preact').ComponentChildren}} props
 */
function General({ room, base, api, show, reload, close, setQuota, extra }) {
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
    ${room.proposed_in && html`<p id="proposed-in">Proposed in ${room.proposed_in.member && room.proposed_in.room
      ? html`<a href=${'/r/' + encodeURIComponent(room.proposed_in.room)}>${room.proposed_in.name}</a>` : room.proposed_in.name}.</p>`}
    <h3>Storage</h3>
    <p id="storage"><${Meter} used=${room.used} max=${room.quota} label=${size(room.used) + ' of ' + size(room.quota)}/> ${size(room.used)} of ${size(room.quota)}${room.quota_bytes ? '' : ', the server default'}.${setQuota ? '' : ' Only the server admin changes the limit.'}</p>
    <details class="disclosure"><summary>What takes the space</summary>
      <dl id="volume">${kinds.map(([k, n]) => html`<div><dt>${k}</dt><dd>${size(/** @type {number} */ (n))}</dd></div>`)}
        <div><dt>Total</dt><dd>${size(v.total)}</dd></div></dl>
      <p class="hint">The limit counts uploads, attachments and plugins.</p>
    </details>
    ${setQuota && html`<form class="row" onSubmit=${formAction(() => setQuotaTo(Math.round(Number(quota) * 1048576)))}>
      <label>Quota (MB)<input id="quota" type="number" min="1" step="1" required value=${quota} onInput=${onInput(setQuota_)}/></label>
      <button type="submit" class="btn-secondary">Set quota</button>
      <button type="button" class="btn-secondary" disabled=${!room.quota_bytes} onClick=${() => setQuotaTo(0)}>Use the default</button>
    </form>`}
    ${extra}
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
 * (011-AC27); me marks the viewer's shareable link. A new one takes an optional name, and each
 * row's ⋯ renames it (040-AC14): whoever sees a row made it or administers the room. Stop new joins
 * asks first in RevokeSheet (011-AC19, 040-AC16); cut, for a room admin or the super-admin, lets it
 * remove the people who joined (016-AC4).
 * @param {{base: string, api: Api, show: (err: unknown) => void, byName?: (id: number) => string, me?: number, cut?: boolean}} props
 */
export function Invites({ base, api, show, byName, me, cut = false }) {
  const [invites, setInvites] = useState(/** @type {Invite[] | null} */ (null));
  const [link, setLink] = useState('');
  const [name, setName] = useState('');
  const [making, setMaking] = useState(false);
  const [asking, setAsking] = useState(0);
  const [preview, setPreview] = useState(/** @type {Preview | null} */ (null));
  const [people, setPeople] = useState(/** @type {{id: number, name: string}[] | null} */ (null));
  // /admin names the makers from the room's participants, read with each list (someone may join
  // since) and set together with it, so a new maker never shows as a former one.
  const load = () => Promise.all([
    api('GET', base + '/invites'),
    byName ? null : api('GET', base + '/participants').catch((e) => { show(e); return null; }),
  ]).then(([list, who]) => { setInvites(list); setPeople(who); }).catch(show);
  useEffect(() => { load(); }, [base]);
  const invite = () => {
    setMaking(true);
    api('POST', base + '/invites', { name }).then((/** @type {{url: string}} */ i) => { setLink(i.url); setName(''); load(); }).catch(show).finally(() => setMaking(false));
  };
  const copy = () => navigator.clipboard.writeText(link).then(() => toast('Copied'),
    () => /** @type {HTMLInputElement | null} */ (document.getElementById('invite-link-new'))?.select());
  /** @param {Invite} i */
  const ask = (i) => {
    if (asking) return;
    setAsking(i.id);
    api('GET', `${base}/invites/${i.id}/revoke-preview`).then((/** @type {Omit<Preview, 'id'>} */ p) => setPreview({ ...p, id: i.id }))
      .catch(show).finally(() => setAsking(0));
  };
  /** @param {Invite} i */
  const by = (i) => (!i.created_by ? 'admin' : byName ? byName(i.created_by)
    : people?.find((p) => p.id === i.created_by)?.name || (people ? 'a former participant' : 'a participant'));
  /** @param {Invite} i */
  const mark = (i) => (!i.shareable ? '' : i.created_by === me ? 'Your shareable link' : 'shareable');
  return html`
    <form class="row" onSubmit=${formAction(invite)}>
      <label>Name<input id="invite-name" maxlength="60" placeholder="Optional" value=${name} onInput=${onInput(setName)}/></label>
      <button type="submit" class="btn-primary" disabled=${making}>New invite link</button></form>
    ${link && html`<div class="row"><label class="link">Invite link, shown once<input id="invite-link-new" readonly value=${link}/></label>
      <button type="button" class="btn-secondary" id="invite-link-copy" onClick=${copy}>Copy</button></div>`}
    ${invites && (invites.length ? html`<div class="cols" id="invites-all">
      <div class="list-row head"><span>Link</span><span>Created</span><span>State</span><span></span></div>
      <ul class="list-rows">${invites.map((i) => html`<${InviteRow} key=${i.id} i=${i} by=${by(i)} mark=${mark(i)} base=${base} api=${api} show=${show}
        ask=${ask} done=${load}/>`)}</ul></div>`
      : html`<p class="empty" id="invites-empty">No invite links yet. New invite link makes one to share.</p>`)}
    ${preview && html`<${RevokeSheet} key=${preview.id} preview=${preview} base=${base} api=${api} cut=${cut}
      done=${() => { followFocus(moreOf('invite-menu-' + preview.id)); setPreview(null); load(); }}/>`}`;
}

/**
 * InviteRow reads "<name or Link <id>> · created by <member> · N in the room" and its state, N the
 * seats that joined through it and are still here (040-AC15). Its ⋯ renames it, a stopped one too.
 * @param {{i: Invite, by: string, mark: string, base: string, api: Api, show: (err: unknown) => void, ask: (i: Invite) => void, done: () => void}} props
 */
function InviteRow({ i, by, mark, base, api, show, ask, done }) {
  const [renaming, setRenaming] = useState(false);
  const menu = 'invite-menu-' + i.id;
  const label = inviteLabel(i.id, i.name);
  const close = () => { setRenaming(false); followFocus(moreOf(menu)); };
  /** @type {MenuItem[]} */
  const items = [['Rename…', () => setRenaming(true), '', 'Rename ' + label]];
  if (!i.revoked_at) items.push(['Stop new joins…', () => ask(i), 'danger', 'Stop new joins through ' + label]);
  return html`<li class="list-row" id=${'invite-' + i.id}>
    <div class="main">${renaming ? html`<${Rename} name=${i.name || ''} label=${label} path=${`${base}/invites/${i.id}`} method="PATCH" max=${60} optional
        api=${api} show=${show} done=${done} cancel=${close}/>`
      : html`<span class="name">${label}</span>${mark && html` <span class="badge">${mark}</span>`}
        <div class="meta">created by <span class="by">${by}</span> · ${i.in_room} in the room</div>`}</div>
    <div class="cells"><span class="cell" data-label="Created">${ago(i.created_at)}</span>
      <span class="cell">${i.revoked_at ? html`<span class="status off">Stopped</span>` : html`<span class="status ok">Active</span>`}</span></div>
    <div class="actions"><${RowMenu} id=${menu} label=${'More actions on ' + label} items=${items}/></div></li>`;
}

/**
 * Blocked lists the profiles a room admin blocked from rejoining, with Unblock (016-AC14), under
 * the member list (038-AC9). people is that list: a removal with a block changes it.
 * @param {{base: string, api: Api, show: (err: unknown) => void, people: unknown}} props
 */
export function Blocked({ base, api, show, people }) {
  const [blocks, setBlocks] = useState(/** @type {Block[] | null} */ (null));
  const load = () => api('GET', base + '/blocks').then(setBlocks).catch(show);
  useEffect(() => { load(); }, [base, people]);
  /** @param {Block} b */
  const unblock = (b) => api('DELETE', `${base}/blocks/${b.profile_id}`).then(() => { toast(b.name + ' unblocked'); load(); }).catch(show);
  if (!blocks) return null;
  // 040-AC13: shown when empty too, so a room admin knows where a block would be.
  return html`<h3>Blocked from rejoining</h3>
    ${!blocks.length ? html`<p class="empty" id="blocked-empty">Nobody is blocked.</p>` : html`<div class="cols" id="blocked">
      <div class="list-row head"><span>Name</span><span>Blocked</span><span>By</span><span></span></div>
      <ul class="list-rows">${blocks.map((b) => html`<li class="list-row" id=${'blocked-' + b.profile_id} key=${b.profile_id}>
        <div class="main"><span class="name">${b.name}</span></div>
        <div class="cells"><span class="cell" data-label="Blocked">${ago(b.created_at)}</span><span class="cell" data-label="By">${b.by || 'admin'}</span></div>
        <div class="actions"><button type="button" class="btn-secondary" onClick=${() => unblock(b)} aria-label=${'Unblock ' + b.name}>Unblock</button></div></li>`)}</ul></div>`}`;
}

/**
 * RevokeSheet confirms Stop new joins, which keeps who joined (016-AC3, 040-AC16). With cut, someone joined
 * through the invite, it offers to remove them too, unticked, with who would lose access and who
 * keeps it (011-AC19, 016-AC4), then says who the revoke cut, from its answer: the state may have
 * moved since the preview (011-AC20).
 * @param {{preview: Preview, base: string, api: Api, cut: boolean, done: () => void}} props
 */
function RevokeSheet({ preview, base, api, cut, done }) {
  const ref = useRef(/** @type {HTMLDialogElement | null} */ (null));
  const [busy, setBusy] = useState(false);
  const [ticked, setTicked] = useState(false);
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
    api('DELETE', `${base}/invites/${preview.id}${ticked ? '?cut=1' : ''}`).then((/** @type {{lost: {name: string}[]}} */ r) => {
      inflight.current = false;
      const lost = (r.lost || []).map((p) => p.name);
      toast(lost.length ? `Stopped. ${new Intl.ListFormat('en').format(lost)} lost access.` : 'Stopped. Nobody lost access.');
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
  const n = preview.lose.length;
  const box = cut && n > 0;
  return html`<dialog ref=${ref} id="revoke-sheet" class="sheet full" aria-labelledby="revoke-title" onClose=${closed} onClick=${outside}
    onCancel=${(/** @type {Event} */ e) => { if (busy) e.preventDefault(); }}>
    <div class="sheet-head"><h2 id="revoke-title">Stop new joins through this link?</h2>
      <button type="button" class="btn-icon btn-ghost" aria-label="Close" disabled=${busy} onClick=${close} dangerouslySetInnerHTML=${icon('close')}></button></div>
    <p id="revoke-stays">No one can join through it any more. Who joined through it keeps their seat.</p>
    ${!preview.lose.length && !preview.keep.length && html`<p id="revoke-nobody">Nobody joined through this invite.</p>`}
    ${box && html`<label id="revoke-cut-label"><input type="checkbox" id="revoke-cut" checked=${ticked} disabled=${busy}
        onChange=${(/** @type {Event} */ e) => setTicked(/** @type {HTMLInputElement} */ (e.target).checked)}/>
        Also remove the ${n} ${n > 1 ? 'people' : 'person'} who joined through it</label>
      <p>Ticked, these people lose access:</p>${list(preview.lose, 'revoke-lose')}
      ${preview.keep.length > 0 && html`<p>Keep access through another invite:</p>${list(preview.keep, 'revoke-keep')}`}
      ${chained > 0 && html`<p id="revoke-chain">Ticked, it also stops ${chained} ${chained > 1 ? 'invites' : 'invite'} made by people who joined through it.</p>`}`}
    <p class="said error" role="alert">${said}</p>
    <div class="actions">
      <button type="button" class="btn-secondary" disabled=${busy} onClick=${close}>Cancel</button>
      <button type="button" class="btn-danger" disabled=${busy} onClick=${revoke}>${box && ticked ? `Stop and remove ${n} ${n > 1 ? 'people' : 'person'}` : 'Stop new joins'}</button>
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
 * Rename lets a room admin or the super-admin rename a participant (005-AC21), or an invite's maker
 * name it (040-AC14, optional, as PATCH): a one-field form in place, opened at once when the caller
 * has a cancel, else behind a Rename button. label names what is renamed, name by default.
 * @param {{name: string, label?: string, path: string, method?: string, max?: number, optional?: boolean, api: Api, show: (err: unknown) => void,
 *   done: () => void, cancel?: () => void}} props
 */
export function Rename({ name, label = name, path, method = 'PUT', max = 40, optional = false, api, show, done, cancel }) {
  const [editing, setEditing_] = useState(!!cancel);
  const setEditing = (/** @type {boolean} */ on) => { if (!on && cancel) cancel(); setEditing_(on); };
  const [value, setValue] = useState(name);
  const input = useRef(/** @type {HTMLInputElement | null} */ (null));
  // Opened from a ⋯ whose menu closed, the focus would be nowhere.
  useEffect(() => { if (editing) input.current?.select(); }, [editing]);
  if (!editing) {
    return html`<button type="button" class="btn-secondary" onClick=${() => { setValue(name); setEditing(true); }}
      aria-label=${'Rename ' + label}>Rename</button>`;
  }
  return html`<form class="rename" onSubmit=${formAction(() => api(method, path, { name: value }).then(() => { setEditing(false); done(); }).catch(show))}>
    <input ref=${input} required=${!optional} maxlength=${max} value=${value} onInput=${onInput(setValue)} aria-label=${'New name for ' + label}/>
    <button type="submit">Save</button>
    <button type="button" class="btn-secondary" onClick=${() => setEditing(false)}>Cancel</button>
  </form>`;
}
