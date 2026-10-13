// Room starters (spec 006), shared by the profile page and the room's settings: the library list
// with its row menus, and the helpers both use. Preact, htm and signals, like /admin.
import { useState, useEffect, useRef } from 'preact/hooks';
import { html } from 'htm/preact';
import { size, ago } from './room-panel.js';

/**
 * @typedef {{id: number, builtin?: string, library: string, mine: boolean, title: string, description: string,
 *   author: string, version: number, versions: number, size: number, updated_at: string}} Starter
 * @typedef {{label: string, key: string, required?: boolean, hint?: string}} Field
 * @typedef {{name: string, badge: string, colour: string, text: string, model: string, effort: string,
 *   every_n?: number, every_voice?: number, every_minutes?: number, self_call?: boolean, autocompact?: number,
 *   skills?: string[]}} StarterPersona
 * @typedef {{name: string, version: number, author: string, description: string, size: number, file: string}} StarterSkill
 * @typedef {{name: string, kind: string, format: string, content_type?: string, text?: string, file?: string, size?: number}} StarterDocument
 * @typedef {{title: string, description: string, room_name: string, agenda: string, fields: Field[],
 *   personas: StarterPersona[], skills: StarterSkill[], documents: StarterDocument[],
 *   chain: {budget: number, suspended?: boolean, parallel_turns?: boolean}, glossary?: string}} Manifest
 */

/** The picker's groups, in the words of docs/design.md §8 (006-AC8). */
export const libraries = /** @type {const} */ ([['builtin', 'Built-in'], ['personal', 'Yours'], ['instance', 'Shared']]);

/**
 * RowMenu is a row's ⋯ (docs/design.md §5, 008-AC13): a popover under its button, its first item
 * focused, ↑ ↓ moving, Escape and a click outside closing it, focus back on ⋯.
 * @param {{id: string, label: string, items: [string, () => void][]}} props
 */
export function RowMenu({ id, label, items }) {
  const btn = useRef(/** @type {HTMLButtonElement | null} */ (null));
  const menu = useRef(/** @type {HTMLDivElement | null} */ (null));
  useEffect(() => {
    const m = menu.current;
    if (!m) return;
    /** @param {Event} e */
    const place = (e) => {
      if (/** @type {ToggleEvent} */ (e).newState !== 'open' || !btn.current) return;
      const r = btn.current.getBoundingClientRect();
      m.style.top = Math.max(8, Math.min(r.bottom + 4, innerHeight - 160)) + 'px';
      m.style.right = Math.max(8, innerWidth - r.right) + 'px';
    };
    /** @param {Event} e */
    const focus = (e) => { if (/** @type {ToggleEvent} */ (e).newState === 'open') /** @type {HTMLElement | null} */ (m.querySelector('button'))?.focus(); };
    /** @param {KeyboardEvent} e */
    const keys = (e) => {
      // Tab leaves the menu as Escape does: it closes, and the focus goes back to ⋯.
      if (e.key === 'Tab') {
        e.preventDefault();
        m.hidePopover();
        btn.current?.focus();
        return;
      }
      const step = /** @type {Record<string, number>} */ ({ ArrowDown: 1, ArrowUp: -1 })[e.key];
      if (!step) return;
      e.preventDefault();
      const all = [...m.querySelectorAll('button')];
      all[(all.indexOf(/** @type {HTMLButtonElement} */ (document.activeElement)) + step + all.length) % all.length]?.focus();
    };
    m.addEventListener('beforetoggle', place);
    m.addEventListener('toggle', focus);
    m.addEventListener('keydown', keys);
    return () => { m.removeEventListener('beforetoggle', place); m.removeEventListener('toggle', focus); m.removeEventListener('keydown', keys); };
  }, []);
  if (!items.length) return null;
  return html`<button ref=${btn} type="button" class="btn-ghost btn-icon" popovertarget=${id} aria-label=${label}>⋯</button>
    <div ref=${menu} id=${id} popover="auto" class="menu" role="menu">${items.map(([name, run]) => html`<button key=${name} type="button" role="menuitem"
      onClick=${() => { menu.current?.hidePopover(); btn.current?.focus(); run(); }}>${name}</button>`)}</div>`;
}

/**
 * meta is a starter's line under its title: version, author, size and when it last changed.
 * @param {Starter} st
 */
export const meta = (st) => html`v${st.version} · by ${st.author}${st.size ? ' · ' + size(st.size) : ''} · ${ago(st.updated_at)}`;

/**
 * StarterList is the profile's view of the library (006-AC1 to 006-AC3): built-ins, its own and
 * the shared ones. Any starter duplicates into "Yours"; its own it shares with everyone or deletes.
 * Its own it also edits, which saves the next version (006-AC3).
 * @param {{starters: Starter[], api: import('./room-panel.js').Api, reload: () => void, say: (text: string) => void,
 *   edit?: (st: Starter) => void}} props
 */
export function StarterList({ starters, api, reload, say, edit }) {
  /** @param {unknown} err */
  const show = (err) => say(/** @type {Error} */ (err).message);
  /** @param {Starter} st */
  const actions = (st) => {
    const base = '/api/profile/starters/' + st.id;
    /** @type {[string, () => void][]} */
    const out = [['Duplicate to yours', () => api('POST', base + '/duplicate').then(() => { say('Duplicated "' + st.title + '".'); reload(); }).catch(show)]];
    if (st.mine && edit) out.push(['Edit', () => edit(st)]);
    if (st.mine && st.library === 'personal') {
      out.push(['Share with everyone', () => window.grillConfirm('Share "' + st.title + '" with everyone?', 'Every profile on this server will see it and create rooms from it. You stay its author.', 'Share')
        .then((yes) => { if (yes) api('POST', base + '/publish').then(() => { say('Shared "' + st.title + '".'); reload(); }).catch(show); })]);
    }
    if (st.mine) {
      out.push(['Delete', () => window.grillConfirm('Delete "' + st.title + '"?', 'Rooms created from it keep their copy.', 'Delete')
        .then((yes) => { if (yes) api('DELETE', base).then(() => { say('Deleted "' + st.title + '".'); reload(); }).catch(show); })]);
    }
    return out;
  };
  return html`${libraries.map(([lib, label]) => {
    const list = starters.filter((st) => st.library === lib);
    return html`<h3 key=${lib}>${label}</h3>${list.length
      ? html`<ul class="starter-list" id=${'starters-' + lib}>${list.map((st) => html`<li key=${st.id} data-starter=${st.id}>
          <div class="starter-text"><strong>${st.title}</strong><span class="hint">${meta(st)}</span>
            ${st.description && html`<span class="hint">${st.description}</span>`}</div>
          <${RowMenu} id=${'starter-menu-' + st.id} label=${'More actions for ' + st.title} items=${actions(st)}/>
        </li>`)}</ul>`
      : html`<p class="hint">${lib === 'personal' ? 'None yet. Duplicate one, or save a room as a starter from its settings.' : 'None yet.'}</p>`}`;
  })}`;
}

/**
 * @typedef {Starter & {manifest: Manifest, models: string[], efforts: string[]}} StarterView
 * @typedef {{kind: string, n: number}} Trigger
 */

/** The "Speaks up" choices, in the words of docs/design.md §8 (006-AC18). */
export const triggers = /** @type {const} */ ([['', 'Only when asked'], ['every_n', 'Every N answers'], ['every_voice', 'Every N voice lines'],
  ['every_minutes', 'Every N minutes']]);

/** @param {StarterPersona} p @returns {Trigger} */
export const triggerOf = (p) => (p.every_n ? { kind: 'every_n', n: p.every_n } : p.every_voice ? { kind: 'every_voice', n: p.every_voice }
  : p.every_minutes ? { kind: 'every_minutes', n: p.every_minutes } : { kind: '', n: 0 });

/** @param {StarterPersona} p @returns {string} */
export function triggerText(p) {
  const t = triggerOf(p);
  if (!t.kind) return 'only when asked';
  const unit = { every_n: 'answers', every_voice: 'voice lines', every_minutes: 'minutes' }[t.kind];
  return 'every ' + t.n + ' ' + unit;
}

/**
 * search tells a starter matching q in its title, description or author (006-AC8).
 * @param {Starter} st @param {string} q
 */
const matches = (st, q) => !q || [st.title, st.description, st.author].some((v) => (v || '').toLowerCase().includes(q));

/**
 * Preview shows what a starter version puts in a room before it is picked (006-AC8).
 * @param {{view: StarterView}} props
 */
function Preview({ view }) {
  const m = view.manifest;
  return html`<section id="starter-preview" aria-label=${'Preview of ' + view.title}>
    <h3>${view.title}</h3>
    <p class="hint">${view.library === 'blank' ? 'A room as it always started' : meta(view)}</p>
    ${m.description && html`<p>${m.description}</p>`}
    <h4>Agenda</h4>${m.agenda ? html`<pre class="starter-agenda">${m.agenda}</pre>` : html`<p class="hint">None.</p>`}
    <h4>AI personas</h4>
    <ul class="starter-personas">${m.personas.map((p) => html`<li key=${p.name}>${p.badge} ${p.name} · ${p.model} · effort ${p.effort} · ${triggerText(p)}</li>`)}</ul>
    <h4>Documents</h4>${m.documents.length ? html`<ul>${m.documents.map((d) => html`<li key=${d.name}>${d.name}</li>`)}</ul>` : html`<p class="hint">None.</p>`}
    <h4>Skills</h4>${m.skills.length ? html`<ul>${m.skills.map((k) => html`<li key=${k.name}>${k.name} v${k.version}</li>`)}</ul>` : html`<p class="hint">None.</p>`}
  </section>`;
}

/**
 * Picker lists Blank and the starters by library, with a search, and previews the one selected
 * (006-AC8). Picking any of them, Blank included, opens the editor (006-AC9).
 * @param {{api: import('./room-panel.js').Api, pick: (view: StarterView) => void, close: () => void}} props
 */
export function Picker({ api, pick, close }) {
  const [list, setList] = useState(/** @type {Starter[]} */ ([]));
  const [q, setQ] = useState('');
  const [chosen, setChosen] = useState(/** @type {StarterView | null} */ (null));
  const [blankText, setBlankText] = useState('');
  const [said, setSaid] = useState('');
  /** @param {unknown} err */
  const show = (err) => setSaid(/** @type {Error} */ (err).message);
  useEffect(() => { api('GET', '/api/profile/starters').then(setList).catch(show); }, []);
  // Only the latest pick shows: an earlier answer arriving late, the first Blank's included, is dropped.
  const asked = useRef(0);
  /** @param {number} id */
  const preview = (id) => {
    const n = ++asked.current;
    return api('GET', '/api/profile/starters/' + id).then((/** @type {StarterView} */ v) => {
      if (!id) setBlankText(v.description);
      if (n === asked.current) setChosen(v);
    }).catch((err) => { if (n === asked.current) show(err); });
  };
  useEffect(() => { preview(0); }, []);
  const needle = q.trim().toLowerCase();
  /** @param {{id: number, title: string, description: string, author?: string, version?: number}} st @param {string} label */
  const entry = (st, label) => html`<li key=${st.id}><button type="button" class="starter-pick" aria-pressed=${chosen?.id === st.id || (!st.id && chosen?.library === 'blank')}
    onClick=${() => preview(st.id)} aria-label=${label + ': ' + st.title}><strong>${st.title}</strong><span class="hint">${st.description}</span></button></li>`;
  const blank = { id: 0, title: 'Blank', description: blankText };
  return html`<div class="starter-picker">
    <div class="starter-choices">
      <input type="search" id="starter-search" aria-label="Search starters" placeholder="Search by title, description or author" value=${q}
        onInput=${(/** @type {Event} */ e) => setQ(/** @type {HTMLInputElement} */ (e.currentTarget).value)}/>
      ${(!needle || matches(/** @type {Starter} */ (/** @type {unknown} */ (blank)), needle)) && html`<ul class="starter-group" id="pick-blank">${entry(blank, 'Blank')}</ul>`}
      ${libraries.map(([lib, label]) => {
        const shown = list.filter((st) => st.library === lib && matches(st, needle));
        return shown.length > 0 && html`<h3 key=${lib}>${label}</h3><ul class="starter-group" id=${'pick-' + lib}>${shown.map((st) => entry(st, label))}</ul>`;
      })}
    </div>
    <div class="starter-detail">${chosen && html`<${Preview} view=${chosen}/>`}</div>
    <p class="said error" role="status">${said}</p>
    <div class="actions">
      <button type="button" class="btn-secondary" onClick=${close}>Cancel</button>
      <button type="button" class="btn-primary" id="starter-use" disabled=${!chosen} onClick=${() => chosen && pick(chosen)}>Use this starter</button>
    </div>
  </div>`;
}

/**
 * PersonaEdit edits one persona of the editor, for the new room only (006-AC9).
 * @param {{p: StarterPersona, models: string[], efforts: string[], set: (p: StarterPersona) => void, remove: () => void}} props
 */
function PersonaEdit({ p, models, efforts, set, remove }) {
  const t = triggerOf(p);
  /** @param {Partial<StarterPersona>} patch */
  const change = (patch) => set({ ...p, ...patch });
  /** @param {string} kind @param {number} n */
  const setTrigger = (kind, n) => change({ every_n: kind === 'every_n' ? n : 0, every_voice: kind === 'every_voice' ? n : 0, every_minutes: kind === 'every_minutes' ? n : 0 });
  /** @param {Event} e */
  const val = (e) => /** @type {HTMLInputElement} */ (e.currentTarget).value;
  return html`<fieldset class="starter-persona" style=${'--persona:' + p.colour}>
    <legend>${p.badge} ${p.name}</legend>
    <div class="persona-fields">
      <label>Name<input required maxlength="30" value=${p.name} onInput=${(/** @type {Event} */ e) => change({ name: val(e) })}/></label>
      <label>Badge<input required maxlength="8" value=${p.badge} onInput=${(/** @type {Event} */ e) => change({ badge: val(e) })}/></label>
      <label>Model<select value=${p.model} onChange=${(/** @type {Event} */ e) => change({ model: val(e) })}>${models.map((m) => html`<option key=${m}>${m}</option>`)}</select></label>
      <label>Effort<select value=${p.effort} onChange=${(/** @type {Event} */ e) => change({ effort: val(e) })}>${efforts.map((m) => html`<option key=${m}>${m}</option>`)}</select></label>
      <label>Speaks up<select value=${t.kind} onChange=${(/** @type {Event} */ e) => setTrigger(val(e), t.n || 1)}>
        ${triggers.map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}</select></label>
      ${t.kind && html`<label>N<input type="number" min="1" max=${t.kind === 'every_n' ? 100 : t.kind === 'every_voice' ? 500 : 240} value=${t.n}
        onInput=${(/** @type {Event} */ e) => { const n = Number(val(e)); if (Number.isInteger(n) && n > 0) setTrigger(t.kind, n); }}/></label>`}
    </div>
    <details><summary>Persona text</summary>
      <textarea rows="6" maxlength="8000" aria-label=${'Persona text of ' + p.name} value=${p.text}
        onInput=${(/** @type {Event} */ e) => change({ text: /** @type {HTMLTextAreaElement} */ (e.currentTarget).value })}></textarea></details>
    <div class="actions"><button type="button" class="btn-danger-outline" onClick=${remove} aria-label=${'Leave ' + p.name + ' out'}>Leave out</button></div>
  </fieldset>`;
}

/**
 * keyOf derives a field's key from its label (006-AC16): lowercase letters, digits and _.
 * @param {string} label
 */
export const keyOf = (label) => label.toLowerCase().normalize('NFD').replace(/\p{M}/gu, '').replace(/[^a-z0-9]+/g, '_')
  .replace(/^_+|_+$/g, '').slice(0, 30);

/** @param {string} text @returns {string[]} the keys {{key}} names in text */
const usedKeys = (text) => [...text.matchAll(/\{\{([a-z0-9_]+)\}\}/g)].map((x) => x[1]);

/**
 * freeKey is the key a field labelled label gets among the others' keys: keyOf, else field_N for a
 * label with no Latin letter or digit, with _2, _3… until it is unused.
 * @param {string} label @param {string[]} taken
 */
const freeKey = (label, taken) => {
  const base = keyOf(label) || 'field_' + (taken.length + 1);
  let key = base;
  for (let n = 2; taken.includes(key); n++) key = base.slice(0, 27) + '_' + n;
  return key;
};

/** The most fields a starter asks, as the server takes (006-AC16). */
const maxFields = 10;

/**
 * FieldsEdit declares a starter's fields (006-AC16): a label, its key derived from it, required or
 * not, an optional hint. A key follows its label until the agenda or the room name uses it:
 * renaming such a field keeps its {{key}}.
 * @param {{fields: Field[], set: (f: Field[]) => void, used: string[]}} props
 */
function FieldsEdit({ fields, set, used }) {
  /** @param {number} i @param {Partial<Field>} patch */
  const change = (i, patch) => set(fields.map((f, j) => (j === i ? { ...f, ...patch } : f)));
  /** @param {number} i @param {string} label */
  const relabel = (i, label) => {
    const f = fields[i];
    const others = fields.filter((_, j) => j !== i).map((x) => x.key);
    const follows = !f.key || (f.key === freeKey(f.label, others) && !used.includes(f.key));
    change(i, follows ? { label, key: freeKey(label, others) } : { label });
  };
  return html`<fieldset id="starter-fields-edit"><legend>Fields</legend>
    <p class="hint">Asked when a room is created from the starter. Their values replace {{key}} in the agenda and the room name.</p>
    ${fields.map((f, i) => html`<div class="starter-field" key=${i}>
      <label>Label<input required maxlength="40" value=${f.label}
        onInput=${(/** @type {Event} */ e) => relabel(i, /** @type {HTMLInputElement} */ (e.currentTarget).value)}/></label>
      <span class="hint">Key: <code>{{${f.key}}}</code></span>
      <label class="check"><input type="checkbox" checked=${!!f.required}
        onChange=${(/** @type {Event} */ e) => change(i, { required: /** @type {HTMLInputElement} */ (e.currentTarget).checked })}/> Required</label>
      <label>Hint<input maxlength="100" value=${f.hint || ''} onInput=${(/** @type {Event} */ e) => change(i, { hint: /** @type {HTMLInputElement} */ (e.currentTarget).value })}/></label>
      <button type="button" class="btn-danger-outline" aria-label=${'Remove the field ' + f.label} onClick=${() => set(fields.filter((_, j) => j !== i))}>Remove</button>
    </div>`)}
    ${fields.length < maxFields ? html`<button type="button" class="btn-secondary" onClick=${() => set([...fields, { label: '', key: '', required: false }])}>+ Add a field</button>`
      : html`<p class="hint">A starter asks at most ${maxFields} fields.</p>`}
  </fieldset>`;
}

/**
 * Editor opens on a starter version, Blank included, or on a room, prefilled (006-AC9, 006-AC15).
 * mode "create" makes a room: the starter's fields first, required ones starred; what it changes
 * goes to the new room only (006-AC10). mode "save" saves a room as a new starter, its documents
 * unchecked; "edit" saves the next version of one of the profile's starters. Both declare fields,
 * refuse an undeclared {{x}} and warn of a field used nowhere (006-AC16).
 * @param {{view: StarterView, api: import('./room-panel.js').Api, displayName?: string, back: () => void,
 *   mode?: 'create' | 'save' | 'edit', path?: string, done?: (st: Starter) => void}} props
 */
export function Editor({ view, api, displayName = '', back, mode = 'create', path = '', done }) {
  const m = view.manifest;
  const creating = mode === 'create';
  const [values, setValues] = useState(/** @type {Record<string, string>} */ ({}));
  const [title, setTitle] = useState(m.title || '');
  const [description, setDescription] = useState(m.description || '');
  const [fields, setFields] = useState(m.fields || []);
  const [name, setName] = useState(m.room_name || '');
  const [as, setAs] = useState(displayName);
  const [agenda, setAgenda] = useState(m.agenda || '');
  const [personas, setPersonas] = useState(m.personas);
  const [docs, setDocs] = useState(mode === 'save' ? [] : m.documents.map((d) => d.name));
  const [skills, setSkills] = useState(m.skills.map((k) => k.name));
  const [said, setSaid] = useState('');
  const [busy, setBusy] = useState(false);
  const area = useRef(/** @type {HTMLTextAreaElement | null} */ (null));
  const missing = creating ? (m.fields || []).filter((f) => f.required && !(values[f.key] || '').trim()).map((f) => f.label) : [];
  const keys = fields.map((f) => f.key);
  const used = [...usedKeys(agenda), ...usedKeys(name)];
  const undeclared = creating ? [] : [...new Set(used.filter((k) => !keys.includes(k)))];
  const unused = creating ? [] : fields.filter((f) => f.key && !used.includes(f.key)).map((f) => f.label);
  const unlabelled = !creating && fields.some((f) => !f.label.trim());
  const blocked = missing.length > 0 || undeclared.length > 0 || unlabelled || busy;
  /** @param {string[]} list @param {string} item @param {boolean} on */
  const toggle = (list, item, on) => (on ? [...list, item] : list.filter((x) => x !== item));
  /** Insert field puts {{key}} at the agenda's cursor (006-AC16). @param {string} key */
  const insert = (key) => {
    const ta = area.current;
    const at = ta ? ta.selectionStart : agenda.length;
    const end = ta ? ta.selectionEnd : at;
    const token = '{{' + key + '}}';
    setAgenda(agenda.slice(0, at) + token + agenda.slice(end));
    requestAnimationFrame(() => { if (ta) { ta.focus(); ta.setSelectionRange(at + token.length, at + token.length); } });
  };
  const submit = async () => {
    setBusy(true);
    try {
      if (creating) {
        const room = await api('POST', '/api/profile/rooms', {
          name, display_name: as, fields: values, agenda, personas, documents: docs, skills,
          starter: view.library === 'blank' ? undefined : { id: view.id, version: view.version },
        });
        // The new room opens on its invite card (005-AC34).
        location.assign('/r/' + encodeURIComponent(room.id) + '?invite');
        return;
      }
      const st = await api('POST', path, { version: view.version, title, description, room_name: name, agenda, fields, personas, documents: docs, skills });
      setBusy(false);
      done?.(st);
    } catch (err) {
      setSaid(/** @type {Error} */ (err).message);
      setBusy(false);
    }
  };
  /** @param {Event} e */
  const val = (e) => /** @type {HTMLInputElement} */ (e.currentTarget).value;
  return html`<form class="starter-editor stack" onSubmit=${(/** @type {Event} */ e) => { e.preventDefault(); if (!blocked) submit(); }}>
    <p class="hint">${creating ? html`From ${view.title}${view.library === 'blank' ? '' : ' v' + view.version}. Your changes go to this room only.`
      : mode === 'save' ? 'Check what goes in. It lands in your starters, which only you see until you share it.'
      : `You edit version ${view.version}. Saving makes version ${view.versions + 1}; rooms keep theirs.`}</p>
    ${!creating && html`<label>Title<input id="starter-title" required maxlength="60" value=${title} onInput=${(/** @type {Event} */ e) => setTitle(val(e))}/></label>
      <label>Description<textarea id="starter-description" rows="2" maxlength="300" value=${description}
        onInput=${(/** @type {Event} */ e) => setDescription(/** @type {HTMLTextAreaElement} */ (e.currentTarget).value)}></textarea></label>
      <${FieldsEdit} fields=${fields} set=${setFields} used=${used}/>`}
    ${creating && (m.fields || []).length > 0 && html`<fieldset id="starter-fields"><legend>About this room</legend>${m.fields.map((f) => html`<label key=${f.key}>
      ${f.label}${f.required && html`<span aria-hidden="true"> *</span>`}
      <input required=${!!f.required} maxlength="100" value=${values[f.key] || ''} aria-describedby=${f.hint ? 'hint-' + f.key : undefined}
        onInput=${(/** @type {Event} */ e) => setValues({ ...values, [f.key]: val(e) })}/>
      ${f.hint && html`<span class="hint" id=${'hint-' + f.key}>${f.hint}</span>`}</label>`)}</fieldset>`}
    <label>${creating ? 'Room name' : 'Default room name'}<input id="starter-room-name" required maxlength="200" value=${name} onInput=${(/** @type {Event} */ e) => setName(val(e))}/></label>
    ${creating && html`<label>Your name in the room<input id="starter-as" required maxlength="40" autocomplete="nickname" value=${as} onInput=${(/** @type {Event} */ e) => setAs(val(e))}/></label>`}
    <label>Agenda (agenda.md)<textarea id="starter-agenda" ref=${area} rows="8" value=${agenda}
      onInput=${(/** @type {Event} */ e) => setAgenda(/** @type {HTMLTextAreaElement} */ (e.currentTarget).value)}></textarea></label>
    <span class="hint">Every AI persona aligns on it. Leave it empty for no agenda.</span>
    ${!creating && fields.some((f) => f.key) && html`<div class="row" id="starter-insert">${fields.filter((f) => f.key).map((f) => html`<button type="button" key=${f.key}
      class="btn-secondary" onClick=${() => insert(f.key)}>Insert ${f.label}</button>`)}</div>`}
    <h3>AI personas</h3>
    ${personas.length ? personas.map((p, i) => html`<${PersonaEdit} key=${i} p=${p} models=${view.models} efforts=${view.efforts}
      set=${(/** @type {StarterPersona} */ np) => setPersonas(personas.map((x, j) => (j === i ? np : x)))}
      remove=${() => setPersonas(personas.filter((_, j) => j !== i))}/>`) : html`<p class="hint">None: add them in the room.</p>`}
    ${m.documents.length > 0 && html`<fieldset id="starter-docs"><legend>Documents</legend>
      ${mode === 'save' && html`<p class="hint">Each one you check goes in at its current revision.</p>`}
      ${m.documents.map((d) => html`<label class="check" key=${d.name}>
      <input type="checkbox" checked=${docs.includes(d.name)} onChange=${(/** @type {Event} */ e) => setDocs(toggle(docs, d.name, /** @type {HTMLInputElement} */ (e.currentTarget).checked))}/> ${d.name}</label>`)}</fieldset>`}
    ${m.skills.length > 0 && html`<fieldset id="starter-skills"><legend>Skills</legend>${m.skills.map((k) => html`<label class="check" key=${k.name}>
      <input type="checkbox" checked=${skills.includes(k.name)} onChange=${(/** @type {Event} */ e) => setSkills(toggle(skills, k.name, /** @type {HTMLInputElement} */ (e.currentTarget).checked))}/> ${k.name} v${k.version}</label>`)}
      ${creating && view.library !== 'builtin' && html`<span class="hint">They wait for the AI host's approval when it approves everything.</span>`}</fieldset>`}
    <p class="hint" id="starter-missing">${missing.length ? 'Fill in ' + missing.join(', ') + ' to create the room.' : ''}</p>
    ${undeclared.length > 0 && html`<p class="said error" id="starter-undeclared" role="alert">${undeclared.map((k) => '{{' + k + '}}').join(', ')} ${undeclared.length > 1 ? 'are' : 'is'} no field: add it to the fields, or remove it.</p>`}
    ${unlabelled && html`<p class="said error" id="starter-unlabelled" role="alert">Every field needs a label: name it, or remove it.</p>`}
    ${unused.length > 0 && html`<p class="hint warn" id="starter-unused">Not used in the agenda or the room name: ${unused.join(', ')}.</p>`}
    <p class="said error" id="room-create-status" role="status">${said}</p>
    <div class="actions">
      <button type="button" class="btn-secondary" onClick=${back}>${creating ? 'Back' : 'Cancel'}</button>
      <button type="submit" class="btn-primary" id="starter-create" disabled=${blocked}
        aria-describedby=${undeclared.length ? 'starter-undeclared' : unlabelled ? 'starter-unlabelled' : missing.length ? 'starter-missing' : undefined}>${creating ? 'Create room' : mode === 'save' ? 'Save as starter' : 'Save version ' + (view.versions + 1)}</button>
    </div>
  </form>`;
}

/**
 * NewRoom is "New room": the picker, then the editor, in one sheet (006-AC8, 006-AC9).
 * @param {{api: import('./room-panel.js').Api, displayName: string, close: () => void}} props
 */
export function NewRoom({ api, displayName, close }) {
  const [view, setView] = useState(/** @type {StarterView | null} */ (null));
  return html`<div class="sheet-head"><h2 id="room-create-title">${view ? 'New room' : 'New room: pick a starter'}</h2>
      <button type="button" class="btn-ghost btn-icon" aria-label="Close" onClick=${close}>✕</button></div>
    ${view ? html`<${Editor} key=${view.id} view=${view} api=${api} displayName=${displayName} back=${() => setView(null)}/>`
      : html`<${Picker} api=${api} pick=${setView} close=${close}/>`}`;
}
