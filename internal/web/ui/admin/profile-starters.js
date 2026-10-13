// The profile page's room starters (006): the library this profile sees with its row menus, and
// "New room", the picker then the editor, in its sheet. profile.js, vanilla, keeps the rest of the
// page.
import { render } from 'preact';
import { useState, useEffect } from 'preact/hooks';
import { html } from 'htm/preact';
import { fetchJSON } from './room-panel.js';
import { StarterList, NewRoom } from './starters.js';

function Starters() {
  const [starters, setStarters] = useState(/** @type {import('./starters.js').Starter[] | null} */ (null));
  const [said, setSaid] = useState('');
  // A browser with no profile gets a 401: the section stays empty, as the rest of the page does.
  const load = () => fetchJSON('GET', '/api/profile/starters').then(setStarters).catch(() => setStarters(null));
  useEffect(() => { load(); }, []);
  if (!starters) return null;
  return html`<h2 id="starters-title">Room starters</h2>
    <p class="hint">A starter sets a new room up: its agenda, personas, documents and skills. Rooms keep their own copy.</p>
    <p class="said" role="status">${said}</p>
    <${StarterList} starters=${starters} api=${fetchJSON} reload=${load} say=${setSaid}/>`;
}

render(html`<${Starters}/>`, /** @type {HTMLElement} */ (document.getElementById('starters')));

const sheet = /** @type {HTMLDialogElement} */ (document.getElementById('room-create-sheet'));
const close = () => sheet.close();
// Each opening starts on the picker; closing empties the sheet. A press outside closes it, as every
// sheet does (005-AC13).
sheet.addEventListener('close', () => render(null, sheet));
sheet.addEventListener('click', (e) => {
  const r = sheet.getBoundingClientRect();
  if (e.target === sheet && (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom)) sheet.close();
});
document.getElementById('room-new')?.addEventListener('click', async () => {
  // The name the profile chose (005-AC1), else the one it last joined with.
  const { profile } = await fetchJSON('GET', '/api/profile').catch(() => ({ profile: null }));
  const name = profile ? profile.name || (profile.rooms[0] || {}).name || '' : '';
  render(html`<${NewRoom} api=${fetchJSON} displayName=${name} close=${close}/>`, sheet);
  sheet.showModal();
});
