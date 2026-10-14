// The turn history on the admin page (003-AC27): a filtered list, daily totals per account, and
// the CSV export of what the filter matches.
import { useState, useEffect } from 'preact/hooks';
import { html } from 'htm/preact';
import { api, download } from './api.js';

/** @typedef {import('./api.js').Turn} Turn */
/** @typedef {import('./api.js').History} History */
/** @typedef {{instance: string, room: string, persona: string, account: string, status: string, from: string, to: string}} Filter */

/** @type {Filter} */
const noFilter = { instance: '', room: '', persona: '', account: '', status: '', from: '', to: '' };

/** @param {Filter} f */
const query = (f) => new URLSearchParams(Object.entries(f).filter(([, v]) => v)).toString();

/** @param {number} n */
const count = (n) => (n >= 1e6 ? (n / 1e6).toFixed(1) + 'M' : n >= 1e4 ? Math.round(n / 1e3) + 'k' : String(n));
/** @param {number} b */
const bytes = (b) => (b >= 1 << 20 ? (b / (1 << 20)).toFixed(1) + ' MiB' : b >= 1024 ? Math.round(b / 1024) + ' KiB' : b + ' B');
/** @param {number} ms */
const secs = (ms) => (ms >= 60e3 ? Math.floor(ms / 60e3) + ' min ' + Math.round((ms % 60e3) / 1e3) + ' s' : (ms / 1e3).toFixed(1) + ' s');
/** @param {number | null} c */
const usd = (c) => (c === null ? 'unknown' : '$' + c.toFixed(c < 1 ? 4 : 2));
/** Turns before 005-AC9 may carry neither. @param {string} m @param {string} e */
const level = (m, e) => (m || 'unset') + ' · effort ' + (e || 'unset');
/** The other models of the turn, or what claude reported instead (005-AC10). @param {Turn} t */
const note = (t) => (t.model_mismatch ? ' · ⚠ claude reported ' + (t.model_note || 'no model') : t.model_note ? ' · ' + t.model_note : '');

/**
 * Cut is a cell's text cut to one line; its full value shows in the page's tip, as a native title
 * would not on touch nor styled.
 * @param {{text: string, tip?: string}} p
 */
const Cut = ({ text, tip = text }) => html`<span class="cut" tabindex="0" data-tip=${tip}>${text}</span>`;

/** The room's id, the tail of its link, for a room no longer served. @param {string} url */
const roomID = (url) => url.replace(/\/+$/, '').split('/').pop() || url;

/** @param {{t: Turn, room: string}} p */
function TurnRow({ t, room }) {
  const pct = t.context_window ? Math.floor((t.context_tokens * 100) / t.context_window) + '%' : '';
  return html`<tr class=${'status-' + t.status.replace(' ', '-')}>
    <td data-label="Ended">${new Date(t.at).toLocaleString()}</td>
    <td data-label="Room"><${Cut} text=${room} tip=${t.room} /></td>
    <td data-label="Persona">${t.persona}</td>
    <td data-label="Account">${t.account}</td>
    <td data-label="Model" class=${t.model_mismatch ? 'bad' : ''}><${Cut} text=${level(t.asked_model, t.asked_effort) + ' → ' + level(t.model, t.effort) + note(t)} /></td>
    <td data-label="Why"><${Cut} text=${t.cause} /></td>
    <td data-label="Took">${secs(t.duration_ms)}</td>
    <td data-label="Queued">${secs(t.queue_ms)}</td>
    <td data-label="Status">${t.error ? html`<${Cut} text=${t.status} tip=${t.error} />` : t.status}</td>
    <td data-label="Tokens">${t.model ? count(t.tokens_in) + ' / ' + count(t.tokens_out) + ' / ' + count(t.tokens_cache_read + t.tokens_cache_write) : 'unknown'}</td>
    <td data-label="Cost">${usd(t.cost_usd)}</td>
    <td data-label="Context" class=${pct && t.context_tokens * 100 > 80 * t.context_window ? 'bad' : ''}>${pct}</td>
    <td data-label="Workspace">${bytes(t.workspace_bytes)}</td>
  </tr>`;
}

/**
 * Monitoring filters the history by instance, room, persona, account, status and dates.
 * @param {{instances: import('./api.js').Instance[]}} p
 */
export function Monitoring({ instances }) {
  const [f, setF] = useState(noFilter);
  const [h, setH] = useState(/** @type {History | null} */ (null));
  const [err, setErr] = useState('');
  useEffect(() => {
    let timer = 0;
    let live = true;
    const load = () => api('GET', '/api/monitor/turns?' + query(f))
      .then((r) => { if (live) { setH(r); setErr(''); } })
      .catch((x) => { if (live) setErr(x.message); })
      .finally(() => { if (live) timer = setTimeout(load, 5000); });
    load();
    return () => { live = false; clearTimeout(timer); };
  }, [query(f)]);
  const url = (/** @type {string} */ id) => instances.find((i) => i.id === id)?.url || id;
  // The workers learn each room's name from its hello (008); a room no longer served shows its id.
  const names = new Map(instances.flatMap((i) => i.room_states || []).filter((r) => r.name).map((r) => [r.room, r.name || '']));
  const roomName = (/** @type {string} */ link) => names.get(link) || roomID(link);
  /** @param {keyof Filter} k @param {string} label @param {string[]} values @param {(v: string) => string} [name] */
  const pick = (k, label, values, name = (v) => v) => html`<label>${label} <select id=${'filter-' + k} value=${f[k]}
    onChange=${(/** @type {any} */ e) => setF({ ...f, [k]: e.currentTarget.value })}>
    <option value="">any</option>${values.map((v) => html`<option value=${v}>${name(v)}</option>`)}
  </select></label>`;
  /** @param {'from' | 'to'} k @param {string} label */
  const date = (k, label) => html`<label>${label} <input id=${'filter-' + k} type="date" value=${f[k]}
    onChange=${(/** @type {any} */ e) => setF({ ...f, [k]: e.currentTarget.value })} /></label>`;
  const fc = h?.facets;
  const exportCSV = () => download('/api/monitor/turns.csv?' + query(f), 'grill-room-turns.csv').catch((x) => setErr(x.message));
  return html`<section id="monitoring-section" aria-labelledby="monitoring-title">
    <h2 id="monitoring-title">Activity</h2>
    <p class="hint">Every turn this machine ran, kept on this machine only for the retention set above. Cost is Claude Code's estimate.</p>
    <form id="monitor-filter" class="row" onSubmit=${(/** @type {Event} */ e) => e.preventDefault()}>
      ${pick('instance', 'Instance', fc?.instances || [], url)} ${pick('room', 'Room', fc?.rooms || [], roomName)}
      ${pick('persona', 'Persona', fc?.personas || [])} ${pick('account', 'Account', fc?.accounts || [])}
      ${pick('status', 'Status', fc?.statuses || [])} ${date('from', 'From')} ${date('to', 'To')}
      <button type="button" onClick=${() => setF(noFilter)}>Clear</button>
      <button type="button" onClick=${exportCSV}>Export CSV</button>
    </form>
    <p class="bad" role="alert">${err}</p>
    <h3 id="totals-title">Daily totals per account</h3>
    ${h && (h.totals.length ? html`<div class="scroll"><table id="totals" class="cards" aria-labelledby="totals-title">
      <thead><tr><th scope="col">Day</th><th scope="col">Account</th><th scope="col">Turns</th><th scope="col">Tokens in / out / cache</th><th scope="col">Cost</th></tr></thead>
      <tbody>${h.totals.map((t) => html`<tr>
        <td data-label="Day">${t.day}</td><td data-label="Account">${t.account}</td><td data-label="Turns">${t.turns}</td>
        <td data-label="Tokens">${count(t.tokens_in)} / ${count(t.tokens_out)} / ${count(t.tokens_cache_read + t.tokens_cache_write)}</td>
        <td data-label="Cost">${usd(t.cost_usd)}${t.unknown_cost ? ' + ' + t.unknown_cost + ' unknown' : ''}</td>
      </tr>`)}</tbody>
    </table></div>` : html`<p>No turn yet.</p>`)}
    <h3 id="turns-title">Turns</h3>
    ${h && h.turns.length >= h.limit && html`<p class="hint">The latest ${h.limit} shown; the CSV holds them all.</p>`}
    ${h && (h.turns.length ? html`<div class="scroll"><table id="turns" class="cards" aria-labelledby="turns-title">
      <thead><tr>
        <th scope="col">Ended</th><th scope="col">Room</th><th scope="col">Persona</th><th scope="col">Account</th>
        <th scope="col">Model · effort, asked → ran</th><th scope="col">Why</th><th scope="col">Took</th><th scope="col">Queued</th>
        <th scope="col">Status</th><th scope="col">Tokens in / out / cache</th><th scope="col">Cost</th><th scope="col">Context</th>
        <th scope="col">Workspace</th>
      </tr></thead>
      <tbody>${h.turns.map((t) => html`<${TurnRow} key=${t.id} t=${t} room=${roomName(t.room)} />`)}</tbody>
    </table></div>` : html`<p>No turn matches.</p>`)}
  </section>`;
}
