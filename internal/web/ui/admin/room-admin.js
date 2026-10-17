// The room's Manage sheet (004-AC8, 004-AC19, 005-AC15): Preact, htm and signals, like /admin.
// room.js, vanilla, hands over the room, the agent state and the transcription state it loads as
// window.grillRoom, window.grillAgent and window.grillSTT, each followed by an event. Each tab is a
// deep link, #manage/<tab>.
import { render } from 'preact';
import { useState, useEffect } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';
import { RoomPanel, AdminRows, TabBar, Invites, fetchJSON, roomTabs, icon } from './room-panel.js';
import { Editor } from './starters.js';
import { PluginsTab, mountReview } from './plugins.js';

/**
 * @typedef {{id: number, name: string, admin: boolean}} Participant
 * @typedef {{quiet_ms: number, resume_s: number, queue_s: number, words: number}} Speech
 * @typedef {{room: {id: string, name: string, speech?: Speech}, me: Participant, participants: Participant[], tts?: boolean}} RoomData
 * @typedef {{id: number, name: string, version: number, author: string, pending?: boolean}} RoomSkill
 * @typedef {{id: number, name: string, badge: string, model: string, effort: string, pending?: boolean}} Persona
 * @typedef {{room_skills?: RoomSkill[], personas?: Persona[], host_participant_id?: number, you_host?: boolean, egress_admins_allow?: boolean,
 *   connectors?: {name: string, actions?: boolean}[]}} AgentState
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
  ['network', 'Websites and connectors'], ['documents', 'Documents'], ['export', roomTabs.export], ['glossary', 'Words to recognise'],
  ['voices', 'Call voices']];
// The tabs a plain member gets; a room admin gets them all. Every member reads the room's plugins
// (009-AC5), its websites (014) and manages their own invites (011-AC27).
const memberTabs = ['invites', 'plugins', 'network', 'glossary'];

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
    <p class="said" role="alert">${said}</p>
    ${room && html`<${RoomPanel} key=${room.id} room=${room} base=${base} api=${fetchJSON} show=${show} reload=${load} byName=${byName} me=${data.me.id}
      tab=${tab} tabs=${tabs} href=${(/** @type {string} */ id) => '#manage/' + id} close=${() => location.assign('/profile')} label="Room settings" heading=${false}
      general=${html`<h3>Save as starter</h3>
        <p class="hint">Keep this room's agenda, AI personas and skills, and the documents and plugins you pick, to start new rooms the same way.</p>
        <div class="row"><button type="button" class="btn-secondary" id="save-starter" onClick=${() => saveAsStarter(base, setSaid)}>Save as starter</button></div>
        <p class="hint">The server's audit log records what room admins do here.</p>`}>
      ${tab === 'admins' && html`
        <p class="hint">Room admins rename the room and its participants, manage every invite, appoint other admins and delete the room. The room keeps at least one.</p>
        <${AdminRows} people=${data.participants} base=${base} api=${fetchJSON} show=${show} done=${() => setSaid('')} me=${data.me.id}/>`}
      ${tab === 'personas' && html`
        <p class="hint">Every member adds and edits the room's personas in the Personas sheet. A persona's name opens its own sheet.</p>
        <ul id="manage-personas">${personas.map((p) => html`<li key=${p.id}><span><span class="badge persona-badge">${p.badge}</span> <a href=${'#personas/' + p.id + '/identity'}
          aria-label=${p.badge + ' ' + p.name}>${p.name}</a> <span class="hint">${p.model} · effort ${p.effort}${p.pending ? ' · waits for the AI host' : ''}</span></span></li>`)}</ul>
        <button type="button" class="btn-primary" onClick=${openPersonas}>Open the Personas sheet</button>`}
      ${tab === 'skills' && (skills.length ? html`<ul id="room-admin-skills">${skills.map((k) => html`<li key=${k.id}>${k.name} v${k.version}, copied by ${k.author}
          ${' '}<button type="button" class="btn-secondary" onClick=${() => removeSkill(k)} aria-label=${'Remove the room skill ' + k.name}>Remove</button></li>`)}</ul>`
        : html`<p class="empty">None yet. Members copy their personal skills in from the Personas sheet.</p>`)}
      ${tab === 'plugins' && html`<${PluginsTab} base=${base} admin=${true} host=${approves(data)} personas=${personas}/>`}
      ${tab === 'network' && html`<${NetworkTab} base=${base} host=${isHost(data)} show=${show}/>`}
      ${tab === 'documents' && html`<${DeletedDocuments} base=${base} show=${show}/>`}
      ${tab === 'glossary' && html`<${STTGlossary} data=${data}/>`}
      ${tab === 'voices' && html`<${CallVoices} data=${data} base=${base} show=${show}/>`}
    <//>`}`;
}

// The bounds the server holds each call setting to (013-AC25).
const speechFields = /** @type {[keyof Speech, string, number, number, number, string][]} */ ([
  ['quiet_ms', 'Quiet before a persona speaks (seconds)', 0.5, 5, 0.1, 'A persona starts only once nobody has spoken for this long.'],
  ['resume_s', 'Resume window (seconds)', 0, 60, 1, 'Cut off by someone, a persona says its sentence again if the room is quiet within it; past it, it drops the rest.'],
  ['queue_s', 'Longest wait (seconds)', 10, 300, 1, 'A line not started by then is dropped from speech; the chat keeps it.'],
  ['words', 'Spoken words', 40, 60, 1, 'The most a persona says after a turn.'],
]);

/**
 * CallVoices sets how the room's personas take turns in the call, for room admins (013-AC25).
 * @param {{data: RoomData, base: string, show: (err: unknown) => void}} props
 */
function CallVoices({ data, base, show }) {
  const sp = data.room.speech || { quiet_ms: 1000, resume_s: 20, queue_s: 40, words: 60 };
  /** @param {keyof Speech} k @param {number} v */
  const shown = (k, v) => (k === 'quiet_ms' ? String(v / 1000) : String(v));
  const [vals, setVals] = useState(Object.fromEntries(speechFields.map(([k]) => [k, shown(k, sp[k])])));
  const [said, setSaid] = useState('');
  useEffect(() => { setVals(Object.fromEntries(speechFields.map(([k]) => [k, shown(k, sp[k])]))); }, [sp.quiet_ms, sp.resume_s, sp.queue_s, sp.words]);
  const dirty = speechFields.some(([k]) => vals[k] !== shown(k, sp[k]));
  /** @param {Event} e */
  const save = (e) => {
    e.preventDefault();
    const body = Object.fromEntries(speechFields.map(([k]) => [k, k === 'quiet_ms' ? Math.round(Number(vals[k]) * 1000) : Number(vals[k])]));
    fetchJSON('PATCH', base + '/speech', body).then(() => setSaid('Saved: it applies to the next line.')).catch(show);
  };
  return html`<p class="hint">Personas say a short line in the call after their turns, one at a time, never over someone speaking.</p>
    <form id="call-voices" class="settings-form" onSubmit=${save}>
      ${speechFields.map(([k, label, min, max, step, hint]) => html`<label key=${k}>${label}
        <input name=${k} type="number" min=${min} max=${max} step=${step} required value=${vals[k]}
          onInput=${(/** @type {Event} */ e) => { setSaid(''); setVals({ ...vals, [k]: /** @type {HTMLInputElement} */ (e.currentTarget).value }); }}/>
        <span class="hint">${hint} ${min} to ${max}.</span></label>`)}
      <div class="settings-foot"><span class="dirty">${said || (dirty ? 'Unsaved changes' : 'No unsaved changes')}</span>
        <button type="submit" class="btn-primary" disabled=${!dirty}>Save</button></div>
    </form>`;
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

// The sheet's one heading names the room (004b-AC11).
const title = /** @type {HTMLElement} */ (document.getElementById('manage-title'));

function App() {
  const [said, setSaid] = useState('');
  /** @param {unknown} err */
  const show = (err) => setSaid(/** @type {Error} */ (err).message);
  const data = roomData.value;
  if (data) title.textContent = 'Room settings · ' + data.room.name;
  if (!tab.value || !data) return null;
  const tabs = allTabs.filter(([id]) => (id !== 'glossary' || sttGlossary.value) && (id !== 'voices' || data.tts));
  if (data.me.admin) return html`<${Manage} data=${data} tabs=${tabs} tab=${tabs.some(([id]) => id === tab.value) ? tab.value : 'general'}/>`;
  const mine = tabs.filter(([id]) => memberTabs.includes(id));
  // A link to an admin's tab, or to the glossary while no engine reads it, opens the first one.
  const t = mine.some(([id]) => id === tab.value) ? tab.value : mine[0][0];
  const base = '/api/rooms/' + encodeURIComponent(data.room.id);
  /** @param {number} id */
  const byName = (id) => data.participants.find((p) => p.id === id)?.name || 'a former participant';
  return html`<${TabBar} tabs=${mine} active=${t} href=${(/** @type {string} */ id) => '#manage/' + id} label="Room settings"/>
    ${said && html`<p class="said" role="alert">${said}</p>`}
    <div class="tab-panel" id=${'panel-' + t}>${t === 'glossary' && html`<${STTGlossary} data=${data}/>`}
      ${t === 'invites' && html`<${Invites} base=${base} api=${fetchJSON} show=${show} byName=${byName} me=${data.me.id}/>`}
      ${t === 'plugins' && html`<${PluginsTab} base=${base} admin=${false} host=${approves(data)} personas=${agentState.value?.personas || []}/>`}
      ${t === 'network' && html`<${NetworkTab} base=${base} host=${isHost(data)} show=${show}/>`}</div>`;
}

/**
 * NetworkTab is what the room's agents may reach (014): the AI host lets the room's admins allow
 * websites for the room, or not (014-AC12); everyone else reads which.
 * @param {{base: string, host: boolean, show: (err: unknown) => void}} props
 */
function NetworkTab({ base, host, show }) {
  const on = !!agentState.value?.egress_admins_allow;
  const connectors = agentState.value?.connectors || [];
  // The box shows the host's click until the agent state confirms it.
  const [clicked, setClicked] = useState(/** @type {boolean | null} */ (null));
  useEffect(() => { if (clicked === on) setClicked(null); }, [on, clicked]);
  /** @param {Event} e */
  const toggle = (e) => {
    const next = /** @type {HTMLInputElement} */ (e.currentTarget).checked;
    setClicked(next);
    fetchJSON('PUT', base + '/egress-delegation', { on: next }).catch((err) => { setClicked(null); show(err); });
  };
  return html`<h3>Websites</h3>
    <p class="hint">When a persona is refused a website, the chat says so. The AI host allows it for this room, or for every room on their computer.</p>
    ${host ? html`<label><input id="egress-admins" type="checkbox" checked=${clicked ?? on} onChange=${toggle}/> Room admins may allow websites for this room</label>
      <p class="hint">For this room only. It turns off when someone else becomes the AI host.</p>`
    : html`<p id="egress-admins-state">${on ? 'Room admins may allow websites for this room.' : 'Only the AI host allows websites for this room.'}</p>`}
    <h3>Connectors</h3>
    <p class="hint">claude.ai connectors of the AI host's account, which the host turns on for this room from their app. A persona uses them as the host.</p>
    ${connectors.length ? html`<ul id="room-connectors">${connectors.map((c) => html`<li key=${c.name}>${c.name}
      <span class="hint">${c.actions ? ' · can act: send, create, change' : ' · read only'}</span></li>`)}</ul>`
    : html`<p class="empty" id="room-connectors-none">None on for this room.</p>`}`;
}

/** @param {RoomData} data */
const isHost = (data) => agentState.value?.host_participant_id === data.me.id;
/** Approvals follow the host's profile: any of its seats here reviews a plugin (daemon-r2). @param {RoomData} data */
const approves = (data) => isHost(data) || !!agentState.value?.you_host;

render(html`<${App}/>`, /** @type {HTMLElement} */ (document.getElementById('room-admin')));
mountReview(roomData, agentState);
