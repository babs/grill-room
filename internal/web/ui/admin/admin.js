// The instance admin page (004-AC1, 004-AC6, 004-AC19): Preact, htm and signals, vendored, no build
// step. The session cookie is HttpOnly: the page never sees a token after the login form.
import { render } from 'preact';
import { useState, useEffect } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';

/**
 * @typedef {{id: number, created_at: string, last_used_at: string, expires_at: string, user_agent: string, ip: string,
 *   current: boolean}} Session
 * @typedef {{id: number, at: string, actor: string, action: string, target_kind: string, target_id: string,
 *   target_name: string, details: Record<string, unknown>}} Entry
 * @typedef {{entries: Entry[], next: number, actions: string[]}} AuditPage
 */

/** @type {import('@preact/signals').Signal<boolean | null>} null until the first answer says. */
const signedIn = signal(null);
const notice = signal('');
/** @param {string} hash */
const viewOf = (hash) => (hash === '#audit' ? 'audit' : 'sessions');
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

function Shell() {
  const logout = () => api('POST', '/api/admin/logout').then(() => { signedIn.value = false; }).catch(show);
  /** @param {string} id @param {string} name */
  const link = (id, name) => html`<a href=${'#' + id} aria-current=${view.value === id ? 'page' : undefined}>${name}</a>`;
  return html`
    <header>
      <h1>grill-room admin</h1>
      <nav aria-label="Views">${link('sessions', 'Sessions')}${link('audit', 'Audit log')}</nav>
      <button type="button" id="logout" onClick=${logout}>Sign out</button>
    </header>
    <main>
      ${notice.value && html`<p class="said bad" role="alert">${notice.value}</p>`}
      ${view.value === 'audit' ? html`<${Audit}/>` : html`<${Sessions}/>`}
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
