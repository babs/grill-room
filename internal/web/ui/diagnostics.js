// The Diagnostics sheet (010-AC22): where the voice is transcribed and why, the engine, the model,
// the tab's memory, the device's latency and hand-overs, and the connection. Read-only: nothing in
// it goes to the server, and nothing runs while the sheet is closed.

// The refresh while the sheet is open; only a test sets grillDiagRefreshMs.
const refreshMs = /** @type {any} */ (globalThis).grillDiagRefreshMs ?? 5_000;
const CACHE = 'grill-room-stt:';
const LAST_LINES = 20;
/** @type {Record<string, string>} */
const HANDOVER = {
  device: 'to this device', slow: 'to the server, too slow', failed: 'to the server, engine failed',
  untapped: 'to the server, microphone not passed',
};

/** @param {string} id */
const $ = (id) => /** @type {HTMLElement} */ (document.getElementById(id));
const sheet = /** @type {HTMLDialogElement} */ ($('diag'));

let open = false;
// opening numbers each opening: a refresh round left from an earlier one ends itself, so a close
// and a reopen during a measure never run two rounds.
let opening = 0;
let timer = 0;
// cached is read once per opening: it does not change while it is open.
let cached = '';
let memory = '';

/**
 * pct is the nearest-rank percentile q of xs.
 * @param {number[]} xs
 * @param {number} q
 */
export function pct(xs, q) {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.max(0, Math.ceil(q * s.length) - 1)];
}

/**
 * icePath names the network path of a WebRTC stats report's selected candidate pair.
 * @param {Iterable<any>} stats the report's values
 */
export function icePath(stats) {
  const all = [...stats];
  const byID = new Map(all.map((s) => [s.id, s]));
  const transport = all.find((s) => s.type === 'transport' && s.selectedCandidatePairId);
  const pair = transport ? byID.get(transport.selectedCandidatePairId)
    : all.find((s) => s.type === 'candidate-pair' && (s.selected || (s.nominated && s.state === 'succeeded')));
  const local = pair && byID.get(pair.localCandidateId);
  if (!local) return 'unknown';
  if (local.candidateType === 'relay') return 'TURN/' + String(local.relayProtocol || 'udp').toUpperCase();
  return local.protocol === 'tcp' ? 'direct, TCP' : 'direct, UDP';
}

/** @param {number} n */
const mb = (n) => Math.round(n / 1e6) + ' MB';

/**
 * where says where the voice goes and, for the server, why.
 * @param {Window['grillDeviceSTT']['info']} i
 * @param {string | undefined} mode the pipeline's
 */
export function where(i, mode) {
  if (i.live && mode === 'device') return 'On this device';
  const why = !i.offer ? 'this server offers no device transcription'
    : i.refused ? "this device can't use it"
    : !i.offered ? 'not available on this device'
    : i.failed ? 'the engine failed on this device, until the page reloads'
    : !i.on ? 'transcription on this device is off'
    : i.untapped ? "this browser can't pass your microphone to the engine"
    : !i.inAudio ? 'you are not in the audio'
    : i.delayed ? 'waiting a few seconds after the page reload'
    : i.live && mode === 'slow' ? 'this device is too slow right now'
    : 'starting on this device';
  return 'By the server: ' + why;
}

/** @returns {Promise<string>} */
async function cacheState() {
  const offer = window.grillDeviceSTT.info.offer;
  if (!offer) return 'no model offered';
  const name = CACHE + offer.revision;
  // has(), not open(): open() would create an empty cache.
  if (!(await caches.has(name).catch(() => false))) return 'not cached';
  const c = await caches.open(name);
  const files = offer.files.filter((f) => !f.name.endsWith('.mjs'));
  const hits = await Promise.all(files.map((f) => c.match(`/models/${encodeURIComponent(offer.revision)}/${f.name}`).then(Boolean)));
  const n = hits.filter(Boolean).length;
  return n === files.length ? 'cached' : `partly cached (${n} of ${files.length} files)`;
}

async function measure() {
  const m = /** @type {any} */ (performance).measureUserAgentSpecificMemory;
  if (!m || !crossOriginIsolated) return 'not available in this browser';
  try {
    return mb((await m.call(performance)).bytes);
  } catch {
    return 'not available in this browser';
  }
}

/** @returns {Promise<[string, string][]>} */
async function transcription() {
  const d = window.grillDeviceSTT;
  const i = d.info;
  const load = /** @type {any} */ (i.backend);
  const enc = i.offer?.files.find((f) => f.name.startsWith('encoder'))?.name.match(/encoder-model\.(\w+)\.onnx/)?.[1];
  const lat = (d.pipeline.latencies || []).slice(-LAST_LINES);
  const nav = /** @type {any} */ (navigator);
  return [
    ['Mode', where(i, d.pipeline.mode)],
    // The load lingers once the engine stops: only a running engine counts.
    ['Engine', load && (d.state === 'testing' || d.state === 'ready') ? (load.backend === 'wasm' ? `WASM, ${load.isolated ? '4 threads' : '1 thread'}` : 'WebGPU') : 'not loaded'],
    ['Model', i.offer ? `parakeet-ultra${enc ? ' ' + enc : ''}, revision ${i.offer.revision}, ${mb(i.offer.bytes)}, ${cached}` : 'none offered'],
    ['Device memory', `${nav.deviceMemory ? nav.deviceMemory + ' GB' : 'not reported'} (Chrome and Edge only, rounded and capped by the browser)`],
    ['Tab memory', memory],
    ['End of speech to line', lat.length ? `p50 ${(pct(lat, 0.5) / 1000).toFixed(1)} s, p95 ${(pct(lat, 0.95) / 1000).toFixed(1)} s ${lat.length === 1 ? 'from 1 line' : `over the last ${lat.length} lines`}` : 'no line yet'],
    ['Hand-overs', i.handovers.length ? i.handovers.map((h) => `${window.grillTime(h.at)} ${HANDOVER[h.mode] || h.mode}`).join('; ') : 'none'],
  ];
}

/** @returns {Promise<[string, string][]>} */
async function connection() {
  const g = window.grillDiag;
  const v = await g.voice();
  return [
    ['App version', /** @type {any} */ (window).grillRoom?.version || 'unknown'],
    ['Event stream', g.stream()],
    ['Audio', v.state],
    ['Audio path', v.stats ? icePath(v.stats.values()) : 'none'],
  ];
}

/**
 * fill writes one section's rows.
 * @param {string} id
 * @param {[string, string][]} rows
 */
function fill(id, rows) {
  $(id).replaceChildren(...rows.flatMap(([k, v]) => {
    const dt = document.createElement('dt');
    dt.textContent = k;
    const dd = document.createElement('dd');
    dd.textContent = v;
    return [dt, dd];
  }));
}

async function render() {
  const [t, c] = await Promise.all([transcription(), connection()]);
  if (!open) return;
  fill('diag-stt', t);
  fill('diag-net', c);
}

// refresh shows what it has at once, then the tab's memory, which a browser may take seconds to
// measure; the next round starts after it, so measures never overlap.
/** @param {number} mine the opening this round belongs to */
async function refresh(mine) {
  const live = () => open && mine === opening;
  if (!live()) return;
  await render();
  const m = await measure();
  if (!live()) return;
  memory = m;
  await render();
  if (live()) timer = window.setTimeout(() => refresh(mine), refreshMs);
}

$('diag-open').addEventListener('click', async () => {
  open = true;
  const mine = ++opening;
  memory = 'measuring…';
  cached = 'checking…';
  $('diag-said').textContent = '';
  sheet.showModal();
  const c = await cacheState().catch(() => 'unknown');
  if (mine !== opening) return;
  cached = c;
  refresh(mine);
});

sheet.addEventListener('close', () => {
  open = false;
  clearTimeout(timer);
});

// The copy holds no token, no room id and no name but the room's: a bug report may go anywhere.
$('diag-copy').addEventListener('click', async () => {
  const rows = (/** @type {string} */ id) => [...$(id).querySelectorAll('dt')].map((dt) => `${dt.textContent}: ${dt.nextElementSibling?.textContent}`);
  const text = ['Grill room diagnostics', `Room: ${window.grillDiag.roomName()}`, '', 'Transcription', ...rows('diag-stt'), '', 'Connection', ...rows('diag-net'), ''].join('\n');
  try {
    await navigator.clipboard.writeText(text);
    $('diag-said').textContent = 'Copied';
  } catch {
    $('diag-said').textContent = "Couldn't copy: your browser refused the clipboard.";
  }
});
