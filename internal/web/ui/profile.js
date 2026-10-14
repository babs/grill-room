'use strict';

const $ = (id) => document.getElementById(id);
const statusLine = $('status');

async function post(url, body) {
  const res = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const data = await res.json().catch(() => ({}));
  return { res, data };
}

// ttl is the emailed link's lifetime in seconds, as the server's mail words it.
let ttl = 0;
function render({ email_enabled: emailEnabled, profile, magic_link_ttl_s: linkTTL }) {
  ttl = linkTTL || 0;
  $('signed-in').hidden = !profile;
  $('signed-out').hidden = !!profile;
  $('signin').hidden = !emailEnabled;
  if (!profile) return;
  // Each room is a card: its name, your name there, its people and last activity (005-AC41).
  $('rooms').replaceChildren(...profile.rooms.map((r) => {
    const li = document.createElement('li');
    li.className = 'card';
    const line = document.createElement('p');
    line.className = 'room-line';
    const a = document.createElement('a');
    a.href = '/r/' + encodeURIComponent(r.room_id);
    a.textContent = r.room_name;
    line.append(a, ' as ' + r.name);
    const meta = document.createElement('p');
    meta.className = 'hint';
    const when = document.createElement('time');
    when.dateTime = r.last_activity;
    when.title = new Date(r.last_activity).toLocaleString();
    when.textContent = ago(r.last_activity);
    meta.append((r.people === 1 ? '1 person' : r.people + ' people') + ' · active ', when);
    li.append(line, meta);
    if (r.admin) {
      const badge = document.createElement('span');
      badge.className = 'badge role';
      badge.textContent = 'admin';
      badge.title = 'You are a room admin here';
      li.append(badge);
    }
    return li;
  }));
  if (!profile.rooms.length) {
    const empty = document.createElement('li');
    empty.className = 'empty';
    empty.textContent = 'No room yet. Create one, or open an invite link.';
    $('rooms').append(empty);
  }
  // The name the profile chose in a room (005-AC1), else the one it last joined with.
  const name = profile.name || (profile.rooms[0] || {}).name || '';
  $('who').textContent = name;
  $('profile-name').textContent = name || 'Not set: it comes from the name you join rooms with.';
  $('email').hidden = !emailEnabled;
  $('profile-anon').hidden = !emailEnabled || !!profile.email;
  shown = profile;
  pending = profile.pending_email || '';
  showEmail(false);
  // The recovery link lives in the tab that joined, until saved: the server keeps only its hash.
  const link = sessionStorage.getItem('grill_room_recovery');
  $('device').textContent = link ? 'Open your rooms, then your initials › "Use on another device" to copy the link.'
    : emailEnabled ? 'Add an email above, then sign in with it on the other device.' : 'Open your recovery link on the other device.';
}

// The Email section's states (011-AC21): none, a link pending at an address, or an address on
// the profile. editing opens the address field over the pending and bound states.
/** @type {{email?: string} | null} */
let shown = null;
let pending = '';
function showEmail(editing) {
  const email = shown ? shown.email || '' : '';
  const state = pending ? 'pending' : email ? 'bound' : 'none';
  $('email-state').textContent = state === 'pending' ? 'Check your inbox at ' + pending
    : state === 'bound' ? email : 'No email on this profile yet.';
  $('email-pending').hidden = state !== 'pending';
  $('email-change').hidden = state !== 'bound' || editing;
  $('bind').hidden = state !== 'none' && !editing;
  $('bind-label').textContent = state === 'none' ? 'Add an email' : 'New address';
}

// openEmailField is the next step of a link that failed: a field to ask for a new one.
function openEmailField() {
  const signedIn = !$('signed-in').hidden;
  if (signedIn) showEmail(true);
  const input = signedIn ? $('bind-email') : $('signin-email');
  input.scrollIntoView({ block: 'center' });
  input.focus();
}

// ago says how long ago t was, coarsely (kit/ui.js); the full date is the tooltip.
const ago = (t) => window.grillAgo(t);

async function load() {
  const res = await fetch('/api/profile');
  if (!res.ok) throw new Error('profile: HTTP ' + res.status);
  const data = await res.json();
  render(data);
  if (data.profile) await loadSkills();
}

// Skills: the profile's personal skills and the shared ones it published (002-AC29, 002-AC30).
async function loadSkills() {
  const res = await fetch('/api/profile/skills');
  if (!res.ok) return;
  const skills = await res.json();
  $('skill-list').replaceChildren(...skills.map((k) => {
    const li = document.createElement('li');
    li.dataset.skill = k.name;
    const name = document.createElement('strong');
    name.textContent = k.name;
    li.append(name, ' v' + k.version + ' · ' + (k.shared ? 'shared' : 'personal') + ' · ' + k.description);
    if (!k.shared) {
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = 'Remove';
      b.setAttribute('aria-label', 'Remove skill ' + k.name);
      b.addEventListener('click', async () => {
        if (!(await window.grillConfirm('Remove ' + k.name + '?', 'Personas that have it on lose it.', 'Remove'))) return;
        const r = await fetch('/api/profile/skills/' + k.id, { method: 'DELETE' });
        $('skill-status').textContent = r.ok ? k.name + ' removed.' : ((await r.json().catch(() => ({}))).error || 'HTTP ' + r.status);
        await loadSkills();
      });
      li.append(' ', b);
    }
    return li;
  }));
  if (!skills.length) $('skill-list').textContent = 'None yet.';
}

$('skill-upload').addEventListener('submit', async (e) => {
  e.preventDefault();
  const file = $('skill-file').files[0];
  if (!file) return;
  const button = $('skill-upload').querySelector('button');
  button.disabled = true;
  try {
    const res = await fetch('/api/profile/skills?scope=' + $('skill-scope').value,
      { method: 'POST', headers: { 'Content-Type': 'application/zip' }, body: file });
    const data = await res.json().catch(() => ({}));
    $('skill-status').textContent = res.ok ? 'Uploaded ' + data.name + ' v' + data.version + '.' : data.error || 'HTTP ' + res.status;
    if (res.ok) $('skill-upload').reset();
    await loadSkills();
  } catch {
    $('skill-status').textContent = 'Network error, try again.';
  }
  button.disabled = false;
});

// ask shows the confirm card and resolves true once the person agrees: a link sent by someone else
// would otherwise silently put this browser, or an address, in their profile.
function ask(text, yes) {
  $('confirm-text').textContent = text;
  $('confirm-yes').textContent = yes;
  $('confirm').hidden = false;
  return new Promise((resolve) => {
    const done = (ok) => () => {
      $('confirm').hidden = true;
      $('confirm-yes').onclick = $('confirm-no').onclick = null;
      resolve(ok);
    };
    $('confirm-yes').onclick = done(true);
    $('confirm-no').onclick = done(false);
  });
}

const confirmSignIn = (current) => ask((current
  ? 'This browser already holds a profile. If this link is for another one, signing in replaces it here: keep this browser\'s recovery link to get back. '
  : '') + 'Only continue if you asked for this link yourself.', 'Sign in to this profile');

// next shows the step after a link's outcome, or none.
function next(label, run) {
  $('next').hidden = !label;
  $('next-step').textContent = label || '';
  $('next-step').onclick = run || null;
}

// failed says what happened to a link that did not work, and what to do next (011-AC23).
function failed(data, signedIn) {
  switch (data.reason) {
    case 'expired':
      statusLine.textContent = 'This link has expired.';
      return next('Send a new link', openEmailField);
    case 'used':
      statusLine.textContent = 'This link was already used.' + (signedIn ? ' Your profile is below.' : '');
      return signedIn ? next('') : next('Sign in by email', openEmailField);
    case 'taken':
      statusLine.textContent = 'This address is already on another profile. Sign in with it to open that profile, or ask the admin to merge the two profiles.';
      return next('');
  }
  statusLine.textContent = data.error || 'This link did not work.';
  next('');
}

// The link's secret rides in the fragment: read it and drop it from the address bar. A bind link
// adds the address to the profile that asked for it: at once in that browser, after a confirmation
// naming the profile anywhere else (011-AC23). A recovery or sign-in link waits for a click.
async function signInFromLink() {
  const hash = new URLSearchParams(location.hash.slice(1));
  const [kind, token] = ['bind', 'magic', 'recover'].map((k) => [k, hash.get(k)]).find(([, v]) => v) || [];
  if (!token) return;
  history.replaceState(null, '', location.pathname);
  next('');
  const res = await fetch('/api/profile');
  const current = res.ok ? (await res.json()).profile : null;
  if (kind === 'bind') {
    let { res: bound, data } = await post('/api/profile/magic', { token, purpose: 'bind' });
    if (bound.status === 409 && data.reason === 'confirm') {
      if (!(await ask('This link adds ' + data.email + ' to ' + data.profile_name + '\'s profile. This browser stays as it is. Only continue if you asked for this link yourself.', 'Add the email'))) {
        statusLine.textContent = 'Link not used.';
        return;
      }
      ({ res: bound, data } = await post('/api/profile/magic', { token, purpose: 'bind', confirm: true }));
      if (bound.ok) {
        statusLine.textContent = 'Email added to ' + data.profile_name + '\'s profile.';
        return;
      }
    }
    if (bound.ok) statusLine.textContent = 'Email added to your profile: ' + data.profile.email + '.';
    else failed(data, !!current);
    return;
  }
  if (!(await confirmSignIn(current))) {
    statusLine.textContent = 'Link not used.';
    return;
  }
  const { res: used, data } = kind === 'recover'
    ? await post('/api/profile/recover', { token })
    : await post('/api/profile/magic', { token, purpose: 'signin' });
  if (!used.ok) return failed(data, !!current);
  const name = data.profile && (data.profile.name || (data.profile.rooms[0] || {}).name);
  statusLine.textContent = kind === 'magic' && name ? 'Signed in as ' + name + ' on this device.' : 'Signed in on this device.';
}

// sendLink asks for a link to email; on success the button turns into Resend for a minute (011-AC21, 011-AC22).
async function sendLink(button, url, email, sent) {
  button.disabled = true;
  try {
    const { res, data } = await post(url, { email });
    if (res.ok) {
      sent();
      button.textContent = 'Resend';
      window.grillCooldown(button, 60);
      return true;
    }
    statusLine.textContent = data.error || 'HTTP ' + res.status;
  } catch {
    statusLine.textContent = 'Network error, try again.';
  }
  button.disabled = false;
  return false;
}

$('bind').addEventListener('submit', async (e) => {
  e.preventDefault();
  const email = $('bind-email').value;
  const button = $('bind').querySelector('button');
  // The form's own button stays "Send me a link": the cooldown goes to Resend, which takes over.
  button.disabled = true;
  const { res, data } = await post('/api/profile/email', { email }).catch(() => ({ res: null, data: {} }));
  button.disabled = false;
  if (!res || !res.ok) {
    statusLine.textContent = res ? data.error || 'HTTP ' + res.status : 'Network error, try again.';
    return;
  }
  statusLine.textContent = '';
  // The server stores the link after its answer: a reload now could still miss it.
  pending = email;
  $('bind').reset();
  showEmail(false);
  $('email-resend').textContent = 'Resend';
  window.grillCooldown($('email-resend'), 60);
});
$('email-resend').addEventListener('click', () => sendLink($('email-resend'), '/api/profile/email', pending, () => { statusLine.textContent = ''; }));
$('email-other').addEventListener('click', () => {
  pending = '';
  showEmail(true);
  $('bind-email').focus();
});
$('email-change').addEventListener('click', () => {
  showEmail(true);
  $('bind-email').focus();
});
$('signin').addEventListener('submit', (e) => {
  e.preventDefault();
  sendLink($('signin').querySelector('button'), '/api/profile/signin', $('signin-email').value, () => {
    statusLine.textContent = 'If a profile uses this address, a link is on its way (valid ' + window.grillDuration(ttl) + ').';
  });
});

function start() {
  signInFromLink().then(load).then(toEmail).catch((err) => {
    console.error(err);
    statusLine.textContent = 'Could not load your profile.';
  });
}
// /profile#email comes from the room's marker and nudge (011-AC7): straight to the field.
function toEmail() {
  if (location.hash !== '#email' || $('email').hidden) return;
  history.replaceState(null, '', location.pathname);
  openEmailField();
}
// A link pasted into a tab already on this page changes only the fragment: no reload.
window.addEventListener('hashchange', () => (location.hash === '#email' ? toEmail() : start()));
start();
