'use strict';

// An internal whose organisation sign-in expired gets 401 reason "reauth" from any call: the page
// goes through the provider, silently first, and comes back here (021-AC8). Loaded before the
// page's scripts, it wraps fetch once for all of them.
(() => {
  const KEY = 'grill_room_reauth_at';
  // The fragment holds secrets (an invite, a link's token): it waits here, never in a URL a proxy
  // logs, and comes back once the sign-in lands on the same path, within the sign-in's 10 minutes.
  const HASH_KEY = 'grill_room_reauth_hash';
  const HASH_TTL_MS = 600_000;
  const here = () => location.pathname + location.search;
  const saved = JSON.parse(sessionStorage.getItem(HASH_KEY) || 'null');
  sessionStorage.removeItem(HASH_KEY);
  if (saved && saved.exp > Date.now() && saved.at === here() && !location.hash) history.replaceState(null, '', saved.at + saved.hash);
  // A sign-in that came back still expired, another account at the provider, must not loop.
  const GUARD_MS = 60_000;
  // grillReauth goes, or returns false when the guard holds it back; true again once going, as
  // every failing call of the page asks.
  let going = false;
  const signIn = (/** @type {string} */ prompt) => {
    if (location.hash) sessionStorage.setItem(HASH_KEY, JSON.stringify({ at: here(), hash: location.hash, exp: Date.now() + HASH_TTL_MS }));
    location.assign('/auth/login?' + prompt + 'next=' + encodeURIComponent(here()));
  };
  window.grillReauth = () => {
    if (going) return true;
    if (Date.now() - Number(sessionStorage.getItem(KEY) || 0) < GUARD_MS) return false;
    going = true;
    sessionStorage.setItem(KEY, String(Date.now()));
    signIn('prompt=none&');
    return true;
  };
  // A sign-in asked for on the page, as the join page's button, comes back to it the same way (021-AC12).
  window.grillSignIn = () => signIn('');
  const plain = window.fetch.bind(window);
  window.fetch = async (input, init) => {
    const res = await plain(input, init);
    // Awaited, so the redirect reads the address before the caller can change it.
    if (res.status === 401 && (await res.clone().json().catch(() => null))?.reason === 'reauth') window.grillReauth();
    return res;
  };
})();
