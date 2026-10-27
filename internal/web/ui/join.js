'use strict';

const form = document.getElementById('join');
const nameInput = document.getElementById('name');
const statusLine = document.getElementById('status');
const invite = location.hash.slice(1);
const noInvite = 'You need an invite link to join a room. Ask the room host for one.';

if (invite) form.hidden = false;

// Set once /api/profile tells whether the create form shows instead, so it does not flash (019-AC8).
// Rewriting the same text on every focus would make screen readers announce it again.
function noInviteLine(on) {
  const line = on ? noInvite : '';
  if (!invite && statusLine.textContent !== line) statusLine.textContent = line;
}

// The browser's own tooltip does not show on every phone and reads as an error page: the reason
// goes under the field (005-AC44).
const nameError = document.getElementById('name-error');
nameInput.addEventListener('input', () => {
  nameError.textContent = '';
  nameInput.removeAttribute('aria-invalid');
});

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!nameInput.value.trim()) {
    nameError.textContent = 'Type the name the others will see, up to 40 characters.';
    nameInput.setAttribute('aria-invalid', 'true');
    nameInput.focus();
    return;
  }
  const button = form.querySelector('button');
  button.disabled = true;
  statusLine.textContent = '';
  try {
    const res = await fetch('/api/join', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ invite, name: nameInput.value }),
    });
    const data = await res.json();
    if (!res.ok) {
      statusLine.textContent = res.status === 401 ? 'This invite link is invalid or was revoked.' : data.error;
      button.disabled = false;
      return;
    }
    // Shown once by the room page, which removes it on read.
    if (data.recovery_url) sessionStorage.setItem('grill_room_recovery', data.recovery_url);
    location.assign('/r/' + encodeURIComponent(data.participant.room_id));
  } catch {
    statusLine.textContent = 'Network error, try again.';
    button.disabled = false;
  }
});

// With no invite and no profile, an instance that allows it offers a room of one's own (019-AC8).
const createForm = document.getElementById('create');
const createError = document.getElementById('create-error');
const createFields = [
  [document.getElementById('room-name'), document.getElementById('room-name-error'), 'Type a name for the room, up to 100 characters.'],
  [document.getElementById('display-name'), document.getElementById('display-name-error'), 'Type the name the others will see, up to 40 characters.'],
];
for (const [input, error] of createFields) {
  input.addEventListener('input', () => {
    error.textContent = '';
    input.removeAttribute('aria-invalid');
  });
}

createForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  createError.textContent = '';
  const empty = createFields.filter(([input]) => !input.value.trim());
  for (const [input, error, msg] of empty) {
    error.textContent = msg;
    input.setAttribute('aria-invalid', 'true');
  }
  if (empty.length) {
    empty[0][0].focus();
    return;
  }
  const button = createForm.querySelector('button');
  button.disabled = true;
  try {
    const res = await fetch('/api/rooms/anonymous', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: createFields[0][0].value, display_name: createFields[1][0].value }),
    });
    // A plain-text 404 (the flag turned off meanwhile) or a proxy's HTML page is no network error.
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      createError.textContent = data.error || 'Room creation is unavailable right now.';
      button.disabled = false;
      return;
    }
    // Shown once by the room page, as after a join.
    if (data.recovery_url) sessionStorage.setItem('grill_room_recovery', data.recovery_url);
    location.assign('/r/' + encodeURIComponent(data.id));
  } catch {
    createError.textContent = 'Network error, try again.';
    button.disabled = false;
  }
});

// Who this browser is (011-AC25): signed in, a link to its profile; signed out with email set up,
// the email recovery (011-AC4). A sign-in from a mailed link in another tab shows here once this
// tab is back in front (011-AC5), and the join above then joins as that profile: it rides on the
// cookie.
const yourProfile = document.getElementById('your-profile');
const recover = document.getElementById('recover');
const recoverOpen = document.getElementById('recover-open');
const recoverForm = document.getElementById('recover-form');

// ttl is the emailed link's lifetime in seconds, as the server's mail words it.
let ttl = 0;
async function who() {
  const res = await fetch('/api/profile');
  if (!res.ok) {
    noInviteLine(createForm.hidden);
    return;
  }
  const { email_enabled: emailEnabled, profile, magic_link_ttl_s: linkTTL, anonymous_rooms: anonymousRooms } = await res.json();
  ttl = linkTTL || 0;
  yourProfile.hidden = !profile;
  // The name the profile chose (005-AC1, 005-AC48), else the one it last joined with.
  const name = profile ? profile.name || (profile.rooms[0] || {}).name || '' : '';
  document.getElementById('your-profile-name').textContent = name;
  // Never over what the person typed: this runs again each time the tab comes back.
  if (!nameInput.value) nameInput.value = name;
  recover.hidden = !!profile || !emailEnabled;
  // A profile made in another tab hides the form: its rooms are created from /profile (019-AC3).
  if (!invite) createForm.hidden = !anonymousRooms || !!profile;
  noInviteLine(createForm.hidden);
}
// A failed check keeps what shows; with no form up, the invite line still explains the page.
function whoOrLine() {
  who().catch((err) => {
    noInviteLine(createForm.hidden);
    console.error(err);
  });
}
whoOrLine();
addEventListener('focus', whoOrLine);
document.addEventListener('visibilitychange', () => { if (!document.hidden) whoOrLine(); });

recoverOpen.addEventListener('click', () => {
  recoverForm.hidden = false;
  recoverOpen.hidden = true;
  recoverOpen.setAttribute('aria-expanded', 'true');
  document.getElementById('recover-email').focus();
});

// The answer is the same whether or not a profile holds the address (011-AC22), in place (011-AC28).
window.grillLinkForm({ form: recoverForm, url: '/api/profile/signin', kind: 'signin', ttl: () => ttl });
