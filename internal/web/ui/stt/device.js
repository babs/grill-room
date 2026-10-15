// On-device transcription in the audio menu (010-AC1, 010-AC2, 010-AC8): the capability check, the
// opt-in toggle, the download and the self-test. Phase 1 transcribes nothing yet: window.grillDeviceSTT
// and the "grill:device-stt" event give the device pipeline its engine.

/**
 * A message from stt/engine.js.
 * @typedef {{ev: string, id?: number, error?: string, message?: string, file?: string, backend?: string,
 *   isolated?: boolean, ms?: any, done?: number, total?: number, text?: string}} EngineMsg
 * @typedef {'off' | 'loading' | 'testing' | 'ready'} DeviceState
 */

const ON_KEY = 'grill-room-device-stt';
/** @param {string} rev */
const failKey = (rev) => 'grill-room-device-stt-failed:' + rev;
/** @param {string} rev */
const passKey = (rev) => 'grill-room-device-stt-passed:' + rev;
/** @param {string} rev */
const reasonKey = (rev) => 'grill-room-device-stt-reason:' + rev;
const CACHE = 'grill-room-stt:';
const GB = 1e9;
// The self-test's bars: decode at least this many times faster than real time, and this close to
// the reference text.
/** @type {Record<string, number>} */
const MIN_RTFX = { wasm: 3, 'webgpu-hybrid': 8 };
const MAX_WER = 0.2;
// A load that sends nothing for stallMs, or a self-test decode longer than testMs, is a hung
// engine. Progress resets the stall clock, so a slow link is not a hang. Tests shorten them.
export const timing = { stallMs: 120_000, testMs: 60_000 };

/** @param {string} id */
const $ = (id) => /** @type {HTMLElement} */ (document.getElementById(id));
const label = $('voice-device');
const box = /** @type {HTMLInputElement} */ (label.querySelector('input'));
const status = $('device-stt-status');

/** @type {DeviceOffer | null} */
let offer = null;
/** @type {Worker | null} */
let worker = null;
/** @type {DeviceState} */
let state = 'off';
// run tells the current worker's messages from a stopped one's; gen does the same for turnOn.
let run = 0;
let gen = 0;
// checked is the revision whose offer the capability check last settled, for the tests.
/** @type {string | null | undefined} */
let checked;
// started is set before the first check awaits, so a second "stt" event does not run it again.
let started = false;
// The engine runs only while this tab is in the audio (room.js sends grill:voice).
let inAudio = !!window.grillVoiceOn;

/**
 * capable is 010-AC1's check, without the storage estimate: phones and tablets never get the option.
 * @param {any} nav
 * @param {boolean} isolated
 */
export function capable(nav, isolated) {
  const desktop = nav.userAgentData ? nav.userAgentData.mobile === false
    : (nav.maxTouchPoints || 0) <= 1 && !/Mobi/.test(nav.userAgent);
  return isolated && desktop && (nav.hardwareConcurrency || 0) >= 4
    && (nav.deviceMemory === undefined || nav.deviceMemory >= 8);
}

/**
 * words normalises a text for WER: lowercase, NFC, letters and digits only.
 * @param {string} s
 */
function words(s) {
  return s.normalize('NFC').toLowerCase().replace(/[^\p{L}\p{N}]+/gu, ' ').trim().split(' ').filter(Boolean);
}

/**
 * wer is the word error rate of hyp against ref: word Levenshtein distance over the reference length.
 * @param {string} ref
 * @param {string} hyp
 */
export function wer(ref, hyp) {
  const r = words(ref);
  const h = words(hyp);
  let prev = Array.from({ length: h.length + 1 }, (_, j) => j);
  for (let i = 1; i <= r.length; i++) {
    const cur = [i];
    for (let j = 1; j <= h.length; j++) cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (r[i - 1] === h[j - 1] ? 0 : 1));
    prev = cur;
  }
  return r.length ? prev[h.length] / r.length : h.length ? 1 : 0;
}

/**
 * pcm16 reads a 16-bit PCM WAV's samples as floats.
 * @param {ArrayBuffer} buf
 */
function pcm16(buf) {
  const v = new DataView(buf);
  for (let o = 12; o + 8 <= v.byteLength; o += 8 + v.getUint32(o + 4, true)) {
    if (v.getUint32(o, false) !== 0x64617461) continue; // "data"
    const n = Math.min(v.getUint32(o + 4, true), v.byteLength - o - 8) >> 1;
    const out = new Float32Array(n);
    for (let i = 0; i < n; i++) out[i] = v.getInt16(o + 8 + 2 * i, true) / 32768;
    return out;
  }
  throw new Error('the self-test clip has no data chunk');
}

/** @param {number} n */
const mb = (n) => Math.round(n / 1e6) + ' MB';

/**
 * say puts text in the status line; an error leads with the alert icon, so it is not colour alone.
 * @param {string} text
 * @param {boolean} [error]
 */
function say(text, error = false) {
  status.classList.toggle('error', error && !!text);
  const icon = window.grillIcon('alert');
  // Not .ico: a menu with a .ico child indents every row for an icon column.
  icon.removeAttribute('class');
  status.replaceChildren(...(error && text ? [icon] : []), text);
}

/** @param {DeviceState} s */
function setState(s) {
  state = s;
  box.checked = s !== 'off';
  window.dispatchEvent(new CustomEvent('grill:device-stt', { detail: { state: s } }));
}

/** @type {Map<number, (m: EngineMsg) => void>} */
const pending = new Map();
let nextID = 0;
/** @type {((m: EngineMsg) => void) | null} */
let abort = null;
const HUNG = { ev: 'error', error: 'hung', message: 'the engine stopped responding' };

// stop ends the engine; what waited on it fails, so no caller hangs.
function stop() {
  run++;
  worker?.terminate();
  worker = null;
  const gone = { ev: 'error', error: 'stopped', message: 'device transcription stopped' };
  for (const f of pending.values()) f(gone);
  pending.clear();
  abort?.(gone);
  abort = null;
}

// pause ends the engine and keeps the choice: it loads again when the tab is back in the audio.
function pause() {
  gen++;
  stop();
  setState('off');
}

/**
 * startWorker loads the engine; it resolves with the 'ready' message or rejects with the 'error' one.
 * @param {string} [backend]
 * @returns {Promise<EngineMsg>}
 */
function startWorker(backend) {
  stop();
  const mine = run;
  const w = new Worker('/static/stt/engine.js', { type: 'module' });
  worker = w;
  let pct = -1;
  return new Promise((resolve, reject) => {
    let timer = 0;
    /** @param {EngineMsg} m */
    const fail = (m) => {
      clearTimeout(timer);
      abort = null;
      reject(m);
    };
    const arm = () => {
      clearTimeout(timer);
      timer = setTimeout(() => fail(HUNG), timing.stallMs);
    };
    abort = fail;
    arm();
    w.onmessage = (e) => {
      /** @type {EngineMsg} */
      const m = e.data;
      if (mine !== run) return;
      if (m.ev === 'progress') {
        arm();
        // The status line is a live region: a step per 10 %, not per chunk.
        const p = Math.floor(((m.done || 0) * 10) / (m.total || 1)) * 10;
        if (p !== pct) say(`Downloading: ${(pct = p)} % of ${mb(m.total || 0)}`);
      } else if (m.ev === 'ready') {
        clearTimeout(timer);
        abort = null;
        resolve(m);
      } else if (m.id !== undefined && pending.has(m.id)) {
        pending.get(m.id)?.(m);
        pending.delete(m.id);
      } else if (m.ev === 'error') fail(m);
    };
    w.onerror = (e) => {
      if (mine !== run) return;
      // Before it is ready: the script did not load, a network fault and no verdict on the device.
      if (abort) return fail({ ev: 'error', error: 'network', message: e.message || 'the engine did not load' });
      // After: the engine died; whatever waits on it fails at once.
      gen++;
      if (offer) halted(offer.revision, 'The voice-to-text engine stopped. Turn it on again to restart it.');
    };
    w.postMessage({ cmd: 'load', revision: offer?.revision, files: offer?.files, backend });
  });
}

/**
 * transcribe decodes 16 kHz mono samples in the engine.
 * @param {Float32Array} pcm
 * @returns {Promise<{text: string, ms: number}>}
 */
function transcribe(pcm) {
  return new Promise((resolve, reject) => {
    if (!worker || state === 'off') return reject(new Error('device transcription is off'));
    const id = nextID++;
    pending.set(id, (m) => (m.ev === 'text' ? resolve({ text: m.text || '', ms: m.ms }) : reject(m)));
    worker.postMessage({ cmd: 'transcribe', id, pcm }, [pcm.buffer]);
  });
}

/**
 * get fetches a self-test file; a failure is a network fault.
 * @param {string} url
 */
async function get(url) {
  try {
    const r = await fetch(url);
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    return r;
  } catch (err) {
    throw { ev: 'error', error: 'network', message: `${url}: ${err instanceof Error ? err.message : err}` };
  }
}

/**
 * selfTest decodes the bundled 10 s clip: '' on a pass, else the reason.
 * @param {string} backend
 */
async function selfTest(backend) {
  const [wav, ref] = await Promise.all([
    get('/static/stt/selftest-fr.wav').then((r) => r.arrayBuffer()),
    get('/static/stt/selftest-fr.json').then((r) => r.json()),
  ]);
  /** @type {Promise<never>} */
  const late = new Promise((_, reject) => setTimeout(() => reject(HUNG), timing.testMs));
  const { text, ms } = await Promise.race([transcribe(pcm16(wav)), late]);
  const rtfx = ref.seconds / (ms / 1000);
  const w = wer(ref.text, text);
  window.grillDeviceSTT.selfTest = { backend, rtfx, wer: w, text };
  if (rtfx < MIN_RTFX[backend]) return `This device is too slow to write voices down: ${rtfx.toFixed(1)} times real time, it needs ${MIN_RTFX[backend]}.`;
  if (w > MAX_WER) return `This device got the test sentence wrong (${Math.round(w * 100)} % of words), so it can't write voices down.`;
  return '';
}

/**
 * halted turns device mode off on an error that says nothing of the device, a download or a
 * server fault: the reason stays for the session, the option stays offered.
 * @param {string} rev
 * @param {string} text
 */
function halted(rev, text) {
  sessionStorage.setItem(reasonKey(rev), text);
  stop();
  localStorage.removeItem(ON_KEY);
  setState('off');
  say(text, true);
}

/**
 * refuse turns the option off for this model revision on this device (010-AC2).
 * @param {string} rev
 * @param {string} reason
 */
async function refuse(rev, reason) {
  localStorage.setItem(failKey(rev), '1');
  halted(rev, reason);
  // The checkbox is going away: keyboard focus moves to the reason instead of the page's top.
  if (document.activeElement === box) status.focus();
  label.hidden = true;
  await caches.delete(CACHE + rev).catch(() => false);
}

async function turnOn() {
  if (!offer) return;
  const rev = offer.revision;
  const mine = ++gen;
  sessionStorage.removeItem(reasonKey(rev));
  setState('loading');
  say('Downloading…');
  // A device that passed loads the backend it passed on and skips the test. Otherwise a failure
  // on WebGPU gets a second chance on WASM before the device is refused (010-AC2). Only the
  // test's verdict, or an engine that runs on neither, refuses the device; a fetch error does not.
  const passed = localStorage.getItem(passKey(rev));
  let reason = '';
  let ready = /** @type {EngineMsg} */ ({ ev: '' });
  for (const backend of passed ? [passed] : [undefined, 'wasm']) {
    let used = backend;
    try {
      ready = await startWorker(backend);
      used = ready.backend;
      if (mine !== gen) return;
      window.grillDeviceSTT.load = { ...ready.ms, backend: ready.backend, isolated: ready.isolated };
      if (passed) break;
      navigator.storage.persist?.().catch(() => false);
      setState('testing');
      say(backend ? 'Testing this device again, without the graphics card…' : 'Testing this device…');
      reason = await selfTest(used || 'wasm');
    } catch (err) {
      if (mine !== gen) return;
      const m = /** @type {EngineMsg} */ (err);
      if (m.error === 'integrity') return halted(rev, `A downloaded file (${m.file}) did not match its checksum, so nothing was kept. Try again later.`);
      if (m.error === 'hung') return halted(rev, 'The voice-to-text engine stopped responding. Try again later.');
      if (m.error !== 'engine') return halted(rev, 'The download stopped. Check your connection and try again.');
      used ||= m.backend;
      reason = `This device can't run the voice-to-text engine: ${m.message}`;
    }
    if (mine !== gen) return;
    if (!reason || used === 'wasm') break;
  }
  if (reason && passed) {
    // The next try tests again, WebGPU then WASM, instead of trusting an old pass.
    localStorage.removeItem(passKey(rev));
    return halted(rev, reason);
  }
  if (reason) return refuse(rev, reason);
  if (!passed) localStorage.setItem(passKey(rev), ready.backend || 'wasm');
  setState('ready');
  say('Ready: this device can write your voice down.');
}

function turnOff() {
  pause();
  localStorage.removeItem(ON_KEY);
  idle();
}

// idle shows what turning it on costs, or the reason it stopped this session.
function idle() {
  const reason = offer && sessionStorage.getItem(reasonKey(offer.revision));
  if (reason) return say(reason, true);
  say(offer && !label.hidden ? `Downloads ${mb(offer.bytes)} once` : '');
}

/** @param {DeviceOffer | null | undefined} next */
async function offered(next) {
  if (started && next?.revision === offer?.revision) return;
  started = true;
  // A new offer starts over, keeping the choice: the new revision loads, and tests, on its own.
  if (state !== 'off') pause();
  offer = next || null;
  const rev = offer?.revision;
  // Only the offered revision's files are worth their space, the toggle on or off.
  if (rev) caches.keys().then((ks) => Promise.all(ks.filter((k) => k.startsWith(CACHE) && k !== CACHE + rev).map((k) => caches.delete(k)))).catch(() => {});
  let ok = !!rev && capable(navigator, crossOriginIsolated) && !localStorage.getItem(failKey(rev));
  if (ok) {
    const est = await navigator.storage?.estimate?.().catch(() => null);
    ok = !!est && (est.quota || 0) - (est.usage || 0) >= GB;
  }
  if (rev !== offer?.revision) return;
  label.hidden = !ok;
  checked = rev ?? null;
  idle();
  if (ok && inAudio && localStorage.getItem(ON_KEY)) turnOn();
}

/** @param {boolean} on */
function voiceMoved(on) {
  inAudio = on;
  if (!on && state !== 'off') {
    pause();
    idle();
  } else if (on && state === 'off' && offer && !label.hidden && localStorage.getItem(ON_KEY)) turnOn();
}

box.addEventListener('change', () => {
  if (box.checked) {
    localStorage.setItem(ON_KEY, '1');
    // The toggle sits in the audio menu, so this holds; the engine never runs outside the audio.
    if (inAudio) turnOn();
  } else turnOff();
});

window.grillDeviceSTT = { get state() { return state; }, get checked() { return checked; }, transcribe };
addEventListener('grill:stt', (e) => offered(/** @type {CustomEvent} */ (e).detail.device));
addEventListener('grill:voice', (e) => voiceMoved(/** @type {CustomEvent} */ (e).detail.on));
offered(window.grillSTT?.device);
