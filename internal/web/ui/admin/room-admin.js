// The room admin panel of the room page (004-AC8, 004-AC19): Preact, htm and signals, like /admin.
// room.js, vanilla, hands over the room and the agent state it loads as window.grillRoom and
// window.grillAgent, each followed by an event.
import { render } from 'preact';
import { useState, useEffect } from 'preact/hooks';
import { signal } from '@preact/signals';
import { html } from 'htm/preact';
import { RoomPanel, fetchJSON } from './room-panel.js';

/**
 * @typedef {{id: number, name: string, admin: boolean}} Participant
 * @typedef {{room: {id: string, name: string}, me: Participant, participants: Participant[]}} RoomData
 * @typedef {{id: number, name: string, version: number, author: string, pending?: boolean}} RoomSkill
 * @typedef {{room_skills?: RoomSkill[]}} AgentState
 */

const w = /** @type {Window & {grillRoom?: RoomData, grillAgent?: AgentState}} */ (window);
const roomData = signal(w.grillRoom);
const agentState = signal(w.grillAgent);
const open = signal(false);
addEventListener('grill:room', (e) => { roomData.value = /** @type {CustomEvent<RoomData>} */ (e).detail; });
addEventListener('grill:agent', (e) => { agentState.value = /** @type {CustomEvent<AgentState>} */ (e).detail; });
const dialog = /** @type {HTMLDialogElement} */ (document.getElementById('manage'));
document.getElementById('manage-open')?.addEventListener('click', () => { open.value = true; });
dialog.addEventListener('close', () => { open.value = false; });

/** @param {{data: RoomData}} props */
function Manage({ data }) {
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
    if (window.confirm(`Remove the room skill ${k.name}? Personas that have it on lose it.`)) fetchJSON('DELETE', `${base}/skills/${k.id}`).catch(show);
  };
  const skills = agentState.value?.room_skills || [];
  return html`
    <p class="said" role="alert">${said}</p>
    ${room && html`<${RoomPanel} key=${room.id} room=${room} base=${base} api=${fetchJSON} show=${show} reload=${load} byName=${byName}
      close=${() => location.assign('/profile')}>
      <h3>Room admins</h3>
      <p class="hint">Room admins rename the room, manage every invite, appoint other admins and delete the room. The room keeps at least one.</p>
      <ul id="room-admins">${data.participants.map((p) => html`<li key=${p.id}>
        ${p.name}${p.admin && html` <strong>admin</strong>`}${' '}
        ${p.admin
          ? html`<button type="button" class="small" onClick=${() => setAdmin(p, false)}>${p.id === data.me.id ? 'Step down' : 'Remove admin'}</button>`
          : html`<button type="button" class="small" onClick=${() => setAdmin(p, true)} aria-label=${'Make ' + p.name + ' a room admin'}>Make admin</button>`}
      </li>`)}</ul>
      <h3>Room skills</h3>
      ${skills.length ? html`<ul id="room-admin-skills">${skills.map((k) => html`<li key=${k.id}>${k.name} v${k.version}, copied by ${k.author}
        ${' '}<button type="button" class="small" onClick=${() => removeSkill(k)} aria-label=${'Remove the room skill ' + k.name}>Remove</button></li>`)}</ul>`
        : html`<p class="hint">None yet. Members copy their personal skills in from the Personas panel.</p>`}
    <//>`}`;
}

function App() {
  const data = roomData.value;
  return open.value && data?.me.admin ? html`<${Manage} data=${data}/>` : null;
}

render(html`<${App}/>`, /** @type {HTMLElement} */ (document.getElementById('room-admin')));
