// STT worker: the official Cactus WASM build (needle.js + needle.wasm, single thread) with whistle.cact,
// one needle_transcribe per VAD utterance, FIFO. Classic worker: needle.js is an emscripten UMD script.
importScripts('/eng/needle.js');

const SR = 16000;
const now = () => performance.timeOrigin + performance.now();
const emit = (o) => postMessage(o);
const b64 = (hex) => btoa(String.fromCharCode(...hex.match(/../g).map((x) => parseInt(x, 16))));

async function cached(name, sha) {
  const url = `/eng/${name}`;
  const cache = await caches.open('s17-models');
  const t0 = performance.now();
  const hit = await cache.match(url);
  if (hit) return { buf: await hit.arrayBuffer(), ms: performance.now() - t0, hit: true };
  // Security: the browser checks the pinned SHA-256 before any byte reaches the engine or the cache.
  const r = await fetch(url, { integrity: `sha256-${b64(sha)}` });
  if (!r.ok) throw new Error(`${url}: ${r.status}`);
  const buf = await r.arrayBuffer();
  await cache.put(url, new Response(buf));
  return { buf, ms: performance.now() - t0, hit: false };
}

async function sttRole({ sums, keywords, vadPort }) {
  const t0 = performance.now();
  const wasm = await cached('needle.wasm', sums['needle.wasm']);
  const cact = await cached('whistle.cact', sums['whistle.cact']);
  const tFetch = performance.now();
  const M = await createNeedle({ wasmBinary: wasm.buf });
  const put = (u8) => { const p = M._malloc(u8.length); M.HEAPU8.set(u8, p); return p; };
  const model = new Uint8Array(cact.buf);
  const mp = put(model);
  if (M._needle_load(mp, BigInt(model.length)) < 0) throw new Error('needle_load failed');
  M._free(mp);
  const kw = keywords ? put(new TextEncoder().encode(`${keywords.join('\n')}\0`)) : 0;
  const cap = 1 << 18;
  const out = M._malloc(cap);
  function transcribe(pcm) {
    const p = put(new Uint8Array(pcm.buffer, pcm.byteOffset, pcm.byteLength));
    const rc = M._needle_transcribe(p, pcm.length, 0, kw, 0, out, cap);
    M._free(p);
    if (rc < 0) return { text: '', error: 'needle_transcribe failed' };
    const u8 = M.HEAPU8.subarray(out, out + cap);
    return JSON.parse(new TextDecoder().decode(u8.subarray(0, u8.indexOf(0))));
  }
  const load = { fetchMs: tFetch - t0, sessionMs: performance.now() - tFetch, files: { wasm: { ms: Math.round(wasm.ms), hit: wasm.hit }, cact: { ms: Math.round(cact.ms), hit: cact.hit } } };
  // Warm-up: the first run would otherwise land on the first utterance.
  transcribe(new Float32Array(SR));
  load.warmupMs = performance.now() - t0 - load.fetchMs - load.sessionMs;
  emit({ ev: 'stt-ready', ...load });
  const queue = [];
  let busy = false;
  function drain() {
    if (busy) return;
    busy = true;
    while (queue.length) {
      const u = queue.shift();
      const ds = now();
      const r = transcribe(u.pcm);
      const de = now();
      emit({ ev: 'line', id: u.id, start: u.start, end: u.end, lastVoiced: u.lastVoiced, cutAt: u.cutAt, lastWall: u.lastWall, cutWall: u.cutWall, decodeStart: ds, decodeEnd: de, dur: u.pcm.length / SR, text: r.text, lang: r.language, ttft: r.ttft_ms });
    }
    busy = false;
    emit({ ev: 'idle' });
  }
  // A queued message cannot be read while a decode runs (one thread): drain on each arrival.
  vadPort.onmessage = (e) => { queue.push(e.data); drain(); };
}

onmessage = async (e) => {
  try {
    if (e.data.cmd === 'stt') await sttRole(e.data);
  } catch (err) {
    emit({ ev: 'error', msg: String(err && (err.stack || err.message) || err) });
  }
};
