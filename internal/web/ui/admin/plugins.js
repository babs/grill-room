// Room plugins (spec 009): Room settings › Plugins, the add sheet and the host's approval sheet.
// Preact, htm and signals, like /admin. room.js loads the list (GET .../plugins) on each "plugins"
// event and hands it over as window.grillPlugins, followed by a grill:plugins event; its status
// strip opens the approval sheet with a grill:plugin-review event.
import { render } from 'preact';
import { useState, useEffect, useLayoutEffect } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';
import { fetchJSON, size, formAction, icon } from './room-panel.js';
import { RowMenu } from './starters.js';

/**
 * @typedef {{event: string, command: string}} Hook
 * @typedef {{name: string, command?: string, url?: string}} Server
 * @typedef {{name: string, version?: string, description?: string, skills?: string[], commands?: string[], agents?: string[],
 *   hooks?: Hook[], mcp_servers?: Server[], lsp_servers?: Server[], bin?: string[], domains?: string[], problems?: string[]}} Manifest
 * @typedef {{always_on_tokens: number, cost_known: boolean}} Details
 * @typedef {{ok?: boolean, validate?: unknown, details?: Details, error?: string, allowlist_missing?: string[]}} Report
 * @typedef {{components_added?: string[], components_removed?: string[], files_added?: string[], files_removed?: string[],
 *   files_changed?: string[]}} Change
 * @typedef {{id: number, n: number, commit: string, short_commit: string, commit_url?: string, size: number, files: number,
 *   manifest: Manifest | null, report?: Report, state: string, reason?: string, added_by: string, added_at: string,
 *   change?: Change, compare_url?: string, imported?: boolean}} Version
 * @typedef {{id: number, name: string, url: string, ref: string, ref_name: string, private: boolean, personas: number[],
 *   running?: number, why?: string, needs_setup?: boolean, versions: Version[]}} Plugin
 * @typedef {{entry?: string, name: string, description?: string, version?: string, pickable: boolean, reason?: string,
 *   size?: number, files?: number}} Candidate
 * @typedef {{commit: string, short_commit: string, ref_name: string, commit_url?: string, marketplace: string, plugins: Candidate[]}} Check
 * @typedef {{id: number, name: string, badge: string}} Persona
 * @typedef {{id: number, plugin_id?: number, plugin_name?: string, url: string, ref: string, entries: string[], personas: number[],
 *   state: string, reason?: string, requested_by: string, created_at: string}} Fetch
 */

const w = /** @type {Window & {grillPlugins?: Plugin[]}} */ (window);
const plugins = signal(w.grillPlugins || []);
addEventListener('grill:plugins', (e) => { plugins.value = /** @type {CustomEvent<Plugin[]>} */ (e).detail; });

/** @param {string} text */
const toast = (text) => dispatchEvent(new CustomEvent('grill:toast', { detail: text }));
/** @param {string} s a server's reason, "the server cannot…" */
const sentence = (s) => { const t = s.charAt(0).toUpperCase() + s.slice(1); return /[.!?]$/.test(t) ? t : t + '.'; };
/** @param {number} n */
const tokens = (n) => '~' + n.toLocaleString('en') + ' tokens';

/** @param {Plugin} p */
const running = (p) => p.versions.find((v) => v.n === p.running);
/** @param {Plugin} p */
const newest = (p) => p.versions[p.versions.length - 1];

/**
 * stateWords says where a version stands, in the room's words (009-AC13, 009-AC18).
 * @param {Version} v
 */
function stateWords(v) {
  switch (v.state) {
    case 'pending': return "waiting for the AI host's approval";
    case 'approved': return 'approved';
    case 'refused': return 'refused by the AI host';
    default: return v.reason || v.state; // inspecting and invalid carry the server's words
  }
}

/**
 * cost is a version's token cost per answer from its inspection, in words.
 * @param {Version | undefined} v
 */
function cost(v) {
  const d = v?.report?.details;
  return d && d.cost_known ? tokens(d.always_on_tokens) + ' per answer' : 'unknown';
}

/**
 * warnings gathers the warnings of `claude plugin validate --json`, wherever its output nests them.
 * @param {unknown} out
 * @returns {string[]}
 */
function warnings(out) {
  /** @type {string[]} */
  const found = [];
  /** @param {unknown} o */
  const walk = (o) => {
    if (Array.isArray(o)) { o.forEach(walk); return; }
    if (!o || typeof o !== 'object') return;
    for (const [k, x] of Object.entries(o)) {
      if (k !== 'warnings' || !Array.isArray(x)) { walk(x); continue; }
      for (const item of x) {
        const it = /** @type {{path?: string, message?: string} | string} */ (item);
        found.push(typeof it === 'string' ? it : (it.path ? it.path + ': ' : '') + (it.message || ''));
      }
    }
  };
  walk(out);
  return found;
}

/**
 * Commit is a short commit, linked to the repo's page when the server knows its form.
 * @param {{v: {short_commit: string, commit_url?: string}}} props
 */
const Commit = ({ v }) => (v.commit_url ? html`<a href=${v.commit_url} target="_blank" rel="noopener"><code>${v.short_commit}</code></a>` : html`<code>${v.short_commit}</code>`);

/**
 * PluginsTab lists the room's plugins for every member (009-AC5): "<name>, <ref> @ <commit>", its
 * private badge, where its running and newest versions stand, and for room admins Update and
 * Remove in ⋯ (009-AC11, 009-AC12) and the add sheet. The host reviews a version waiting for them.
 * @param {{base: string, admin: boolean, host: boolean, personas: Persona[]}} props
 */
export function PluginsTab({ base, admin, host, personas }) {
  const [said, setSaid] = useState('');
  /** @param {unknown} err */
  const show = (err) => setSaid(/** @type {Error} */ (err).message);
  const list = plugins.value;
  // After a decision taken from a row, the redraw that drops its Review button moves the focus.
  useLayoutEffect(() => { if (refocus?.home === 'plugin-list') landFocus(); });
  /** @param {Plugin} p */
  const actions = (p) => /** @type {[string, () => void][]} */ ([
    ['Update', () => fetchJSON('POST', `${base}/plugins/${p.id}/update`).then((/** @type {Plugin & {up_to_date?: boolean, host_fetch?: Fetch}} */ r) => {
      setSaid('');
      if (r.up_to_date) return toast('Up to date');
      if (r.host_fetch) return toast(`Asked the AI host to fetch the update of ${p.name}`);
      const v = newest(r);
      toast(`${p.name}: new version v${v.n}, ${r.ref_name} @ ${v.short_commit}`);
    }).catch(show)],
    ['Remove', () => window.grillConfirm(`Remove ${p.name}?`, 'Every persona loses it from its next answer, and its files leave the room.', 'Remove')
      .then((yes) => { if (yes) fetchJSON('DELETE', `${base}/plugins/${p.id}`).then(() => { setSaid(''); toast('Removed ' + p.name); }).catch(show); })],
  ]);
  /** @param {Plugin} p */
  const on = (p) => p.personas.map((id) => personas.find((x) => x.id === id)?.name).filter(Boolean).join(', ');
  const fetches = useFetches(base);
  /** @param {Fetch} f */
  const drop = (f) => fetchJSON('DELETE', `${base}/plugin-fetches/${f.id}`).then(() => setSaid('')).catch(show);
  return html`
    <p class="hint">Claude Code plugins from git repositories, pinned at a commit: nothing follows the branch by itself. A room admin adds them and turns them on per persona, in its Capabilities tab. The AI host checks each version first.</p>
    <p class="said" role="alert">${said}</p>
    ${list.length ? html`<ul class="plugin-list" id="plugin-list" tabindex="-1" aria-label="The room's plugins">${list.map((p) => {
      const run = running(p);
      const last = newest(p);
      const shown = run || last;
      const later = last && last !== run ? last : null;
      return html`<li key=${p.id} data-plugin=${p.name}>
        <div class="plugin-text">
          <span class="plugin-name"><strong>${p.name}</strong>, ${p.ref_name} @ <${Commit} v=${shown}/>${p.private && html` <span class="badge">private</span>`}</span>
          <span class="hint plugin-state">${run ? `v${run.n} in use` : ''}${run && later ? ' · ' : ''}${later ? `v${later.n}: ${stateWords(later)}` : ''}${p.needs_setup ? ` · ${p.why}` : ''}</span>
          <span class="hint">${on(p) ? 'On for ' + on(p) : 'On for no persona yet'}</span>
        </div>
        ${host && later?.state === 'pending' && html`<button type="button" class="btn-secondary" aria-label=${`Review the plugin ${p.name} v${later.n}`}
          onClick=${() => review(p.id, later.n)}>Review</button>`}
        ${admin && html`<${RowMenu} id=${'plugin-menu-' + p.id} label=${'More actions for the plugin ' + p.name} items=${actions(p)}/>`}
      </li>`;
    })}</ul>`
      : html`<p class="empty" id="plugins-empty">No plugins yet.${admin ? ' Add one from a git repository.' : ' A room admin adds them from git repositories.'}</p>`}
    ${fetches.length ? html`<h3 class="plugin-fetches-title">Asked of the AI host</h3>
      <ul class="plugin-list" id="plugin-fetches" aria-label="Requests to the AI host">${fetches.map((f) => html`<li key=${f.id} data-fetch=${f.id}>
        <div class="plugin-text">
          <span class="plugin-name">${f.plugin_name ? html`Update of <strong>${f.plugin_name}</strong>` : html`<code>${f.url}</code>`}${f.ref ? ', ' + f.ref : ''}</span>
          <span class="hint plugin-state">${f.state === 'waiting' ? 'waiting for the AI host to fetch it' : sentence(f.reason || 'the fetch failed')}</span>
          <span class="hint">Asked by ${f.requested_by}</span>
        </div>
        ${admin && html`<button type="button" class="btn-secondary" aria-label=${(f.state === 'waiting' ? 'Cancel the request for ' : 'Dismiss the request for ') + (f.plugin_name || f.url)}
          onClick=${() => drop(f)}>${f.state === 'waiting' ? 'Cancel' : 'Dismiss'}</button>`}
      </li>`)}</ul>` : ''}
    ${admin && html`<div class="row"><button type="button" class="btn-primary" id="plugin-add-open" onClick=${() => openAdd(base, personas)}>Add a plugin</button></div>`}`;
}

/**
 * useFetches is the room's requests to the AI host (009-AC19), read again on each plugins event,
 * which their changes raise too.
 * @param {string} base
 * @returns {Fetch[]}
 */
function useFetches(base) {
  const [list, setList] = useState(/** @type {Fetch[]} */ ([]));
  // plugins.value changes on each plugins event: read the requests again then.
  const seen = plugins.value;
  useEffect(() => {
    let live = true;
    fetchJSON('GET', base + '/plugins').then((r) => { if (live) setList(r.fetches || []); }).catch(() => {});
    return () => { live = false; };
  }, [base, seen]);
  return list;
}

const addSheet = /** @type {HTMLDialogElement} */ (document.getElementById('plugin-add'));
const reviewSheet = /** @type {HTMLDialogElement} */ (document.getElementById('plugin-review'));
addSheet.addEventListener('close', () => render(null, addSheet));

/**
 * openAdd opens the add sheet afresh: nothing typed, nothing checked.
 * @param {string} base @param {Persona[]} personas
 */
function openAdd(base, personas) {
  render(html`<${AddSheet} base=${base} personas=${personas} close=${() => addSheet.close()}/>`, addSheet);
  addSheet.showModal();
}

/**
 * AddSheet adds plugins from a repo (009-AC5, 009-AC6, 009-AC10): its link and a ref, Check, then
 * the marketplace's entries to pick, none checked, those it cannot add disabled with why, or the
 * repo's one plugin; then the personas to turn them on for, none checked.
 * @param {{base: string, personas: Persona[], close: () => void}} props
 */
function AddSheet({ base, personas, close }) {
  const [url, setURL] = useState('');
  const [ref, setRef] = useState('');
  const [check, setCheck] = useState(/** @type {Check | null} */ (null));
  const [entries, setEntries] = useState(/** @type {string[]} */ ([]));
  const [on, setOn] = useState(/** @type {number[]} */ ([]));
  const [said, setSaid] = useState('');
  // The link and ref stay read-only while a request runs: Add must send what Check listed.
  const [busy, setBusy] = useState(false);
  // viaHost is set once the server said it cannot fetch the repo: the AI host may (009-AC19).
  const [viaHost, setViaHost] = useState(false);
  const [names, setNames] = useState('');
  /** @param {unknown} err */
  const fail = (err) => {
    const e = /** @type {Error & {data?: {host_fetch?: boolean}}} */ (err);
    setSaid(sentence(e.message));
    if (e.data?.host_fetch) setViaHost(true);
  };
  /** @param {(v: string) => void} set */
  const edit = (set) => (/** @type {Event} */ e) => { set(/** @type {HTMLInputElement} */ (e.currentTarget).value); setCheck(null); setEntries([]); setViaHost(false); };
  const askHost = () => {
    setBusy(true);
    setSaid('');
    const picked = names.split(/[\s,]+/).filter(Boolean);
    fetchJSON('POST', base + '/plugins/host-fetch', { url, ref, entries: picked, personas: on }).then(() => {
      close();
      toast('Asked the AI host. The request waits in Room settings › Plugins until the host fetches it.');
    }).catch((err) => setSaid(sentence(/** @type {Error} */ (err).message))).finally(() => setBusy(false));
  };
  const run = () => {
    setBusy(true);
    setSaid('');
    fetchJSON('POST', base + '/plugins/check', { url, ref }).then(setCheck).catch(fail).finally(() => setBusy(false));
  };
  const market = !!check?.marketplace;
  const one = check && !market ? check.plugins[0] : null;
  const ready = !!check && (market ? entries.length > 0 : !!one?.pickable);
  const add = () => {
    setBusy(true);
    setSaid('');
    fetchJSON('POST', base + '/plugins', { url, ref, entries: market ? entries : [], personas: on }).then(() => {
      close();
      toast('Added ' + (market ? entries.join(', ') : one?.name) + '. The AI host checks it next.');
    }).catch(fail).finally(() => setBusy(false));
  };
  /** @template T @param {T[]} list @param {T} x @param {boolean} yes */
  const toggle = (list, x, yes) => (yes ? [...list, x] : list.filter((y) => y !== x));
  /** @param {Candidate} c */
  const facts = (c) => [c.version && 'v' + c.version, c.size && size(c.size), c.files && c.files + ' files'].filter(Boolean).join(', ');
  return html`<div class="sheet-head"><h2 id="plugin-add-title">Add a plugin</h2>
      <button type="button" class="btn-ghost btn-icon" aria-label="Close" onClick=${close} dangerouslySetInnerHTML=${icon('close')}></button></div>
    <form class="plugin-form" onSubmit=${formAction(run)}>
      <label>Repository link<input id="plugin-url" required autocomplete="off" spellcheck="false" inputmode="url" placeholder="https://github.com/owner/repo"
        readOnly=${busy} value=${url} onInput=${edit(setURL)}/></label>
      <label>Git ref<input id="plugin-ref" autocomplete="off" spellcheck="false" placeholder="branch, tag or commit; empty = default branch"
        readOnly=${busy} value=${ref} onInput=${edit(setRef)}/></label>
      <div class="row"><button type="submit" class="btn-secondary" disabled=${busy || !url.trim()}>Check</button></div>
    </form>
    <p class="said error" role="alert">${said}</p>
    ${viaHost && html`<div class="plugin-host" id="plugin-host">
      <p>The AI host can fetch it with the git access of their own computer: ssh keys, git logins. Nothing is fetched until they press Fetch.</p>
      <div class="plugin-form"><label>Plugins to add, for a marketplace; empty for a repo that is one plugin<input id="plugin-names" autocomplete="off" spellcheck="false" placeholder="names, separated by commas"
        readOnly=${busy} value=${names} onInput=${(/** @type {Event} */ e) => setNames(/** @type {HTMLInputElement} */ (e.currentTarget).value)}/></label></div>
      <fieldset class="plugin-pick" id="plugin-host-personas"><legend>Turn it on for</legend>
        ${personas.length ? personas.map((p) => html`<div key=${p.id} class="pick"><label><input type="checkbox" checked=${on.includes(p.id)}
          onChange=${(/** @type {Event} */ e) => setOn(toggle(on, p.id, /** @type {HTMLInputElement} */ (e.currentTarget).checked))}/> ${p.badge} ${p.name}</label></div>`)
          : html`<p class="empty">No persona in the room yet.</p>`}</fieldset>
      <div class="row"><button type="button" class="btn-primary" id="plugin-ask-host" disabled=${busy} onClick=${askHost}>Ask the AI host to fetch it</button></div>
    </div>`}
    ${check && html`<p id="plugin-found">Found ${check.ref_name} @ <${Commit} v=${check}/>${market ? `, the marketplace ${check.marketplace}` : ''}.</p>`}
    ${market && html`<fieldset class="plugin-pick" id="plugin-entries"><legend>Plugins to add</legend>
      ${check?.plugins.map((c, i) => html`<div key=${c.entry || c.name} class="pick">
        <label><input type="checkbox" disabled=${!c.pickable} checked=${entries.includes(c.entry || '')} aria-describedby=${'plugin-why-' + i}
          onChange=${(/** @type {Event} */ e) => setEntries(toggle(entries, c.entry || '', /** @type {HTMLInputElement} */ (e.currentTarget).checked))}/> ${c.name}</label>
        <span class="hint" id=${'plugin-why-' + i}>${c.pickable ? [facts(c), c.description].filter(Boolean).join(' · ') : sentence(c.reason || '')}</span>
      </div>`)}</fieldset>`}
    ${one && html`<p id="plugin-one" class=${one.pickable ? '' : 'error'}>${one.pickable ? html`One plugin: <strong>${one.name}</strong>${facts(one) ? ', ' + facts(one) : ''}.`
      : `${one.name} cannot be added: ${sentence(one.reason || '')}`}</p>`}
    ${check && html`<fieldset class="plugin-pick" id="plugin-personas"><legend>Turn it on for</legend>
      ${personas.length ? personas.map((p) => html`<div key=${p.id} class="pick"><label><input type="checkbox" checked=${on.includes(p.id)}
        onChange=${(/** @type {Event} */ e) => setOn(toggle(on, p.id, /** @type {HTMLInputElement} */ (e.currentTarget).checked))}/> ${p.badge} ${p.name}</label></div>`)
        : html`<p class="empty">No persona in the room yet.</p>`}
      <p class="hint">None checked: turn it on later from a persona's Capabilities tab.</p></fieldset>`}
    <div class="actions"><button type="button" class="btn-secondary" onClick=${close}>Cancel</button>
      <button type="button" class="btn-primary" id="plugin-add-submit" disabled=${busy || !ready} onClick=${add}>Add</button></div>`;
}

const reviewing = signal(/** @type {{id: number, n: number} | null} */ (null));
/** @type {Element | null} */
let opener = null;
let home = 'plugin-list';
/** @type {{id: number, n: number, home: string} | null} */
let refocus = null;
/**
 * landFocus moves the focus once the decided version shows as decided, so after the redraw that
 * removed the Review button that opened the sheet: to the strip or the plugin list it was in, else
 * the composer or ⋯. Until then it waits; the redraws call it again.
 */
function landFocus() {
  const r = refocus;
  const v = r && plugins.value.find((p) => p.id === r.id)?.versions.find((x) => x.n === r.n);
  if (!r || v?.state === 'pending') return;
  refocus = null;
  const to = [document.getElementById(r.home), document.getElementById('body'), document.getElementById('more-open')]
    .find((el) => el instanceof HTMLElement && el.isConnected && el.offsetParent !== null);
  if (to instanceof HTMLElement) to.focus();
}
// room.js redraws the strip right after it hands the list over: a microtask runs after that.
addEventListener('grill:plugins', () => queueMicrotask(() => { if (refocus?.home === 'approvals') landFocus(); }));
// A sheet closed without a decision gives the focus back to what opened it.
reviewSheet.addEventListener('close', () => {
  reviewing.value = null;
  if (refocus) {
    landFocus();
    return;
  }
  requestAnimationFrame(() => {
    const to = [opener, document.getElementById(home), document.getElementById('body'), document.getElementById('more-open')]
      .find((el) => el instanceof HTMLElement && el.isConnected && el.offsetParent !== null);
    if (to instanceof HTMLElement && !to.contains(document.activeElement)) to.focus();
    opener = null;
  });
});
/** @param {number} id @param {number} n */
function review(id, n) {
  reviewing.value = { id, n };
  if (!reviewSheet.open) {
    opener = document.activeElement;
    home = opener?.closest('#approvals') ? 'approvals' : 'plugin-list';
    reviewSheet.showModal();
  }
}
addEventListener('grill:plugin-review', (e) => {
  const d = /** @type {CustomEvent<{id: number, n: number}>} */ (e).detail;
  review(d.id, d.n);
});

/** The components the approval sheet counts, in the order Claude Code lists them (009-AC14). */
const kinds = /** @type {const} */ ([['skills', 'Skills'], ['agents', 'Agents'], ['commands', 'Commands'], ['hooks', 'Hooks'],
  ['mcp_servers', 'MCP servers'], ['lsp_servers', 'LSP servers'], ['bin', 'Programs in bin/']]);

/**
 * Names lists at most 20 names, then how many more.
 * @param {{label: string, names?: string[]}} props
 */
function Names({ label, names }) {
  if (!names?.length) return null;
  const more = names.length - 20;
  return html`<li><span>${label} (${names.length}):</span> ${names.slice(0, 20).map((n, i) => html`${i ? ', ' : ''}<code>${n}</code>`)}${more > 0 ? `, and ${more} more` : ''}</li>`;
}

/**
 * ReviewSheet is the host's approval sheet (009-AC14, 009-AC15): what a version holds and runs,
 * Claude Code's check, its token cost and, for an update, its change against the running version.
 * The host opens it from the status strip or the plugin's row.
 * @param {{data: {room: {id: string}, me: {id: number}}, host: boolean}} props
 */
function ReviewSheet({ data, host }) {
  const r = reviewing.value;
  const [said, setSaid] = useState('');
  const [busy, setBusy] = useState(false);
  const p = r && plugins.value.find((x) => x.id === r.id);
  const v = p && p.versions.find((x) => x.n === r?.n);
  const close = () => reviewSheet.close();
  if (!r) return null;
  if (!p || !v) {
    return html`<div class="sheet-head"><h2 id="plugin-review-title">Plugin</h2>
      <button type="button" class="btn-ghost btn-icon" aria-label="Close" onClick=${close} dangerouslySetInnerHTML=${icon('close')}></button></div>
      <p>This plugin version is gone: someone removed it.</p>`;
  }
  const base = '/api/rooms/' + encodeURIComponent(data.room.id) + '/plugins';
  const m = v.manifest || /** @type {Manifest} */ ({ name: p.name });
  const run = running(p);
  /** @param {boolean} approve */
  const decide = (approve) => {
    if (busy) return;
    setBusy(true);
    fetchJSON('POST', `${base}/${p.id}/versions/${v.n}/decision`, { approve }).then(() => {
      // The Review button that opened the sheet goes with the decision: landFocus follows the redraw.
      refocus = { id: p.id, n: v.n, home };
      opener = null;
      close();
      toast((approve ? 'Approved ' : 'Refused ') + p.name + ' v' + v.n);
    }).catch((err) => setSaid(/** @type {Error} */ (err).message)).finally(() => setBusy(false));
  };
  const risky = [...(m.hooks || []).map((h) => ['Hook ' + h.event, h.command]),
    ...(m.mcp_servers || []).map((s) => ['MCP server ' + s.name, s.url || s.command || '']),
    ...(m.lsp_servers || []).map((s) => ['LSP server ' + s.name, s.command || ''])];
  const warned = warnings(v.report?.validate);
  const c = v.change;
  return html`<div class="sheet-head"><h2 id="plugin-review-title">Review ${p.name} v${v.n}</h2>
      <button type="button" class="btn-ghost btn-icon" aria-label="Close" onClick=${close} dangerouslySetInnerHTML=${icon('close')}></button></div>
    <p class="hint">Added by ${v.added_by}, ${window.grillAgo(v.added_at)}. ${sentence(stateWords(v))}</p>
    ${v.imported && html`<p class="warn" id="plugin-imported">Files came from an archive, not from the repo: nothing proves they match this commit. Update fetches the repo's.</p>`}
    <dl class="plugin-facts">
      <div><dt>Repository</dt><dd><code>${p.url}</code>${p.private ? ' (private)' : ''}</dd></div>
      <div><dt>Ref</dt><dd>${p.ref_name}</dd></div>
      <div><dt>Commit</dt><dd><${Commit} v=${v}/></dd></div>
      <div><dt>Size</dt><dd>${size(v.size)}, ${v.files} files</dd></div>
      <div><dt>Token cost per answer</dt><dd id="plugin-cost">${cost(v)}</dd></div>
    </dl>
    ${m.description && html`<p>${m.description}</p>`}
    <h3>Components</h3>
    <ul class="plugin-components" id="plugin-components">${kinds.map(([k, label]) => {
      const list = /** @type {(string | Hook | Server)[]} */ (m[k] || []);
      return html`<li key=${k}><span>${label} (${list.length})</span>${list.length ? ': ' : ''}${list.map((x, i) => html`${i ? ', ' : ''}${typeof x === 'string' ? x : 'event' in x ? x.event : x.name}`)}</li>`;
    })}</ul>
    <h3>What runs on the AI host's computer</h3>
    ${risky.length ? html`<ul class="plugin-risky" id="plugin-risky">${risky.map(([what, cmd]) => html`<li><span>${what}</span><code class="risk">${cmd}</code></li>`)}</ul>`
      : html`<p class="empty">No hook and no server: nothing runs by itself.</p>`}
    <p id="plugin-domains">${m.domains?.length ? html`Websites its MCP servers name: ${m.domains.map((d, i) => html`${i ? ', ' : ''}<code>${d}</code>${
      v.report?.allowlist_missing?.includes(d) ? html` <span class="badge bad off-allowlist">not on the AI host's allowlist</span>` : ''}`)}.`
      : 'Its MCP servers name no website.'}</p>
    ${v.report?.allowlist_missing?.some((d) => m.domains?.includes(d)) && html`<p class="hint">The sandbox reaches only the websites on the AI host's allowlist: the host adds these from their app's Rooms tab.</p>`}
    ${m.problems?.length ? html`<div class="warn" id="plugin-problems"><p>grill-room could not read all of it: what it missed may still run.</p>
      <ul>${m.problems.map((x) => html`<li>${x}</li>`)}</ul></div>` : ''}
    <h3>Claude Code's check</h3>
    ${v.state === 'inspecting' ? html`<p class="hint">${sentence(stateWords(v))}</p>`
      : v.state === 'invalid' ? html`<p class="error">${sentence(stateWords(v))}</p>`
      : warned.length ? html`<ul class="plugin-warnings" id="plugin-warnings">${warned.map((x) => html`<li>${x}</li>`)}</ul>`
      : html`<p class="empty">No warnings.</p>`}
    ${c && run && html`<h3>Changes since v${run.n}</h3>
      <ul class="plugin-change" id="plugin-change">
        <${Names} label="Components added" names=${c.components_added}/><${Names} label="Components removed" names=${c.components_removed}/>
        <${Names} label="Files added" names=${c.files_added}/><${Names} label="Files removed" names=${c.files_removed}/>
        <${Names} label="Files changed" names=${c.files_changed}/>
        ${!c.components_added && !c.components_removed && !c.files_added && !c.files_removed && !c.files_changed && html`<li>No file changed.</li>`}
      </ul>
      <p id="plugin-compare">${v.compare_url ? html`<a href=${v.compare_url} target="_blank" rel="noopener">Compare v${run.n} and v${v.n} on the repo's site</a>`
        : html`Commits: v${run.n} <${Commit} v=${run}/>, v${v.n} <${Commit} v=${v}/>.`}</p>`}
    <p class="said error" role="alert">${said}</p>
    ${host && v.state === 'pending' ? html`<div class="actions">
        <button type="button" class="btn-danger-outline" aria-disabled=${busy} onClick=${() => decide(false)}>Refuse</button>
        <button type="button" class="btn-primary" aria-disabled=${busy} onClick=${() => decide(true)}>Approve</button></div>`
      : v.state === 'pending' ? html`<p class="hint">The AI host decides.</p>` : ''}`;
}

/**
 * mountReview draws the approval sheet into its dialog, following the room and the agent state the
 * page hands over: only the host gets Approve and Refuse.
 * @param {import('@preact/signals').Signal<{room: {id: string}, me: {id: number}} | undefined>} room
 * @param {import('@preact/signals').Signal<{host_participant_id?: number} | undefined>} agent
 */
export function mountReview(room, agent) {
  const App = () => {
    const d = room.value;
    const r = reviewing.value;
    if (!d || !r) return null;
    return html`<${ReviewSheet} key=${r.id + '-' + r.n} data=${d} host=${agent.value?.host_participant_id === d.me.id}/>`;
  };
  render(html`<${App}/>`, reviewSheet);
}
