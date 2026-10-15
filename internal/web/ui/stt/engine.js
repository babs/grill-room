// The on-device transcription engine, a module worker: decoding never runs on the page's thread
// (010-AC2, 010-AC3). Messages in: {cmd: 'load', revision, files, backend?}, {cmd: 'transcribe',
// id, pcm}, {cmd: 'pipe', port}. Out: {ev: 'progress', done, total}, {ev: 'ready', backend, isolated,
// ms}, {ev: 'text', id, text, ms}, {ev: 'error', id?, error: 'integrity' | 'network' | 'engine', file,
// message, backend}. A 'pipe' port gets {ev: 'vad', silero, wasm}, then takes 'transcribe' as above.
import { ParakeetModel } from './vendor/parakeet/parakeet.js';

const SR = 16000;
const CACHE = 'grill-room-stt:';
/** @type {ParakeetModel | null} */
let model = null;
// backend is the one this load picked: an error names it, so the page knows whether WASM is left to try.
let backend = '';
// silero and wasm hold the VAD model's and the runtime's checked bytes, for the pipeline worker.
/** @type {Uint8Array | null} */
let silero = null;
/** @type {Uint8Array | null} */
let wasm = null;

class LoadError extends Error {
  /**
   * @param {'integrity' | 'network' | 'engine'} kind
   * @param {string} file
   * @param {string} message
   */
  constructor(kind, file, message) {
    super(message);
    this.kind = kind;
    this.file = file;
  }
}

/**
 * sri is a buffer's SHA-256 in the form of an integrity attribute.
 * @param {Uint8Array<ArrayBuffer>} buf
 */
async function sri(buf) {
  const d = new Uint8Array(await crypto.subtle.digest('SHA-256', buf));
  return 'sha256-' + btoa(String.fromCharCode(...d));
}

/**
 * fetchChecked returns a file's bytes, from this revision's cache or from the server, with its
 * sha256 checked either way before any byte reaches ORT: a cached copy can rot on disk. A bad
 * cached copy is fetched again, once. The body streams into one buffer of the pinned size, for the
 * progress.
 * @param {Cache | null} cache
 * @param {string} url
 * @param {DeviceOfferFile} f
 * @param {(n: number) => void} progress
 * @param {RequestCache} [http] the HTTP cache mode: 'no-store' for files Cache Storage keeps
 */
async function fetchChecked(cache, url, f, progress, http = 'no-store') {
  const hit = await cache?.match(url).catch(() => undefined);
  if (hit) {
    const b = new Uint8Array(await hit.arrayBuffer());
    if (await sri(b) === f.integrity) {
      progress(b.length);
      return b;
    }
    await cache?.delete(url).catch(() => false);
  }
  const buf = new Uint8Array(f.size);
  let n = 0;
  let longer = false;
  try {
    // no-store: Cache Storage keeps the one copy, the HTTP cache would hold a second.
    const res = await fetch(url, { credentials: 'omit', cache: http });
    if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
    const reader = res.body.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (n + value.length > buf.length) {
        longer = true;
        await reader.cancel();
        break;
      }
      buf.set(value, n);
      n += value.length;
      progress(value.length);
    }
  } catch (err) {
    throw new LoadError('network', f.name, String(err));
  }
  if (longer) throw new LoadError('integrity', f.name, `longer than the pinned ${f.size} bytes`);
  if (n !== buf.length) throw new LoadError('network', f.name, `the download ended after ${n} of ${f.size} bytes`);
  if (await sri(buf) !== f.integrity) throw new LoadError('integrity', f.name, 'the sha256 differs from the pinned one');
  // A full or in-memory store (a private window) still runs the engine, downloading each time.
  await cache?.put(url, new Response(buf)).catch(() => {});
  return buf;
}

async function pickBackend() {
  try {
    if (await /** @type {any} */ (navigator).gpu?.requestAdapter()) return 'webgpu-hybrid';
  } catch { /* no adapter: WASM */ }
  return 'wasm';
}

/** @param {{revision: string, files: DeviceOfferFile[], backend?: string}} req */
async function load({ revision, files, backend: wanted }) {
  const t0 = performance.now();
  const base = `/models/${encodeURIComponent(revision)}/`;
  // Without Cache Storage (a private window may refuse it) the engine still runs, downloading
  // each time: a storage fault is no verdict on the device.
  const cache = await caches.open(CACHE + revision).catch(() => null);
  const total = files.reduce((s, f) => s + f.size, 0);
  let done = 0;
  let last = 0;
  /** @param {number} n */
  const progress = (n) => {
    done += n;
    if (done === total || performance.now() - last > 250) {
      last = performance.now();
      postMessage({ ev: 'progress', done, total });
    }
  };
  /** @type {Map<string, Uint8Array>} */
  const bytes = new Map();
  try {
    for (const f of files) {
      // The runtime's modules go to import(), which takes no hash: they are checked here, through
      // the HTTP cache (immutable), so the import reads the copy just checked.
      if (f.name.endsWith('.mjs')) await fetchChecked(null, base + f.name, f, progress, 'default');
      else bytes.set(f.name, await fetchChecked(cache, base + f.name, f, progress));
    }
  } catch (err) {
    // A bad file empties the revision's cache; a lost connection keeps what was already checked.
    if (err instanceof LoadError && err.kind === 'integrity') await caches.delete(CACHE + revision).catch(() => false);
    throw err;
  }
  /** @param {RegExp} re */
  const pick = (re) => {
    for (const [name, b] of bytes) if (re.test(name)) return b;
    throw new LoadError('engine', '', `the offer has no file matching ${re}`);
  };
  const tFetch = performance.now();
  silero = pick(/silero/);
  backend = wanted || await pickBackend();
  /** @type {any} */
  let ort;
  // ORT imports its glue module inside InferenceSession.create, where a lost connection would read
  // as an engine that cannot run: both load here, where it reads as a network fault.
  for (const name of ['ort-wasm-simd-threaded.jsep.mjs', 'ort.all.min.mjs']) {
    try {
      ort = await import(base + name);
    } catch (err) {
      throw new LoadError('network', name, String(err));
    }
  }
  /** @type {any} */ (globalThis).ort = ort;
  ort.env.wasm.wasmPaths = base;
  // The checked bytes: ORT would otherwise fetch the .wasm again, unchecked.
  wasm = pick(/\.wasm$/);
  ort.env.wasm.wasmBinary = wasm.buffer;
  ort.env.wasm.numThreads = 4;
  const m = await ParakeetModel.fromUrls({
    encoderUrl: pick(/^encoder/), decoderUrl: pick(/^decoder/), tokenizerUrl: pick(/^vocab/),
    backend, cpuThreads: 4, preprocessorBackend: 'js',
  });
  const tSession = performance.now();
  // The first run compiles kernels; without it the first utterance pays for that.
  await m.transcribe(new Float32Array(SR), SR, {});
  model = m;
  const tWarm = performance.now();
  // isolated: without SharedArrayBuffer ORT quietly runs one thread instead of four.
  postMessage({ ev: 'ready', backend, isolated: crossOriginIsolated, ms: { fetch: tFetch - t0, session: tSession - tFetch, warmup: tWarm - tSession } });
}

/**
 * run answers one command; reply goes to the page or to a pipeline's port.
 * @param {any} d
 * @param {(m: object) => void} reply
 */
async function run(d, reply) {
  try {
    if (d.cmd === 'load') await load(d);
    else if (d.cmd === 'pipe') {
      /** @type {MessagePort} */
      const port = d.port;
      port.onmessage = (e) => run(e.data, (m) => port.postMessage(m));
      // Copies: ORT keeps reading its own.
      if (silero && wasm) port.postMessage({ ev: 'vad', silero: silero.slice(), wasm: wasm.slice() });
      else port.postMessage({ ev: 'error', error: 'engine', message: 'the engine is not loaded' });
    } else if (d.cmd === 'transcribe') {
      if (!model) throw new LoadError('engine', '', 'the engine is not loaded');
      const t = performance.now();
      const r = await model.transcribe(d.pcm, SR, {});
      reply({ ev: 'text', id: d.id, text: r.utterance_text || '', ms: performance.now() - t });
    }
  } catch (err) {
    const le = err instanceof LoadError ? err : new LoadError('engine', '', err instanceof Error ? err.message : String(err));
    reply({ ev: 'error', id: d.id, error: le.kind, file: le.file, message: le.message, backend });
  }
}

onmessage = (e) => run(e.data, postMessage);
