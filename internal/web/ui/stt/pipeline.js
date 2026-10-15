// The device pipeline, a module worker: Silero v6 and the segmenter on the mic's windows, each
// utterance transcribed once by the engine worker, the plausible lines posted under the
// participant's name (010-AC9), the device beat (010-AC13) and the fallback (010-AC17). The beat
// comes from here, while the tapped mic sends audio or the mic is muted: a stalled pipeline or a
// dead tap stops the beats and the flag lapses, whatever the main thread's timers do.
// In: {cmd: 'start', base, rate, engine: MessagePort, mic: MessagePort, presence, lines, tab}, {cmd:
// 'muted', on}, {cmd: 'stop'}. Out: {ev: 'stats', ...counters}, {ev: 'mode', mode, error?}, {ev: 'stopped'}.
import { Segmenter, SR, WINDOW } from './segmenter.js';
import { Resampler } from './resample.js';
import { implausible } from './filter.js';
import { Policy, deadline, shed } from './policy.js';

// Samples of the previous window Silero v6 expects in front of each one.
const CTX = 64;
const BEAT_MS = 1000;
const BEAT_TIMEOUT_MS = 2_000;
const POST_TIMEOUT_MS = 10_000;
// A stop waits this long for the lines still to post; stt/device.js ends the worker 1 s later.
export const STOP_MS = 4000;
const KEEP_LATENCIES = 50;

/**
 * @typedef {import('./segmenter.js').Utterance & {post: boolean}} Queued
 * @typedef {{base: string, rate: number, engine: MessagePort, mic: MessagePort, presence: string, lines: string, tab: string}} Start
 */

const seg = new Segmenter();
const policy = new Policy(() => performance.now());
// The counters the page and the tests read; mode is 'loading' until Silero runs.
const stats = {
  windows: 0, utterances: 0, decoded: 0, posted: 0, dropped: 0, lost: 0, beats: 0, ticks: 0, queue: 0,
  /** @type {'loading' | import('./policy.js').Mode} */
  mode: 'loading',
  /** @type {number[]} */
  latencies: [],
};
/** @type {Queued[]} */
const queue = [];
/** @type {Map<number, (m: any) => void>} */
const waiting = new Map();
let nextID = 0;
let busy = false;
let draining = Promise.resolve();
// stopping: the page asked to stop; no audio comes in, only the lines to post go out.
let stopping = false;
// beatWindows is the window count at the last beat: no new window, no beat.
let beatWindows = 0;
// muted: no track is tapped, so no window is due; an unmute must not find the worker transcribing.
let muted = true;
/** @type {Resampler} */
let resampler;
let timer = 0;
/** @type {Start} */
let cfg;
/** @type {any} */
let ort;
/** @type {any} */
let session;
/** @type {any} */
let h;
/** @type {any} */
let c;
let ctx = new Float32Array(CTX);
let chain = Promise.resolve();
let posts = Promise.resolve();
// beats chains the beats: a late true landing after a false would raise the flag again for 3 s.
let beats = Promise.resolve();
let beating = 0;

const report = () => postMessage({ ev: 'stats', ...stats, queue: queue.length, speaking: seg.speech.length > 0 });

/**
 * beat tells the server whether this tab transcribes its participant's voice.
 * @param {boolean} on
 * @param {boolean} [now] a return to the device: no wait for the next window
 */
function beat(on, now = false) {
  // A true beat still on its way keeps the flag up: another would only queue behind it.
  if (on && (beating || stopping || (!now && !muted && stats.windows === beatWindows))) return beats;
  if (on) {
    stats.beats++;
    beatWindows = stats.windows;
  }
  beating++;
  beats = beats.then(() => fetch(cfg.presence, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ tab: cfg.tab, device_stt: on }),
    // A hung beat must not hold the ones after it: the flag would lapse behind it.
    signal: AbortSignal.timeout(BEAT_TIMEOUT_MS),
  }).catch(() => {})).then(() => { beating--; });
  return beats;
}

/**
 * setMode moves the voice: the flag goes up or down at once, not on the next beat.
 * @param {import('./policy.js').Mode} m
 * @param {string} [error]
 */
function setMode(m, error) {
  if (m === stats.mode) return;
  const was = stats.mode;
  stats.mode = m;
  // Back from a fallback the flag goes up at once; at the start it waits for the mic's audio.
  if (m === 'device' && was !== 'loading') beat(true, true);
  else if (was === 'device') beat(false);
  postMessage({ ev: 'mode', mode: m, error });
  report();
}

/**
 * transcribe decodes one utterance in the engine worker.
 * @param {Float32Array} pcm
 * @returns {Promise<string>}
 */
function transcribe(pcm) {
  return new Promise((resolve, reject) => {
    const id = nextID++;
    const late = setTimeout(() => {
      waiting.delete(id);
      reject(new Error('the engine did not answer in time'));
    }, deadline(pcm.length / SR));
    waiting.set(id, (m) => {
      clearTimeout(late);
      if (m.ev === 'text') resolve(m.text || '');
      else reject(new Error(m.message || 'the engine failed'));
    });
    cfg.engine.postMessage({ cmd: 'transcribe', id, pcm }, [pcm.buffer]);
  });
}

/**
 * post sends a line, after the ones before it.
 * @param {string} body
 */
function post(body) {
  posts = posts.then(async () => {
    // A hung post counts as lost: the lines after it, and the latency it ends, must not wait forever.
    const r = await fetch(cfg.lines, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ body }), signal: AbortSignal.timeout(POST_TIMEOUT_MS),
    }).catch(() => null);
    if (r?.ok) stats.posted++;
    else stats.lost++;
  });
  return posts;
}

/**
 * measured records an utterance's end of speech to line latency.
 * @param {Queued} u
 */
function measured(u) {
  const ms = performance.now() - u.lastVoiced;
  stats.latencies = [...stats.latencies, ms].slice(-KEEP_LATENCIES);
  setMode(policy.record(ms));
  report();
}

// drain decodes the queue, one utterance at a time; it returns the run in progress.
function drain() {
  if (busy) return draining;
  busy = true;
  draining = run();
  return draining;
}

async function run() {
  while (queue.length && stats.mode !== 'failed') {
    const u = /** @type {Queued} */ (queue.shift());
    // Stopping, only the lines to post are worth the engine's time.
    if (stopping && !u.post) continue;
    setMode(policy.waiting(queue.length));
    let text;
    try {
      text = (await transcribe(u.pcm)).trim();
    } catch (err) {
      setMode(policy.fail(), err instanceof Error ? err.message : String(err));
      break;
    }
    stats.decoded++;
    const why = await implausible(text);
    if (why) stats.dropped++;
    // A posted line is ready when the server has it.
    if (!why && u.post) post(text).then(() => measured(u));
    else measured(u);
  }
  if (stats.mode === 'failed') queue.length = 0;
  busy = false;
}

/** @param {import('./segmenter.js').Utterance | null} u */
function enqueue(u) {
  if (!u || stats.mode === 'failed') return;
  stats.utterances++;
  // Speech cut while the flag was up: the server skipped that audio, so the device posts it even
  // if it fell back since.
  queue.push({ ...u, post: stats.mode === 'device' });
  const kept = shed(queue);
  queue.splice(0, queue.length, ...kept);
  setMode(policy.waiting(queue.length));
  return drain();
}

/**
 * step runs Silero on one window, its state carried over, and feeds the segmenter.
 * @param {Float32Array} w
 * @param {number} at the time the window arrived
 */
async function step(w, at) {
  if (stopping || stats.mode === 'failed') return;
  const x = new Float32Array(CTX + WINDOW);
  x.set(ctx);
  x.set(w, CTX);
  ctx = w.slice(WINDOW - CTX);
  const o = await session.run({ input: new ort.Tensor('float32', x, [1, CTX + WINDOW]), h, c });
  h = o[session.outputNames[1]];
  c = o[session.outputNames[2]];
  stats.windows++;
  enqueue(seg.push(w, o[session.outputNames[0]].data[0], at));
}

// resetVAD starts Silero afresh, as the worker does on a new track: the audio before a gap is no
// context for the audio after it.
function resetVAD() {
  h = new ort.Tensor('float32', new Float32Array(128), [1, 1, 128]);
  c = new ort.Tensor('float32', new Float32Array(128), [1, 1, 128]);
  ctx = new Float32Array(CTX);
}

/** @param {Start} d */
async function start(d) {
  resampler = new Resampler(d.rate);
  cfg = d;
  /** @type {{silero: Uint8Array, wasm: Uint8Array}} */
  const vad = await new Promise((resolve, reject) => {
    d.engine.onmessage = (e) => (e.data.ev === 'vad' ? resolve(e.data) : reject(new Error(e.data.message)));
  });
  d.engine.onmessage = (e) => {
    const f = waiting.get(e.data.id);
    waiting.delete(e.data.id);
    f?.(e.data);
  };
  ort = await import(d.base + 'ort.all.min.mjs');
  ort.env.wasm.wasmPaths = d.base;
  // The engine's checked bytes: ORT would otherwise fetch the .wasm again, unchecked.
  ort.env.wasm.wasmBinary = vad.wasm.buffer;
  // One thread: Silero takes well under a millisecond a window, the threads belong to the engine.
  ort.env.wasm.numThreads = 1;
  session = await ort.InferenceSession.create(vad.silero, { executionProviders: ['wasm'] });
  resetVAD();
  if (stopping) return;
  d.mic.onmessage = (e) => {
    const at = performance.now();
    /** @type {Float32Array} */
    const chunk = e.data;
    const windows = resampler.push(chunk);
    // The tap reuses its chunks.
    d.mic.postMessage(chunk, [chunk.buffer]);
    for (const w of windows) chain = chain.then(() => step(w, at));
    chain = chain.catch((err) => setMode(policy.fail(), String(err)));
  };
  timer = setInterval(() => {
    stats.ticks++;
    if (stats.mode === 'device') beat(true);
    report();
  }, BEAT_MS);
  setMode('device');
}

// flush ends the speech in progress as an utterance, as on the server when a track ends.
function flush() {
  chain = chain.then(() => {
    const u = seg.flush();
    if (session) resetVAD();
    enqueue(u);
  });
  return chain;
}

// stop drops the flag now, not 3 s after the last beat, and after any beat still on its way; then
// it posts what was said under the flag, within STOP_MS, before the page ends the engine.
async function stop() {
  stopping = true;
  clearInterval(timer);
  if (!cfg) return;
  const off = beat(false);
  await Promise.race([Promise.all([off, flush().then(() => drain()).then(() => posts)]), new Promise((r) => setTimeout(r, STOP_MS))]);
}

onmessage = (e) => {
  const d = e.data;
  if (d.cmd === 'start') start(d).catch((err) => setMode(policy.fail(), err instanceof Error ? err.message : String(err)));
  // The tap lost its track (muted) or got one: the speech in progress ends as an utterance.
  else if (d.cmd === 'muted') {
    if (d.on && !muted) flush();
    muted = d.on;
  }
  else if (d.cmd === 'stop') stop().finally(() => postMessage({ ev: 'stopped' }));
};
