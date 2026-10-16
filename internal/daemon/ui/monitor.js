// The turn history on the admin page (003-AC27): a filtered list, daily totals per account, and
// the CSV export of what the filter matches.
import { useState, useEffect, useRef } from 'preact/hooks';
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

/**
 * Status is a turn's status as a pill (docs/design.md §5), its error in the tip.
 * @param {{t: Turn}} p
 */
const Status = ({ t }) => html`<span class=${'status ' + (t.status === 'ok' ? 'ok' : ['failed', 'conflict', 'expired', 'cut off'].includes(t.status) ? 'failed' : 'idle')}
  tabindex=${t.error ? 0 : undefined} data-tip=${t.error || undefined}>${t.status}</span>`;

/** The room's id, the tail of its link, for a room no longer served. @param {string} url */
const roomID = (url) => url.replace(/\/+$/, '').split('/').pop() || url;

/** The model and effort asked, then run, with claude's note. @param {Turn} t */
const ran = (t) => level(t.asked_model, t.asked_effort) + ', ran ' + level(t.model, t.effort) + note(t);

/**
 * TurnRow is one answer in one line (003b-AC18): the rest is in its sheet, which a click or Enter
 * opens.
 * @param {{t: Turn, room: string, onOpen: () => void}} p
 */
function TurnRow({ t, room, onOpen }) {
  return html`<tr class="turn-row" tabindex="0" onClick=${onOpen} onKeyDown=${(/** @type {KeyboardEvent} */ e) => { if (e.key === 'Enter') onOpen(); }}>
    <td data-label="Ended"><span data-tip=${window.grillDate(t.at)}>${window.grillAgo(t.at)}</span></td>
    <td data-label="Room"><${Cut} text=${room} tip=${t.room} /></td>
    <td data-label="AI persona">${t.persona}</td>
    <td data-label="Model" class=${'wide-only' + (t.model_mismatch ? ' bad' : '')}><${Cut} text=${level(t.model, t.effort)} tip=${ran(t)} /></td>
    <td data-label="Status"><${Status} t=${t} /></td>
    <td data-label="Took" class="wide-only">${secs(t.duration_ms)}</td>
    <td data-label="Cost" class="wide-only">${usd(t.cost_usd)}</td>
  </tr>`;
}

/**
 * TurnSheet is every field of one answer.
 * @param {{t: Turn | null, room: string, onClose: () => void}} p
 */
function TurnSheet({ t, room, onClose }) {
  const ref = useRef(/** @type {HTMLDialogElement | null} */ (null));
  useEffect(() => { if (t && ref.current && !ref.current.open) ref.current.showModal(); }, [t]);
  const pct = t && t.context_window ? Math.floor((t.context_tokens * 100) / t.context_window) + '%' : 'unknown';
  return html`<dialog id="turn-sheet" ref=${ref} class="sheet full" aria-labelledby="turn-sheet-title" onClose=${onClose}>
    ${t && html`<div class="sheet-head"><h2 id="turn-sheet-title">${t.persona} in ${room}</h2>
      <button type="button" class="btn-icon btn-ghost" aria-label="Close" onClick=${() => ref.current?.close()} dangerouslySetInnerHTML=${{ __html: window.grillIconHTML('close') }}></button></div>
    <dl class="turn-facts">
      <dt>Ended</dt><dd>${window.grillDate(t.at)}</dd>
      <dt>Room</dt><dd><code>${t.room}</code></dd>
      <dt>AI account</dt><dd>${t.account}</dd>
      <dt>Model</dt><dd class=${t.model_mismatch ? 'bad' : ''}>${ran(t)}</dd>
      <dt>Why</dt><dd>${t.cause}</dd>
      <dt>Status</dt><dd><${Status} t=${t} />${t.error ? ' ' + t.error : ''}</dd>
      <dt>Took</dt><dd>${secs(t.duration_ms)}, ${secs(t.queue_ms)} waiting for a slot</dd>
      <dt>Tokens</dt><dd>${t.model ? count(t.tokens_in) + ' in, ' + count(t.tokens_out) + ' out, ' + count(t.tokens_cache_read + t.tokens_cache_write) + ' cache' : 'unknown'}</dd>
      <dt>Cost</dt><dd>${usd(t.cost_usd)}</dd>
      <dt>Context</dt><dd class=${t.context_window && t.context_tokens * 100 > 80 * t.context_window ? 'bad' : ''}>${pct}</dd>
      <dt>Workspace</dt><dd>${bytes(t.workspace_bytes)}</dd>
    </dl>`}
  </dialog>`;
}

/**
 * Monitoring filters the history by instance, room, persona, account, status and dates.
 * @param {{instances: import('./api.js').Instance[]}} p
 */
export function Monitoring({ instances }) {
  const [f, setF] = useState(noFilter);
  const [h, setH] = useState(/** @type {History | null} */ (null));
  const [err, setErr] = useState('');
  const [shown, setShown] = useState(0);
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
  // A server by its host name, as on Rooms.
  const url = (/** @type {string} */ id) => { const u = instances.find((i) => i.id === id)?.url; return u ? new URL(u).host : id; };
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
  const open = h?.turns.find((t) => t.id === shown) || null;
  return html`<section id="monitoring-section" aria-labelledby="monitoring-title">
    <h2 id="monitoring-title">Activity</h2>
    <p class="hint">Every answer this computer ran, kept here only, for the days set on Settings. Cost is Claude Code's estimate.</p>
    <form id="monitor-filter" class="filters" onSubmit=${(/** @type {Event} */ e) => e.preventDefault()}>
      ${pick('instance', 'Server', fc?.instances || [], url)} ${pick('room', 'Room', fc?.rooms || [], roomName)}
      ${pick('persona', 'AI persona', fc?.personas || [])} ${pick('account', 'AI account', fc?.accounts || [])}
      ${pick('status', 'Status', fc?.statuses || [])} ${date('from', 'From')} ${date('to', 'To')}
      <div class="filter-actions"><button type="button" class="btn-secondary" onClick=${() => setF(noFilter)}>Clear</button>
        <button type="button" class="btn-secondary" onClick=${exportCSV}>Export CSV</button></div>
    </form>
    <p class="bad" role="alert">${err}</p>
    <h3 id="totals-title">Daily totals per AI account</h3>
    ${h && (h.totals.length ? html`<div class="scroll"><table id="totals" class="cards" aria-labelledby="totals-title">
      <thead><tr><th scope="col">Day</th><th scope="col">AI account</th><th scope="col">Answers</th><th scope="col">Tokens in / out / cache</th><th scope="col">Cost</th></tr></thead>
      <tbody>${h.totals.map((t) => html`<tr>
        <td data-label="Day">${t.day}</td><td data-label="AI account">${t.account}</td><td data-label="Answers">${t.turns}</td>
        <td data-label="Tokens">${count(t.tokens_in)} / ${count(t.tokens_out)} / ${count(t.tokens_cache_read + t.tokens_cache_write)}</td>
        <td data-label="Cost">${usd(t.cost_usd)}${t.unknown_cost ? ' + ' + t.unknown_cost + ' unknown' : ''}</td>
      </tr>`)}</tbody>
    </table></div>` : html`<p>No answer yet.</p>`)}
    <h3 id="turns-title">Answers</h3>
    ${h && h.turns.length >= h.limit && html`<p class="hint">The latest ${h.limit} shown; the CSV holds them all.</p>`}
    ${h && (h.turns.length ? html`<div class="scroll"><table id="turns" aria-labelledby="turns-title">
      <thead><tr>
        <th scope="col">Ended</th><th scope="col">Room</th><th scope="col">AI persona</th><th scope="col" class="wide-only">Model</th>
        <th scope="col">Status</th><th scope="col" class="wide-only">Took</th><th scope="col" class="wide-only">Cost</th>
      </tr></thead>
      <tbody>${h.turns.map((t) => html`<${TurnRow} key=${t.id} t=${t} room=${roomName(t.room)} onOpen=${() => setShown(t.id)} />`)}</tbody>
    </table></div>` : html`<p>No answer matches.</p>`)}
    <${TurnSheet} t=${open} room=${open ? roomName(open.room) : ''} onClose=${() => setShown(0)} />
  </section>`;
}
