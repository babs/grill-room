'use strict';

const $ = (id) => document.getElementById(id);
const statusLine = $('status');

async function post(url, body) {
  const res = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const data = await res.json().catch(() => ({}));
  return { res, data };
}

function render({ email_enabled: emailEnabled, profile }) {
  $('signed-in').hidden = !profile;
  $('signed-out').hidden = !!profile;
  $('signin').hidden = !emailEnabled;
  if (!profile) return;
  $('rooms').replaceChildren(...profile.rooms.map((r) => {
    const li = document.createElement('li');
    const a = document.createElement('a');
    a.href = '/r/' + encodeURIComponent(r.room_id);
    a.textContent = r.room_name;
    li.append(a, ' as ' + r.name);
    return li;
  }));
  if (!profile.rooms.length) $('rooms').textContent = 'None yet.';
  // The name the profile chose in a room (005-AC1), else the one it last joined with.
  if (!$('room-create-as').value) $('room-create-as').value = profile.name || (profile.rooms[0] || {}).name || '';
  $('email').hidden = !emailEnabled;
  $('email-state').textContent = profile.email ? 'Bound to ' + profile.email + '.' : 'No email bound.';
}

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
        if (!window.confirm('Remove ' + k.name + '? Personas that have it on lose it.')) return;
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

// New room: the profile creates a room and becomes its admin, within the instance's limits (004-AC7).
$('room-new').addEventListener('click', () => {
  $('room-new').hidden = true;
  $('room-create').hidden = false;
  $('room-create-name').focus();
});
$('room-create').addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = $('room-create').querySelector('button');
  button.disabled = true;
  try {
    const { res, data } = await post('/api/profile/rooms', { name: $('room-create-name').value, display_name: $('room-create-as').value });
    if (res.ok) {
      // The new room opens on its invite card (005-AC34).
      location.assign('/r/' + encodeURIComponent(data.id) + '?invite');
      return;
    }
    $('room-create-status').textContent = data.error || 'HTTP ' + res.status;
  } catch {
    $('room-create-status').textContent = 'Network error, try again.';
  }
  button.disabled = false;
});

// confirm resolves true once the person agrees to sign this browser in with a link: a link sent by
// someone else would otherwise silently put this browser, and every room it joins next, in their
// profile.
function confirm(current) {
  $('confirm-text').textContent = (current
    ? 'This browser already holds a profile. If this link is for another one, signing in replaces it here: keep this browser\'s recovery link to get back. '
    : '') + 'Only continue if you asked for this link yourself.';
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

// The link's secret rides in the fragment: read it and drop it from the address bar. A bind link
// only binds the address to the profile this browser is in, so it is spent at once; a recovery or
// sign-in link waits for a click.
async function signInFromLink() {
  const hash = new URLSearchParams(location.hash.slice(1));
  const [kind, token] = ['bind', 'magic', 'recover'].map((k) => [k, hash.get(k)]).find(([, v]) => v) || [];
  if (!token) return;
  history.replaceState(null, '', location.pathname);
  if (kind === 'bind') {
    const { res, data } = await post('/api/profile/magic', { token, purpose: 'bind' });
    statusLine.textContent = res.ok ? 'Email bound.' : data.error || 'Binding failed.';
    return;
  }
  const res = await fetch('/api/profile');
  const current = res.ok ? (await res.json()).profile : null;
  if (!(await confirm(current))) {
    statusLine.textContent = 'Link not used.';
    return;
  }
  const { res: used, data } = kind === 'recover'
    ? await post('/api/profile/recover', { token })
    : await post('/api/profile/magic', { token, purpose: 'signin' });
  statusLine.textContent = used.ok ? 'Signed in on this device.' : data.error || 'Sign-in failed.';
}

function emailForm(form, input, url, sent) {
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const button = form.querySelector('button');
    button.disabled = true;
    try {
      const { res, data } = await post(url, { email: input.value });
      statusLine.textContent = res.ok ? sent : data.error || 'HTTP ' + res.status;
    } catch {
      statusLine.textContent = 'Network error, try again.';
    }
    button.disabled = false;
  });
}

emailForm($('bind'), $('bind-email'), '/api/profile/email', 'Check your mailbox. The link works once and expires after a few minutes.');
emailForm($('signin'), $('signin-email'), '/api/profile/signin', 'If a profile holds this address, a sign-in link is on its way. It works once and expires after a few minutes.');

function start() {
  signInFromLink().then(load).catch((err) => {
    console.error(err);
    statusLine.textContent = 'Could not load your profile.';
  });
}
// A link pasted into a tab already on this page changes only the fragment: no reload.
window.addEventListener('hashchange', start);
start();
