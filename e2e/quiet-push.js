// The push explainer offers itself after a participant's first line (008-AC11), as a modal sheet
// that would stop any spec typing in the chat. Contexts start with the offer snoozed; the
// explainer's own spec clears it.
export const snoozeKey = 'grill-room-push-snooze';
const until = String(Number.MAX_SAFE_INTEGER);

// quietPush is a storageState for a config's `use`, for each origin its specs open room pages on:
// browser.newContext() inherits it too. Every config that drives a room page sets it.
export const quietPush = (...origins) => ({ cookies: [], origins: origins.map((origin) => ({ origin, localStorage: [{ name: snoozeKey, value: until }] })) });

// snoozePush is an init script for contexts from a browser launched by hand:
// ctx.addInitScript(snoozePush, snoozeKey).
export function snoozePush(key) {
  if (!localStorage.getItem(key)) localStorage.setItem(key, String(Number.MAX_SAFE_INTEGER));
}
