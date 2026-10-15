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
let emailEnabled = false;
function render({ email_enabled: enabled, profile, magic_link_ttl_s: linkTTL }) {
  ttl = linkTTL || 0;
  emailEnabled = !!enabled;
  $('signed-in').hidden = !profile;
  $('signed-out').hidden = !!profile;
  $('signin').hidden = !emailEnabled;
  $('signed-out-text').textContent = 'You\'re not signed in on this browser. Open an invite link to join a room, or '
    + (emailEnabled ? 'sign in below.' : 'open your recovery link here.');
  if (!profile) return;
  // The name the profile chose in a room (005-AC1), else the one it last joined with.
  const name = profile.name || (profile.rooms[0] || {}).name || '';
  // Each room is a card: its name, its people and last activity (005-AC41); the admin badge leads
  // the meta line, where a long name cannot run under it, and your name there shows only when it
  // differs from the profile's.
  $('rooms').replaceChildren(...profile.rooms.map((r) => {
    const li = document.createElement('li');
    li.className = 'card';
    const line = document.createElement('p');
    line.className = 'room-line';
    const a = document.createElement('a');
    a.href = '/r/' + encodeURIComponent(r.room_id);
    a.textContent = r.room_name;
    line.append(a);
    if (r.name !== name) line.append(' as ' + r.name);
    const meta = document.createElement('p');
    meta.className = 'hint';
    if (r.admin) {
      const badge = document.createElement('span');
      badge.className = 'badge role';
      badge.textContent = 'Admin';
      badge.dataset.tip = 'You are a room admin here';
      meta.append(badge, ' · ');
    }
    const when = document.createElement('time');
    when.dateTime = r.last_activity;
    when.dataset.tip = grillDate(r.last_activity);
    when.textContent = ago(r.last_activity);
    meta.append((r.people === 1 ? '1 person' : r.people + ' people') + ' · active ', when);
    li.append(line, meta);
    return li;
  }));
  if (!profile.rooms.length) {
    const empty = document.createElement('li');
    empty.className = 'empty';
    empty.textContent = 'No room yet. Create one, or open an invite link.';
    $('rooms').append(empty);
  }
  $('who').textContent = name;
  $('profile-name').textContent = name || 'Set when you join a room.';
  $('email').hidden = !emailEnabled;
  $('profile-anon').hidden = !emailEnabled || !!profile.email;
  shown = profile;
  pending = profile.pending_email || '';
  // Seconds on the server clock, anchored to this one on arrival: a wrong client clock does not matter.
  const at = Date.now();
  pendingExpires = profile.pending_ttl_s != null ? at + profile.pending_ttl_s * 1000 : 0;
  pendingSent = profile.pending_age_s != null ? at - profile.pending_age_s * 1000 : 0;
  showEmail(false);
}

// deviceText says how to reach this profile from another device, from the Email state. The
// recovery link lives in the tab that joined, until saved: the server keeps only its hash.
function deviceText(email) {
  if (emailEnabled && email) return 'Sign in with ' + email + ' on the other device.';
  if (sessionStorage.getItem('grill_room_recovery')) return 'Open your rooms, then your initials › "Use on another device" to copy the link.';
  if (!emailEnabled) return 'Open your recovery link on the other device.';
  return pending ? 'Confirm the link we sent, then sign in on the other device.' : 'Add an email, then sign in with it on the other device.';
}

// The Email section's states (011-AC21): none, a link pending at an address (the sent panel,
// 011-AC28), or an address on the profile. editing opens the address field over the other states.
/** @type {{email?: string} | null} */
let shown = null;
let pending = '';
// When the pending link expires and was sent, in local ms: a reload shows what is left of both.
let pendingExpires = 0;
let pendingSent = 0;
function showEmail(editing) {
  const email = shown ? shown.email || '' : '';
  $('email-state').textContent = email || (pending && !editing ? '' : 'No email yet. Add one to sign in on other devices.');
  $('device').textContent = deviceText(email);
  if (pending && !editing) {
    const rest = pendingExpires ? Math.max(1, Math.ceil((pendingExpires - Date.now()) / 1000)) : ttl;
    // Whole minutes, as the mail words it, unless under one.
    const left = rest < 60 ? rest : Math.ceil(rest / 60) * 60;
    const since = pendingSent ? (Date.now() - pendingSent) / 1000 : 60;
    bindLink.show(pending, left, since < 60 ? Math.ceil(60 - since) : 0);
  } else {
    bindLink.reset();
    $('bind').hidden = !!email && !editing;
  }
  $('email-change').hidden = !email || editing || !!pending;
  $('bind-label').textContent = email ? 'New address' : 'Add an email';
}

// openEmailField is the next step of a link that failed: a field to ask for a new one.
function openEmailField() {
  const signedIn = !$('signed-in').hidden;
  if (signedIn) showEmail(true);
  else signinLink.reset();
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
    li.append(name, ' v' + k.version + ' · ' + (k.shared ? 'Everyone on this server' : 'Only my rooms') + ' · ' + k.description);
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
  if (!skills.length) {
    const empty = document.createElement('li');
    empty.className = 'empty';
    const p = document.createElement('p');
    p.textContent = 'No skills yet. Add a .zip to give your AI personas a new ability.';
    const add = document.createElement('button');
    add.type = 'button';
    add.className = 'btn-secondary';
    add.textContent = 'Add a skill';
    add.addEventListener('click', () => $('skill-file').click());
    empty.append(p, add);
    $('skill-list').append(empty);
  }
}

// The file input stays hidden: the native one cannot take the kit's button style.
$('skill-pick').addEventListener('click', () => $('skill-file').click());
$('skill-file').addEventListener('change', () => {
  const file = $('skill-file').files[0];
  $('skill-file-name').textContent = file ? file.name : 'No file chosen';
});

$('skill-upload').addEventListener('submit', async (e) => {
  e.preventDefault();
  const file = $('skill-file').files[0];
  if (!file) {
    $('skill-status').textContent = 'Choose a .zip first.';
    $('skill-pick').focus();
    return;
  }
  const button = $('skill-upload').querySelector('button[type=submit]');
  button.disabled = true;
  try {
    const res = await fetch('/api/profile/skills?scope=' + $('skill-scope').value,
      { method: 'POST', headers: { 'Content-Type': 'application/zip' }, body: file });
    const data = await res.json().catch(() => ({}));
    $('skill-status').textContent = res.ok ? 'Added ' + data.name + ' v' + data.version + '.' : data.error || 'HTTP ' + res.status;
    if (res.ok) {
      $('skill-upload').reset();
      $('skill-file-name').textContent = 'No file chosen';
    }
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

// Both email forms answer in place (011-AC28). A bind link sent is the pending state.
const bindLink = window.grillLinkForm({ form: $('bind'), url: '/api/profile/email', kind: 'bind', ttl: () => ttl,
  onSent: (email) => {
    pending = email;
    pendingExpires = pendingSent = 0;
    $('device').textContent = deviceText(shown && shown.email);
    $('email-change').hidden = true;
    if (!(shown && shown.email)) $('email-state').textContent = '';
  },
  onOther: () => {
    pending = '';
    showEmail(true);
  } });
const signinLink = window.grillLinkForm({ form: $('signin'), url: '/api/profile/signin', kind: 'signin', ttl: () => ttl });
$('email-change').addEventListener('click', () => {
  showEmail(true);
  $('bind-email').focus();
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
