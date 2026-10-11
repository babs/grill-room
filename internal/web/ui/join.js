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

form.addEventListener('submit', async (e) => {
  e.preventDefault();
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
