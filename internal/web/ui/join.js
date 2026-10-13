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
