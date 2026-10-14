'use strict';

const form = document.getElementById('join');
const nameInput = document.getElementById('name');
const statusLine = document.getElementById('status');
const invite = location.hash.slice(1);

if (invite) {
  form.hidden = false;
} else {
  statusLine.textContent = 'You need an invite link to join a room. Ask the room host for one.';
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

// Who this browser is (011-AC25): signed in, a link to its profile; signed out with email set up,
// the email recovery (011-AC4). A sign-in from a mailed link in another tab shows here once this
// tab is back in front (011-AC5), and the join above then joins as that profile: it rides on the
// cookie.
const yourProfile = document.getElementById('your-profile');
const recover = document.getElementById('recover');
const recoverOpen = document.getElementById('recover-open');
const recoverForm = document.getElementById('recover-form');
const recoverStatus = document.getElementById('recover-status');

// ttl is the emailed link's lifetime in seconds, as the server's mail words it.
let ttl = 0;
async function who() {
  const res = await fetch('/api/profile');
  if (!res.ok) return;
  const { email_enabled: emailEnabled, profile, magic_link_ttl_s: linkTTL } = await res.json();
  ttl = linkTTL || 0;
  yourProfile.hidden = !profile;
  document.getElementById('your-profile-name').textContent = profile ? profile.name || (profile.rooms[0] || {}).name || '' : '';
  recover.hidden = !!profile || !emailEnabled;
}
who().catch(console.error);
addEventListener('focus', () => who().catch(console.error));
document.addEventListener('visibilitychange', () => { if (!document.hidden) who().catch(console.error); });

recoverOpen.addEventListener('click', () => {
  recoverForm.hidden = false;
  recoverOpen.hidden = true;
  recoverOpen.setAttribute('aria-expanded', 'true');
  document.getElementById('recover-email').focus();
});

// The answer is the same whether or not a profile holds the address (011-AC22).
recoverForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = recoverForm.querySelector('button');
  button.disabled = true;
  try {
    const res = await fetch('/api/profile/signin', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: document.getElementById('recover-email').value }),
    });
    if (res.ok) {
      recoverStatus.textContent = 'If a profile uses this address, a link is on its way (valid ' + window.grillDuration(ttl) + ').';
      button.textContent = 'Resend';
      window.grillCooldown(button, 60);
      return;
    }
    recoverStatus.textContent = (await res.json().catch(() => ({}))).error || 'HTTP ' + res.status;
  } catch {
    recoverStatus.textContent = 'Network error, try again.';
  }
  button.disabled = false;
});
