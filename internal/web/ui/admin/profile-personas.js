// The profile page's persona library (035): Yours, Shared with everyone and Built-in, each library
// persona's page, the author's editor and the row menus. profile.js, vanilla, keeps the rest of the
// page; the words are "personas", the routes keep "templates" (035 Decisions).
import { render } from 'preact';
import { useState, useEffect, useRef } from 'preact/hooks';
import { html } from 'htm/preact';
import { fetchJSON, icon, when, TabBar } from './room-panel.js';
import { RowMenu, triggers, triggerOf, triggerText } from './starters.js';

/**
 * @typedef {import('./starters.js').StarterPersona & {id: number, section: string, mine: boolean, title: string, version: number,
 *   author: string, description: string, voice?: string, voice_rate?: number, created_at: string,
 *   frozen_skills?: {name: string, version: number}[], frozen_plugins?: {name: string, ref_name: string}[]}} LibPersona
 * @typedef {{id: string, name: string, lang: string}} Voice
 * @typedef {LibPersona & {versions: {n: number, author: string, created_at: string}[], models: string[], efforts: string[],
 *   voices: Voice[], tts: boolean}} LibView
 * @typedef {(text: string) => void} Say
 */

/** The sections in the order the tab lists them (035-AC1). */
const sections = /** @type {const} */ ([['personal', 'Yours'], ['instance', 'Shared with everyone'], ['builtin', 'Built-in']]);
/** @type {Record<string, string>} */
const emptyText = {
  personal: 'No personas of yours yet. Save one from a room, or duplicate one below.',
  instance: 'Nobody has shared a persona yet.',
  builtin: 'None.',
};

const sheet = /** @type {HTMLDialogElement} */ (document.getElementById('persona-lib-sheet'));
const roomSheet = /** @type {HTMLDialogElement} */ (document.getElementById('persona-room-sheet'));
for (const d of [sheet, roomSheet]) {
  // Closing empties the sheet; a press outside closes it, as every sheet does (005-AC13).
  d.addEventListener('close', () => render(null, d));
  d.addEventListener('click', (e) => {
    const r = d.getBoundingClientRect();
    if (e.target === d && (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom)) d.close();
  });
}

/**
 * actionsFor is a library persona's ⋯, on its row and on its page (035-AC4): the author's own get
 * Duplicate, Share or Stop sharing and Delete…; anyone else's, a built-in included, Duplicate to yours.
 * gone runs once it is deleted; reload after any other change.
 * @param {LibPersona} t @param {Say} say @param {() => void} reload @param {() => void} [gone]
 * @returns {[string, () => void][]}
 */
function actionsFor(t, say, reload, gone = reload) {
  const base = '/api/profile/personas/' + t.id;
  // A refusal may come from a list gone stale, another tab's delete or unshare: read it again.
  /** @param {unknown} err */
  const fail = (err) => { say(/** @type {Error} */ (err).message); reload(); };
  const dup = () => fetchJSON('POST', base + '/duplicate').then((/** @type {LibPersona} */ c) => { say('Duplicated as ' + c.title + '.'); reload(); }).catch(fail);
  /** @type {[string, () => void][]} */
  const out = [['Add to a room…', () => addToRoom(t, say, reload)]];
  if (!t.mine) return [...out, ['Duplicate to yours', dup]];
  out.push(['Duplicate', dup]);
  // 035-AC5: sharing asks first; stopping does not, rooms keep their copies either way.
  out.push(t.section === 'personal'
    ? ['Share with everyone', () => window.grillConfirm('Share ' + t.title + ' with everyone?',
      'Everyone on this server will see ' + t.title + ' and can add it to their rooms. Rooms keep their copies if you stop sharing.', 'Share')
      .then((yes) => { if (yes) fetchJSON('POST', base + '/share').then(() => { say('Shared ' + t.title + ' with everyone.'); reload(); }).catch(fail); })]
    : ['Stop sharing', () => fetchJSON('POST', base + '/unshare').then(() => { say('Stopped sharing ' + t.title + '.'); reload(); }).catch(fail)]);
  out.push(['Delete…', () => window.grillConfirm('Delete ' + t.title + '?', 'Rooms keep the personas they added from it.', 'Delete')
    .then((yes) => { if (yes) fetchJSON('DELETE', base).then(() => { say('Deleted ' + t.title + '.'); gone(); }).catch(fail); })]);
  return out;
}

/**
 * RoomPick adds a library persona to one of the profile's rooms (035-AC4): any room it has a seat
 * in, where every member may add personas.
 * @param {{t: LibPersona, say: Say, reload: () => void}} props
 */
function RoomPick({ t, say, reload }) {
  const [rooms, setRooms] = useState(/** @type {{room_id: string, room_name: string}[] | null} */ (null));
  const [room, setRoom] = useState('');
  const [said, setSaid] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    fetchJSON('GET', '/api/profile').then(({ profile }) => {
      const rs = profile ? profile.rooms : [];
      setRooms(rs);
      setRoom(rs.length ? rs[0].room_id : '');
    }).catch((err) => { setRooms([]); setSaid(err.message); });
  }, []);
  const add = () => {
    const r = (rooms || []).find((x) => x.room_id === room);
    if (!r) return;
    setBusy(true);
    fetchJSON('POST', '/api/profile/personas/' + t.id + '/rooms', { room }).then((/** @type {{name: string, pending?: boolean}} */ p) => {
      roomSheet.close();
      // A room that has a persona of that name already gives the new one another name.
      const as = p.name && p.name !== t.name ? ' as ' + p.name : '';
      say('Added ' + t.title + as + ' to ' + r.room_name + (p.pending ? ': it waits for the AI host\'s approval.' : '.'));
    }).catch((err) => {
      setSaid(err.message);
      setBusy(false);
      // The persona may be gone, deleted or unshared since the list was read.
      if (err.status === 404) reload();
    });
  };
  const none = rooms !== null && !rooms.length;
  return html`<div class="sheet-head"><h2 id="persona-room-title">Add ${t.title} to a room</h2>
      <button type="button" class="btn-ghost btn-icon" aria-label="Close" onClick=${() => roomSheet.close()} dangerouslySetInnerHTML=${icon('close')}></button></div>
    <form class="stack" onSubmit=${(/** @type {Event} */ e) => { e.preventDefault(); add(); }}>
      ${none ? html`<p class="hint" id="persona-room-none">You have no rooms yet. Create or join one first.</p>`
        : html`<label for="persona-room">Room</label>
        <select id="persona-room" value=${room} disabled=${!rooms} onChange=${(/** @type {Event} */ e) => setRoom(/** @type {HTMLSelectElement} */ (e.currentTarget).value)}>
          ${(rooms || []).map((r) => html`<option key=${r.room_id} value=${r.room_id}>${r.room_name}</option>`)}</select>`}
      <p class="field-error" role="alert">${said}</p>
      <div class="actions">
        <button type="button" class="btn-secondary" onClick=${() => roomSheet.close()}>Cancel</button>
        <button type="submit" disabled=${!room || busy} aria-describedby=${none ? 'persona-room-none' : undefined}>Add</button>
      </div>
    </form>`;
}

/** @param {LibPersona} t @param {Say} say @param {() => void} reload */
function addToRoom(t, say, reload) {
  render(html`<${RoomPick} t=${t} say=${say} reload=${reload}/>`, roomSheet);
  roomSheet.showModal();
}

/** @param {boolean | undefined} v */
const yes = (v) => (v ? 'Yes' : 'No');
/** @param {string} s */
const cap = (s) => s.charAt(0).toUpperCase() + s.slice(1);
/** A persona saved before 013 has no speaks: it speaks (013-AC30). @param {LibPersona} t */
const speaksOf = (t) => t.speaks !== false;
/** The languages' names, as the room's voice picker words them. @param {string} l */
const langName = (l) => ({ fr: 'French', en: 'English' }[l] || l);
/** @param {Voice} x */
const voiceName = (x) => x.name + ' (' + langName(x.lang) + ')';
/** @param {LibView} v */
const voiceOf = (v) => {
  const x = v.voices.find((y) => y.id === v.voice);
  return x ? voiceName(x) : v.voice || 'Default voice';
};

/**
 * Facts lists a group of settings as a term and its value.
 * @param {{rows: [string, unknown][]}} props
 */
const Facts = ({ rows }) => html`<dl class="profile-facts persona-lib-facts">${rows.map(([k, v]) => html`<div key=${k}><dt>${k}</dt><dd>${v}</dd></div>`)}</dl>`;

/**
 * Read shows every setting of a library persona, read-only, then its versions (035-AC2).
 * @param {{v: LibView}} props
 */
function Read({ v }) {
  const skills = v.frozen_skills || [];
  const plugins = v.frozen_plugins || [];
  const keeper = v.time_keeper ? (speaksOf(v) && v.cut_in_s ? 'Yes, speaks over after ' + v.cut_in_s + ' s' : 'Yes') : 'No';
  return html`<div class="persona-lib-read">
    <h3>Identity</h3>
    <${Facts} rows=${[['Badge', v.badge], ['Name', v.name],
      ['Colour', html`<span class="persona-lib-swatch" style=${'background:' + v.colour} aria-hidden="true"></span> ${v.colour}`],
      ['Description', v.description || 'None.'], ['Persona text', html`<pre class="starter-agenda">${v.text}</pre>`]]}/>
    <h3>Model</h3>
    <${Facts} rows=${[['Model', v.model], ['Effort', v.effort], ['Memory size', v.autocompact ? v.autocompact / 1000 + 'k tokens' : 'Automatic']]}/>
    <h3>Triggers</h3>
    <${Facts} rows=${[['Speaks up', cap(triggerText(v))], ['Follows up on its own', yes(v.self_call)], ['Watches the notes', yes(v.watches_notes)],
      ['Time keeper', keeper]]}/>
    <h3>Call</h3>
    <${Facts} rows=${[['Speaks in the call', yes(speaksOf(v))], ['Voice', voiceOf(v)], ['Speaking rate', (v.voice_rate || 1) + '×']]}/>
    <h3>Capabilities</h3>
    <${Facts} rows=${[['Skills', skills.length ? html`<ul class="persona-lib-caps">${skills.map((k) => html`<li key=${k.name}>${k.name} v${k.version}</li>`)}</ul>` : 'None.'],
      ['Plugins', plugins.length ? html`<ul class="persona-lib-caps">${plugins.map((p) => html`<li key=${p.name}>${p.name}, ${p.ref_name}</li>`)}</ul>` : 'None.']]}/>
    <h3>Versions</h3>
    <ul class="persona-lib-history" id="persona-lib-history">${v.versions.map((x) => html`<li key=${x.n}>v${x.n} · by ${x.author} · <time datetime=${x.created_at}>${when(x.created_at)}</time></li>`)}</ul>
  </div>`;
}

/** The editor's tabs, the room persona sheet's four (035-AC3). */
const editTabs = [['identity', 'Identity'], ['model', 'Model'], ['triggers', 'Triggers'], ['capabilities', 'Capabilities']];

/**
 * Edit is the author's editor (035-AC3): the room persona sheet's fields on its four tabs, plus the
 * description. Saving makes the next version of the one opened; rooms keep theirs. A refusal keeps
 * the draft; once the page knows of a newer version, latest, Reopen starts over from it.
 * @param {{v: LibView, latest: number, done: (saved: LibPersona) => void, cancel: () => void, conflict: () => void,
 *   reopen: () => void, reopened: boolean}} props
 */
function Edit({ v, latest, done, cancel, conflict, reopen, reopened }) {
  const t0 = triggerOf(v);
  const [f, setF] = useState({
    name: v.name, badge: v.badge, colour: v.colour, text: v.text, description: v.description, model: v.model, effort: v.effort,
    autocompact: v.autocompact ? String(v.autocompact / 1000) : '', kind: t0.kind, n: String(t0.n || 1), self_call: !!v.self_call,
    watches_notes: !!v.watches_notes, time_keeper: !!v.time_keeper, cut_in_s: String(v.cut_in_s || 0), speaks: speaksOf(v),
    voice: v.voice || '', voice_rate: String(v.voice_rate || 1),
  });
  const [tab, setTab] = useState('identity');
  const [said, setSaid] = useState('');
  const [busy, setBusy] = useState(false);
  const form = useRef(/** @type {HTMLFormElement | null} */ (null));
  // Reopen replaced the button that was pressed: the focus starts the new draft.
  useEffect(() => { if (reopened) /** @type {HTMLElement | null | undefined} */ (form.current?.querySelector('input[name=name]'))?.focus(); }, []);
  /** @param {Partial<typeof f>} patch */
  const set = (patch) => setF({ ...f, ...patch });
  /** @param {keyof typeof f} k */
  const text = (k) => (/** @type {Event} */ e) => set({ [k]: /** @type {HTMLInputElement} */ (e.currentTarget).value });
  /** @param {keyof typeof f} k */
  const check = (k) => (/** @type {Event} */ e) => set({ [k]: /** @type {HTMLInputElement} */ (e.currentTarget).checked });
  const submit = () => {
    // A hidden tab's field cannot show the browser's bubble: show its tab, then the field's error.
    const bad = /** @type {HTMLInputElement | null} */ (form.current?.querySelector('input:invalid, select:invalid, textarea:invalid'));
    if (bad) {
      setTab(/** @type {HTMLElement} */ (bad.closest('[data-tab]')).dataset.tab || 'identity');
      requestAnimationFrame(() => bad.reportValidity());
      return;
    }
    const n = f.kind ? Number(f.n) : 0;
    setBusy(true);
    fetchJSON('POST', '/api/profile/personas/' + v.id + '/versions', {
      version: v.version, description: f.description, name: f.name, badge: f.badge, colour: f.colour, text: f.text, model: f.model, effort: f.effort,
      autocompact: f.autocompact ? Math.round(Number(f.autocompact) * 1000) : 0,
      every_n: f.kind === 'every_n' ? n : 0, every_voice: f.kind === 'every_voice' ? n : 0, every_minutes: f.kind === 'every_minutes' ? n : 0,
      self_call: f.self_call, watches_notes: f.watches_notes, time_keeper: f.time_keeper, cut_in_s: Number(f.cut_in_s),
      speaks: f.speaks, voice: f.voice, voice_rate: Number(f.voice_rate),
    }).then(done).catch((err) => {
      setSaid(err.message);
      setBusy(false);
      // A newer version from another tab, or a limit: the page reads the persona again behind the draft.
      if (err.status === 409) conflict();
    });
  };
  const skills = v.frozen_skills || [];
  const plugins = v.frozen_plugins || [];
  const max = /** @type {Record<string, number>} */ ({ every_n: 100, every_voice: 500, every_minutes: 240 })[f.kind] || 500;
  return html`<form ref=${form} class="persona-form persona-lib-edit" noValidate style=${'--persona:' + f.colour}
      onSubmit=${(/** @type {Event} */ e) => { e.preventDefault(); if (!busy) submit(); }}>
    <div onClick=${(/** @type {MouseEvent} */ e) => {
      // The tabs are links, in the keyboard's reach, that switch the form in place.
      const a = /** @type {HTMLElement} */ (e.target).closest('a[id^="tab-"]');
      if (a) { e.preventDefault(); setTab(a.id.slice(4)); }
    }}><${TabBar} tabs=${editTabs} active=${tab} href=${() => '#personas'} label="Persona settings" cls="sticky"/></div>
    <p class="hint">You edit version ${v.version}. Saving makes version ${v.version + 1}; rooms keep theirs.</p>
    <section data-tab="identity" hidden=${tab !== 'identity'}>
      <div class="persona-fields">
        <label>Name<input name="name" required maxlength="30" value=${f.name} onInput=${text('name')}/></label>
        <label>Badge<input name="badge" required maxlength="8" value=${f.badge} onInput=${text('badge')}/></label>
        <label>Colour<input name="colour" type="color" value=${f.colour} onInput=${text('colour')}/></label>
      </div>
      <label>Description<input name="description" maxlength="200" value=${f.description} onInput=${text('description')} aria-describedby="persona-lib-desc-hint"/></label>
      <p class="hint" id="persona-lib-desc-hint">One line, shown in the library and the room's picker.</p>
      <label>Persona text<textarea name="text" rows="6" required maxlength="8000" value=${f.text} onInput=${text('text')}></textarea></label>
      ${/* Without speech on this server the call fields say so, as on the room's persona sheet (038-AC12). */ ''}
      ${!v.tts && html`<fieldset class="persona-voice"><legend>In the call</legend><p class="empty unavailable">Not available on this server</p></fieldset>`}
      ${v.tts && html`<fieldset class="persona-voice"><legend>In the call</legend>
        <label class="check"><input type="checkbox" name="speaks" checked=${f.speaks} onChange=${check('speaks')}/> Speaks in the call</label>
        <p class="hint">After its turns it says a short line while someone is in the call. Each listener can mute it for themselves.</p>
        <label>Voice<select name="voice" value=${f.voice} onChange=${text('voice')} aria-describedby="persona-lib-voice-hint">
          <option value="">Default voice</option>${v.voices.map((x) => html`<option key=${x.id} value=${x.id}>${voiceName(x)}</option>`)}</select></label>
        <p class="hint" id="persona-lib-voice-hint">Hear the samples in a room's persona sheet.</p>
        <label>Speaking rate<input name="voice_rate" type="number" min="0.9" max="1.3" step="0.05" required value=${f.voice_rate} onInput=${text('voice_rate')}/></label>
      </fieldset>`}
    </section>
    <section data-tab="model" hidden=${tab !== 'model'}>
      <div class="persona-fields">
        <label>Model<select name="model" value=${f.model} onChange=${text('model')}>${v.models.map((m) => html`<option key=${m}>${m}</option>`)}</select></label>
        <label>Effort<select name="effort" value=${f.effort} onChange=${text('effort')}>${v.efforts.map((m) => html`<option key=${m}>${m}</option>`)}</select></label>
        <label>Memory size (k tokens)<input name="autocompact" type="number" min="100" max="1000" placeholder="auto" value=${f.autocompact} onInput=${text('autocompact')}/></label>
      </div>
      <p class="hint">Memory size is how much it remembers before it summarizes its session; empty for automatic.</p>
    </section>
    <section data-tab="triggers" hidden=${tab !== 'triggers'}>
      <div class="persona-fields">
        <label>Speaks up<select name="trigger" value=${f.kind} onChange=${text('kind')}>${triggers.map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}</select></label>
        ${/* The server's bounds per kind (006-AC18); an empty N would turn the trigger off unseen. */ ''}
        ${f.kind && html`<label>N<input name="every" type="number" min="1" max=${max} required value=${f.n} onInput=${text('n')}/></label>`}
      </div>
      <label class="check"><input type="checkbox" name="self_call" checked=${f.self_call} onChange=${check('self_call')}/> Can follow up on its own: its own @${f.name} gives it another answer</label>
      <label class="check"><input type="checkbox" name="watches_notes" checked=${f.watches_notes} onChange=${check('watches_notes')}/> Watches the notes</label>
      <p class="hint">Answers note threads that name no persona. A room that has a notes watcher keeps it.</p>
      <label class="check"><input type="checkbox" name="time_keeper" checked=${f.time_keeper} onChange=${check('time_keeper')}/> Time keeper</label>
      ${f.time_keeper && f.speaks && html`<label>Speak over after<select name="cut_in_s" value=${f.cut_in_s} onChange=${text('cut_in_s')}>
        ${[['0', 'Off'], ['10', '10 s'], ['20', '20 s'], ['30', '30 s'], ['60', '60 s']].map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}</select></label>`}
      <p class="hint">Sees the raised hands and who speaks for how long; one per room, so a room that has one keeps its own.</p>
    </section>
    <section data-tab="capabilities" hidden=${tab !== 'capabilities'}>
      <p class="hint" id="persona-lib-caps-from">These come from the room it was saved from. Change them in a room, then save it to your personas again.</p>
      <h4>Skills</h4>${skills.length ? html`<ul class="persona-lib-caps">${skills.map((k) => html`<li key=${k.name}>${k.name} v${k.version}</li>`)}</ul>` : html`<p class="empty">None.</p>`}
      <h4>Plugins</h4>${plugins.length ? html`<ul class="persona-lib-caps">${plugins.map((p) => html`<li key=${p.name}>${p.name}, ${p.ref_name}</li>`)}</ul>` : html`<p class="empty">None.</p>`}
    </section>
    <p class="said error" id="persona-lib-edit-status" role="alert">${said}</p>
    <div class="actions">
      <button type="button" class="btn-secondary" onClick=${cancel}>Cancel</button>
      ${latest > v.version && html`<button type="button" class="btn-secondary" id="persona-lib-reopen" onClick=${reopen}>Reopen version ${latest}</button>`}
      <button type="submit" class="btn-primary" id="persona-lib-save" disabled=${busy}>Save version ${v.version + 1}</button>
    </div>
  </form>`;
}

/**
 * Page is a library persona's page in its sheet (035-AC2): read-only, Edit for its author
 * (035-AC3), and the row's ⋯. On a phone the sheet takes the whole screen.
 * @param {{first: LibView, say: Say, reload: () => void}} props
 */
function Page({ first, say, reload }) {
  const [v, setV] = useState(first);
  // base is the version the editor opened from: a reread behind it leaves the draft alone.
  const [base, setBase] = useState(first);
  const [reopened, setReopened] = useState(false);
  const [editing, setEditing] = useState(false);
  const [said, setSaid] = useState('');
  // Leaving the editor gives the focus back to Edit, which it replaced (035-AC3).
  const edit = useRef(/** @type {HTMLButtonElement | null} */ (null));
  const back = useRef(false);
  useEffect(() => { if (!editing && back.current) { back.current = false; edit.current?.focus(); } }, [editing]);
  const leave = () => { back.current = true; setEditing(false); };
  /** @param {string} s */
  const tell = (s) => { setSaid(s); say(s); };
  // Only a persona gone (deleted, unshared) closes its page; any other failure says why.
  const reread = () => fetchJSON('GET', '/api/profile/personas/' + v.id).then(setV)
    .catch((err) => (err.status === 404 ? sheet.close() : tell(err.message)));
  const changed = () => { reload(); return reread(); };
  // The menu item that ran it is gone: the focus goes to the page's heading, not the body.
  const title = useRef(/** @type {HTMLHeadingElement | null} */ (null));
  const items = actionsFor(v, tell, () => changed().then(() => title.current?.focus()), () => { reload(); sheet.close(); });
  return html`<div class="sheet-head"><h2 ref=${title} tabIndex="-1" id="persona-lib-title"><span class="badge persona-badge">${v.badge}</span> ${editing ? 'Edit ' + v.title : v.title}</h2>
      <span class="sheet-tools">
        ${v.mine && !editing && html`<button ref=${edit} type="button" class="btn-secondary" id="persona-lib-edit" onClick=${() => { setSaid(''); setBase(v); setReopened(false); setEditing(true); }}>Edit</button>`}
        ${!editing && html`<${RowMenu} id="persona-lib-menu" label=${'More actions for ' + v.title} items=${items}/>`}
        <button type="button" class="btn-ghost btn-icon" aria-label="Close" onClick=${() => sheet.close()} dangerouslySetInnerHTML=${icon('close')}></button>
      </span></div>
    <p class="hint">v${v.version} · by ${v.author} · ${sections.find(([s]) => s === v.section)?.[1]}</p>
    <p class="said" id="persona-lib-status" role="status">${said}</p>
    ${editing
      ? html`<${Edit} key=${base.version} v=${base} latest=${v.version} cancel=${leave} conflict=${reread} reopen=${() => { setBase(v); setReopened(true); }} reopened=${reopened}
        done=${(/** @type {LibPersona} */ saved) => { leave(); tell('Saved ' + saved.title + ' v' + saved.version + '.'); changed(); }}/>`
      : html`<${Read} v=${v}/>`}`;
}

/** @param {number} id @param {Say} say @param {() => void} reload */
async function openPage(id, say, reload) {
  const v = await fetchJSON('GET', '/api/profile/personas/' + id).catch((err) => { say(err.message); return null; });
  if (!v) return;
  render(html`<${Page} key=${id} first=${v} say=${say} reload=${reload}/>`, sheet);
  sheet.showModal();
}

function Personas() {
  const [list, setList] = useState(/** @type {LibPersona[] | null} */ (null));
  const [said, setSaid] = useState('');
  // A browser with no profile gets a 401: the section stays empty, as the rest of the page does.
  const load = () => fetchJSON('GET', '/api/profile/personas').then(setList).catch(() => setList(null));
  useEffect(() => { load(); }, []);
  // The tab counts Yours, as Starters counts its own (035-AC1, 011b-AC12).
  useEffect(() => {
    const count = document.getElementById('count-personas');
    if (count && list) count.textContent = String(list.filter((t) => t.section === 'personal' && t.mine).length);
  }, [list]);
  if (!list) return null;
  return html`<h2 id="personas-title">Personas</h2>
    <p class="hint">AI personas to add to any of your rooms, with their settings, skills and plugins. Changing one here leaves rooms alone.</p>
    <p class="said" role="status">${said}</p>
    ${sections.map(([sec, label]) => {
      const rows = list.filter((t) => t.section === sec);
      return html`<h3 key=${sec}>${label}</h3>${rows.length
        ? html`<ul class="starter-list persona-lib" id=${'personas-' + sec}>${rows.map((t) => html`<li key=${t.id} data-persona=${t.id}>
            <span class="persona-lib-badge" aria-hidden="true">${t.badge}</span>
            <div class="starter-text"><button type="button" class="persona-lib-name" onClick=${() => openPage(t.id, setSaid, load)}>${t.title}</button>
              <span class="hint">v${t.version} · by ${t.author}${t.name !== t.title ? ' · ' + t.name : ''}</span>
              ${t.description && html`<span class="hint">${t.description}</span>`}</div>
            <${RowMenu} id=${'persona-menu-' + t.id} label=${'More actions for ' + t.title} items=${actionsFor(t, setSaid, load)}/>
          </li>`)}</ul>`
        : html`<p class="empty" id=${'personas-' + sec + '-empty'}>${emptyText[sec]}</p>`}`;
    })}`;
}

render(html`<${Personas}/>`, /** @type {HTMLElement} */ (document.getElementById('personas')));
