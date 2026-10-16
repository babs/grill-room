// The admin page's session and API calls, and the shapes of what the supervisor sends.

/**
 * @typedef {{ok: boolean, reason?: string, desktop: boolean, version?: string}} Readiness
 * @typedef {{state: string, ref?: string, progress?: string, error?: string}} Image
 * @typedef {{available?: string, served?: string, from?: string, running: boolean, error?: string}} Update
 * @typedef {{max_model: string, max_effort: string, approve_all: boolean}} Settings
 * @typedef {{cpus: string, memory: string, egress_cpus: string, egress_memory: string}} Limits
 * @typedef {{key: string, title?: string, description?: string, type?: string, sensitive?: boolean, required?: boolean, default?: unknown}} PluginOption
 * @typedef {{id: number, name: string, version: number, options?: PluginOption[], values?: Record<string, unknown>, set?: string[], needs_setup?: boolean,
 *   domains?: string[], off_allowlist?: string[]}} HostPlugin
 * @typedef {{id: number, plugin_name?: string, url: string, ref: string, entries?: string[], requested_by: string, created_at: string,
 *   fetching?: boolean}} HostFetch
 * @typedef {{room: string, pairing: string, code?: string, approve_url?: string, connection: string, error?: string,
 *   can_host: boolean, reason?: string, running?: number, queued?: number, host?: boolean, parallel_turns?: boolean, name?: string,
 *   plugins?: HostPlugin[], fetches?: HostFetch[]}} RoomStatus
 * @typedef {{host: string, version: string, room?: string, pairing: string, code?: string, approve_url?: string,
 *   connection: string, error?: string, docker: Readiness, image: Image, update: Update, can_host: boolean, reason?: string,
 *   settings: Settings, allowlist: string[], egress?: EgressState, rooms?: RoomStatus[]}} Status
 * @typedef {{domain: string, by?: string, at?: string}} AllowEntry
 * @typedef {{id: string, name: string, state: string, detail?: string, on: boolean, actions: boolean}} ConnectorView
 * @typedef {{all_websites: boolean, local_network: boolean, server: AllowEntry[], rooms?: Record<string, AllowEntry[]>}} EgressState
 * @typedef {{uptime_seconds: number, turns: number, failed_turns: number, heap_bytes: number, goroutines: number}} Metrics
 * @typedef {{id: string, url: string, rooms: string[], state: string, error?: string, pid?: number, proto?: number,
 *   restarts: number, version?: string, trust?: string, accept_unsigned: boolean, account: string,
 *   room_accounts?: Record<string, string>, room_states?: RoomStatus[], requests: number, cut_off?: boolean,
 *   room?: string, connection?: string, metrics?: Metrics, unsigned?: boolean, limits: Limits, status?: Status, paused?: string[],
 *   update_needed?: string}} Instance
 * @typedef {{name: string, kind: string, config_dir?: string, display_dir?: string, storage?: string, state: string, detail?: string,
 *   expires_at?: string, expiry_estimated?: boolean, checked_at?: string, last_use?: string,
 *   requests: Record<string, number>, warning?: string}} Account
 * @typedef {{instance: string, room: string, turn: number, since_ms: number}} Slot
 * @typedef {{max_turns: number, running: Slot[], queued: Slot[], granted: number, waited: number,
 *   wait_total_ms: number, wait_max_ms: number}} Concurrency
 * @typedef {{name: string, fingerprint: string}} DevKey
 * @typedef {{self: {state: string, detail?: string}, root: {source: string, at: string, error?: string},
 *   dev_keys: DevKey[], channel: string, follow?: string, check_error?: string, checked: string}} Trust
 * @typedef {{kind: string, text: string}} Alert
 * @typedef {{status: Status, logs: string[], models: string[], efforts: string[], instances?: Instance[], instance?: string,
 *   autostart?: boolean, trust?: Trust, accounts?: Account[], concurrency?: Concurrency, retention_days?: number,
 *   alerts?: Alert[]}} StatusReply
 * @typedef {{id: string, created: string, used: string, this?: boolean}} Browser
 * @typedef {{host: string, time: string}} Denied
 * @typedef {{id: number, at: string, day: string, instance: string, room: string, turn_id: number, persona: string,
 *   account: string, asked_model: string, asked_effort: string, model: string, model_note?: string,
 *   model_mismatch?: boolean, effort: string, cause: string,
 *   duration_ms: number, queue_ms: number, status: string, error?: string, tokens_in: number, tokens_out: number,
 *   tokens_cache_read: number, tokens_cache_write: number, cost_usd: number | null, context_tokens: number,
 *   context_window: number, workspace_bytes: number}} Turn
 * @typedef {{day: string, account: string, turns: number, tokens_in: number, tokens_out: number,
 *   tokens_cache_read: number, tokens_cache_write: number, cost_usd: number, unknown_cost: number}} Total
 * @typedef {{instances: string[], rooms: string[], personas: string[], accounts: string[], statuses: string[]}} Facets
 * @typedef {{turns: Turn[], totals: Total[], facets: Facets, limit: number}} History
 */

/** @type {string} */
let token = '';

// The link's one-time code is traded for a session token kept in localStorage: only this page's
// origin, 127.0.0.1 and its port, reads it, and the browser never sends it on its own, so other
// sites, other localhost ports included, cannot use it. The app remembers the session, so a
// bookmark of the page keeps working until the session is forgotten on the General tab.
export async function startSession() {
  const m = location.pathname.match(/^\/s\/([^/]+)$/);
  if (m) {
    history.replaceState(null, '', '/');
    const res = await fetch('/api/session', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code: m[1] }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || 'HTTP ' + res.status);
    localStorage.setItem('token', data.token);
  }
  // A tab of a version before localStorage kept its token in sessionStorage; a self-update admits it.
  const old = sessionStorage.getItem('token');
  if (old) {
    if (!localStorage.getItem('token')) localStorage.setItem('token', old);
    sessionStorage.removeItem('token');
  }
  token = localStorage.getItem('token') || '';
  if (!token) throw new Error(signIn);
}

const signIn = 'This browser is not signed in to the AI host app: run `grill-room-daemon admin` on this computer, or open the latest link the app printed.';

/** forgetToken drops this browser's session, which the app forgot. */
export function forgetToken() {
  localStorage.removeItem('token');
  token = '';
}

/**
 * fetchAPI sends an authenticated request and returns the raw response, failed with the server's
 * error text.
 * @param {string} method
 * @param {string} path
 * @param {unknown} [body]
 */
async function fetchAPI(method, path, body) {
  /** @type {RequestInit & {headers: Record<string, string>}} */
  const opts = { method, headers: { Authorization: 'Bearer ' + token } };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401 && token) {
    // The session was forgotten or expired: the next visit asks for a new link.
    forgetToken();
    throw new Error(signIn);
  }
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.error || 'HTTP ' + res.status);
  }
  return res;
}

/**
 * api sends a request and returns its JSON answer, {} for none.
 * @param {string} method
 * @param {string} path
 * @param {unknown} [body]
 * @returns {Promise<any>}
 */
export async function api(method, path, body) {
  const res = await fetchAPI(method, path, body);
  return res.status === 204 ? {} : res.json();
}

/**
 * download saves what path answers as a file: the CSV needs the session token, which a plain link
 * would not send.
 * @param {string} path
 * @param {string} name
 */
export async function download(path, name) {
  const blob = await (await fetchAPI('GET', path)).blob();
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 10_000);
}
