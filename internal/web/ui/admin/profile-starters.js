// The profile page's room starters (006-AC1): the library this profile sees, with its row menus.
// profile.js, vanilla, keeps the rest of the page.
import { render } from 'preact';
import { useState, useEffect } from 'preact/hooks';
import { html } from 'htm/preact';
import { fetchJSON } from './room-panel.js';
import { StarterList } from './starters.js';

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
