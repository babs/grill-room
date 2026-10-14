// The room's Manage sheet (004-AC8, 004-AC19, 005-AC15): Preact, htm and signals, like /admin.
// room.js, vanilla, hands over the room, the agent state and the transcription state it loads as
// window.grillRoom, window.grillAgent and window.grillSTT, each followed by an event. Each tab is a
// deep link, #manage/<tab>.
import { render } from 'preact';
import { useState, useEffect } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';
import { RoomPanel, Rename, TabBar, fetchJSON, roomTabs, icon } from './room-panel.js';
import { Editor } from './starters.js';
import { PluginsTab, mountReview } from './plugins.js';

/**
 * @typedef {{id: number, name: string, admin: boolean}} Participant
 * @typedef {{room: {id: string, name: string}, me: Participant, participants: Participant[]}} RoomData
 * @typedef {{id: number, name: string, version: number, author: string, pending?: boolean}} RoomSkill
 * @typedef {{id: number, name: string, badge: string, model: string, effort: string, pending?: boolean}} Persona
 * @typedef {{room_skills?: RoomSkill[], personas?: Persona[], host_participant_id?: number}} AgentState
 * @typedef {{glossary: string, prompt: string, used: number, total: number, capacity: number}} Glossary
 * @typedef {{glossary: boolean}} STTState
 */

const w = /** @type {Window & {grillRoom?: RoomData, grillAgent?: AgentState, grillSTT?: STTState}} */ (window);
const roomData = signal(w.grillRoom);
const agentState = signal(w.grillAgent);
// The glossary tab shows only while a transcription worker reads it (007-AC14); its data stays.
const sttGlossary = signal(!!w.grillSTT?.glossary);
addEventListener('grill:room', (e) => { roomData.value = /** @type {CustomEvent<RoomData>} */ (e).detail; });
addEventListener('grill:agent', (e) => { agentState.value = /** @type {CustomEvent<AgentState>} */ (e).detail; });
addEventListener('grill:stt', (e) => { sttGlossary.value = /** @type {CustomEvent<STTState>} */ (e).detail.glossary; });

const allTabs = [...Object.entries(roomTabs).slice(0, 3), ['personas', 'Personas'], ['skills', 'Skills'], ['plugins', 'Plugins'],
  ['documents', 'Documents'], ['export', roomTabs.export], ['glossary', 'Words to recognise']];
// The tabs a plain member gets; a room admin gets them all. Every member reads the room's plugins
// (009-AC5).
const memberTabs = ['plugins', 'glossary'];

// tab is the open tab from #manage/<tab>, '' when the sheet is closed.
/** @param {string} hash */
const tabOf = (hash) => (hash === '#manage' ? 'general' : hash.startsWith('#manage/') ? hash.slice(8) : '');
const tab = signal(tabOf(location.hash));
const dialog = /** @type {HTMLDialogElement} */ (document.getElementById('manage'));
const follow = () => {
  tab.value = tabOf(location.hash);
  if (tab.value && !dialog.open) dialog.showModal();
  else if (!tab.value && dialog.open) dialog.close();
};
addEventListener('hashchange', follow);
follow();
document.getElementById('manage-open')?.addEventListener('click', () => { location.hash = '#manage'; });
document.getElementById('manage-close')?.addEventListener('click', () => dialog.close());
// Closed by Escape, a click outside or Close: the deep link goes too, without a history entry.
dialog.addEventListener('close', () => {
  if (tabOf(location.hash)) history.replaceState(null, '', location.pathname + location.search);
  tab.value = '';
});

/** @param {{data: RoomData, tab: string, tabs: string[][]}} props */
function Manage({ data, tab, tabs }) {
  const base = '/api/rooms/' + encodeURIComponent(data.room.id);
  const [room, setRoom] = useState(/** @type {import('./room-panel.js').Room | null} */ (null));
  const [said, setSaid] = useState('');
  /** @param {unknown} err */
  const show = (err) => setSaid(/** @type {Error} */ (err).message);
  const load = () => fetchJSON('GET', base + '/overview').then(setRoom).catch(show);
  // The room answer changes with every system line: a rename, an appointment, a join.
  useEffect(() => { load(); }, [data]);
  /** @param {number} id */
  const byName = (id) => data.participants.find((p) => p.id === id)?.name || 'a former participant';
  /** @param {Participant} p @param {boolean} on */
  const setAdmin = (p, on) => fetchJSON(on ? 'PUT' : 'DELETE', `${base}/admins/${p.id}`).then(() => setSaid('')).catch(show);
  /** @param {RoomSkill} k */
  const removeSkill = (k) => {
    window.grillConfirm(`Remove the room skill ${k.name}?`, 'Personas that have it on lose it.', 'Remove')
      .then((yes) => { if (yes) fetchJSON('DELETE', `${base}/skills/${k.id}`).catch(show); });
  };
  const skills = agentState.value?.room_skills || [];
  const personas = agentState.value?.personas || [];
  const openPersonas = () => {
    dialog.close();
    document.getElementById('personas-open')?.click();
  };
  return html`
    <p class="hint">You are a room admin. The instance's audit log records every action here.</p>
    <p class="said" role="alert">${said}</p>
    ${room && html`<${RoomPanel} key=${room.id} room=${room} base=${base} api=${fetchJSON} show=${show} reload=${load} byName=${byName}
      tab=${tab} tabs=${tabs} href=${(/** @type {string} */ id) => '#manage/' + id} close=${() => location.assign('/profile')} label="Room settings"
      general=${html`<h3>Save as starter</h3>
        <p class="hint">Keep this room's agenda, AI personas and skills, and the documents and plugins you pick, to start new rooms the same way.</p>
        <div class="row"><button type="button" class="btn-secondary" id="save-starter" onClick=${() => saveAsStarter(base, setSaid)}>Save as starter</button></div>`}>
      ${tab === 'admins' && html`
        <p class="hint">Room admins rename the room and its participants, manage every invite, appoint other admins and delete the room. The room keeps at least one.</p>
        <ul id="room-admins" class="admin-rows">${data.participants.map((p) => html`<li key=${p.id}>
          <span class="who">${p.name}</span><span>${p.admin && html`<strong class="badge role">admin</strong>`}</span>
          <span>${p.admin
            ? html`<button type="button" class="btn-secondary" onClick=${() => setAdmin(p, false)}>${p.id === data.me.id ? 'Step down' : 'Remove admin'}</button>`
            : html`<button type="button" class="btn-secondary" onClick=${() => setAdmin(p, true)} aria-label=${'Make ' + p.name + ' a room admin'}>Make admin</button>`}</span>
          <${Rename} key=${p.name} name=${p.name} path=${`${base}/participants/${p.id}/name`} api=${fetchJSON} show=${show} done=${() => setSaid('')}/>
        </li>`)}</ul>`}
      ${tab === 'personas' && html`
        <p class="hint">Every member adds and edits the room's personas in the Personas sheet. A persona's name opens its own sheet.</p>
        <ul id="manage-personas">${personas.map((p) => html`<li key=${p.id}><span><span class="badge persona-badge">${p.badge}</span> <a href=${'#personas/' + p.id + '/identity'}
          aria-label=${p.badge + ' ' + p.name}>${p.name}</a> <span class="hint">${p.model} · effort ${p.effort}${p.pending ? ' · waits for the AI host' : ''}</span></span></li>`)}</ul>
        <button type="button" class="btn-primary" onClick=${openPersonas}>Open the Personas sheet</button>`}
      ${tab === 'skills' && (skills.length ? html`<ul id="room-admin-skills">${skills.map((k) => html`<li key=${k.id}>${k.name} v${k.version}, copied by ${k.author}
          ${' '}<button type="button" class="btn-secondary" onClick=${() => removeSkill(k)} aria-label=${'Remove the room skill ' + k.name}>Remove</button></li>`)}</ul>`
        : html`<p class="empty">None yet. Members copy their personal skills in from the Personas sheet.</p>`)}
      ${tab === 'plugins' && html`<${PluginsTab} base=${base} admin=${true} host=${isHost(data)} personas=${personas}/>`}
      ${tab === 'documents' && html`<${DeletedDocuments} base=${base} show=${show}/>`}
      ${tab === 'glossary' && html`<${STTGlossary} data=${data}/>`}
    <//>`}`;
}

const starterSheet = /** @type {HTMLDialogElement} */ (document.getElementById('starter-save-sheet'));
starterSheet.addEventListener('close', () => render(null, starterSheet));

/**
 * saveAsStarter opens the editor on the room for a room admin; the starter lands in their own
 * library (006-AC15).
 * @param {string} base @param {(s: string) => void} say
 */
async function saveAsStarter(base, say) {
  const view = await fetchJSON('GET', base + '/starter-draft').catch((err) => { say(err.message); return null; });
  if (!view) return;
  const close = () => starterSheet.close();
  render(html`<div class="sheet-head"><h2 id="starter-save-title">Save as starter</h2>
      <button type="button" class="btn-ghost btn-icon" aria-label="Close" onClick=${close} dangerouslySetInnerHTML=${icon('close')}></button></div>
    <${Editor} view=${view} api=${fetchJSON} mode="save" path=${base + '/starters'} back=${close}
      done=${(/** @type {import('./starters.js').Starter} */ st) => {
        close();
        dispatchEvent(new CustomEvent('grill:toast', { detail: 'Saved "' + st.title + '" to your starters, on your profile page' }));
      }}/>`, starterSheet);
  starterSheet.showModal();
}

/**
 * DeletedDocuments lists the room's deleted documents for a room admin to restore (005-AC26).
 * @param {{base: string, show: (err: unknown) => void}} props
 */
function DeletedDocuments({ base, show }) {
  const [list, setList] = useState(/** @type {{id: number, name: string, author: string}[] | null} */ (null));
  const load = () => fetchJSON('GET', base + '/documents/deleted').then(setList).catch(show);
  useEffect(() => { load(); }, [base]);
  /** @param {number} id */
  const restore = (id) => fetchJSON('POST', `${base}/documents/${id}/restore`).then(load).catch(show);
  return html`
    <p class="hint">Documents a room admin deleted. They left the room and the AI's workspace; their data stays until the room is deleted.</p>
    ${list && (list.length ? html`<ul id="deleted-documents">${list.map((d) => html`<li key=${d.id}>${d.name}, by ${d.author}
      ${' '}<button type="button" class="btn-secondary" onClick=${() => restore(d.id)} aria-label=${'Restore ' + d.name}>Restore</button></li>`)}</ul>`
      : html`<p class="empty">No deleted document.</p>`)}`;
}

/**
 * STTGlossary edits the terms the transcription expects, for every member (005-AC23). The worker
 * reads them before each utterance, with the room's names and document titles, while they fit.
 * @param {{data: RoomData}} props
 */
function STTGlossary({ data }) {
  const base = '/api/rooms/' + encodeURIComponent(data.room.id) + '/glossary';
  const [g, setG] = useState(/** @type {Glossary | null} */ (null));
  const [text, setText] = useState('');
  const [said, setSaid] = useState('');
  /** @param {Glossary} v */
  const got = (v) => { setG(v); setText(v.glossary); setSaid(''); };
  /** @param {unknown} err */
  const show = (err) => setSaid(/** @type {Error} */ (err).message);
  useEffect(() => { fetchJSON('GET', base).then(got).catch(show); }, [base]);
  /** @param {Event} e */
  const save = (e) => {
    e.preventDefault();
    fetchJSON('PUT', base, { glossary: text }).then((v) => { got(v); setSaid('Saved: it applies from the next utterance.'); }).catch(show);
  };
  return html`
    <p class="hint">Words people will say, so that voice-to-text writes them right: product names, jargon, acronyms. One per line or comma-separated.
      It also expects the participants' and personas' names, then the documents' titles, while they fit.</p>
    <p class="hint" id="glossary-engine">Used by the transcription engine when supported.</p>
    <form id="stt-glossary" onSubmit=${save}>
      <textarea rows="5" aria-label="Words to recognise" value=${text} onInput=${(/** @type {Event} */ e) => setText(/** @type {HTMLTextAreaElement} */ (e.target).value)}></textarea>
      <div class="glossary-actions">
        <span id="glossary-used" data-tip=${g ? g.prompt : ''}>${g ? `${g.used} / ~${g.capacity} terms used` : ''}</span>
        <button type="submit">Save</button>
      </div>
    </form>
    <p class="said" role="status">${said}</p>`;
}

function App() {
  const data = roomData.value;
  if (!tab.value || !data) return null;
  const tabs = sttGlossary.value ? allTabs : allTabs.filter(([id]) => id !== 'glossary');
  if (data.me.admin) return html`<${Manage} data=${data} tabs=${tabs} tab=${tabs.some(([id]) => id === tab.value) ? tab.value : 'general'}/>`;
  const mine = tabs.filter(([id]) => memberTabs.includes(id));
  // A link to an admin's tab, or to the glossary while no engine reads it, opens the first one.
  const t = mine.some(([id]) => id === tab.value) ? tab.value : mine[0][0];
  const base = '/api/rooms/' + encodeURIComponent(data.room.id);
  return html`<${TabBar} tabs=${mine} active=${t} href=${(/** @type {string} */ id) => '#manage/' + id} label="Room settings"/>
    <div class="tab-panel" id=${'panel-' + t}>${t === 'glossary' && html`<${STTGlossary} data=${data}/>`}
      ${t === 'plugins' && html`<${PluginsTab} base=${base} admin=${false} host=${isHost(data)} personas=${agentState.value?.personas || []}/>`}</div>`;
}

/** @param {RoomData} data */
const isHost = (data) => agentState.value?.host_participant_id === data.me.id;

render(html`<${App}/>`, /** @type {HTMLElement} */ (document.getElementById('room-admin')));
mountReview(roomData, agentState);
