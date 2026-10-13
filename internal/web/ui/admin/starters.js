// Room starters (spec 006), shared by the profile page and the room's settings: the library list
// with its row menus, and the helpers both use. Preact, htm and signals, like /admin.
import { useEffect, useRef } from 'preact/hooks';
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
 * @param {{starters: Starter[], api: import('./room-panel.js').Api, reload: () => void, say: (text: string) => void}} props
 */
export function StarterList({ starters, api, reload, say }) {
  /** @param {unknown} err */
  const show = (err) => say(/** @type {Error} */ (err).message);
  /** @param {Starter} st */
  const actions = (st) => {
    const base = '/api/profile/starters/' + st.id;
    /** @type {[string, () => void][]} */
    const out = [['Duplicate to yours', () => api('POST', base + '/duplicate').then(() => { say('Duplicated "' + st.title + '".'); reload(); }).catch(show)]];
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
