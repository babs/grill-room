// VAD worker: Silero v6 on single-thread ORT and the server worker's Segmenter, as in S11 (scratch prototype, not shipped).
import * as ort from '/ort/ort.all.min.mjs';

const SR = 16000;
const WIN = 512;
const CTX = 64;
const now = () => performance.timeOrigin + performance.now();
const emit = (o) => postMessage(o);
ort.env.wasm.wasmPaths = '/ort/';

// Port of stt/ Segmenter (vad.py): thresholds 0.5/0.35, 800 ms cut, 400 ms min speech, 15 s max, 200 ms pad.
class Segmenter {
  constructor({ threshold = 0.5, neg = 0.35, minSilenceMs = 800, minSpeechMs = 400, maxSpeechS = 15, padMs = 200 } = {}) {
    Object.assign(this, { threshold, neg, minSilenceMs, minSpeechMs, maxSpeechS, padMs });
    this.preroll = [];
    this.speech = null;
  }

  windows(ms) { return Math.max(1, Math.round((ms * SR) / 1000 / WIN)); }

  // One 512-sample window and its speech probability; returns an utterance or null.
  push(w, p, idx, wall) {
    if (!this.speech) {
      this.preroll.push(w);
      if (this.preroll.length > this.windows(this.padMs)) this.preroll.shift();
      if (p >= this.threshold) {
        this.speech = this.preroll;
        this.start = idx - this.preroll.length + 1;
        this.preroll = [];
        Object.assign(this, { voiced: 1, silent: 0, lastIdx: idx, lastWall: wall });
      }
      return null;
    }
    this.speech.push(w);
    if (p >= this.threshold) Object.assign(this, { voiced: this.voiced + 1, silent: 0, lastIdx: idx, lastWall: wall });
    else if (p < this.neg) this.silent += 1;
    if (this.silent >= this.windows(this.minSilenceMs) || this.speech.length >= this.windows(this.maxSpeechS * 1000)) {
      return this.cut(idx, wall);
    }
    return null;
  }

  flush(idx, wall) { return this.speech ? this.cut(idx, wall) : null; }

  cut(idx, wall) {
    const keep = this.speech.length - Math.max(0, this.silent - this.windows(this.padMs));
    const parts = this.speech.slice(0, keep);
    const u = { start: this.start * WIN, end: (this.start + keep) * WIN, lastVoiced: (this.lastIdx + 1) * WIN, cutAt: (idx + 1) * WIN, lastWall: this.lastWall, cutWall: wall };
    const voiced = this.voiced;
    this.speech = null;
    if (voiced < this.windows(this.minSpeechMs)) return null; // clicks and coughs
    u.pcm = new Float32Array(parts.length * WIN);
    parts.forEach((x, i) => u.pcm.set(x, i * WIN));
    return u;
  }
}

async function vadRole({ model, sttPort }) {
  ort.env.wasm.numThreads = 1;
  const s = await ort.InferenceSession.create(model, { executionProviders: ['wasm'] });
  let h = new ort.Tensor('float32', new Float32Array(128), [1, 1, 128]);
  let c = new ort.Tensor('float32', new Float32Array(128), [1, 1, 128]);
  let ctx = new Float32Array(CTX);
  const seg = new Segmenter();
  let idx = 0;
  let vadMs = 0;
  let n = 0;
  const send = (u) => { if (u) { u.id = n++; sttPort.postMessage(u, [u.pcm.buffer]); } };
  async function step(w, wall) {
    const x = new Float32Array(CTX + WIN);
    x.set(ctx); x.set(w, CTX);
    ctx = w.slice(WIN - CTX);
    const t = performance.now();
    const o = await s.run({ input: new ort.Tensor('float32', x, [1, CTX + WIN]), h, c });
    vadMs += performance.now() - t;
    h = o[s.outputNames[1]];
    c = o[s.outputNames[2]];
    send(seg.push(w, o[s.outputNames[0]].data[0], idx++, wall));
  }
  // Windows are processed strictly in order; the receive time is taken before queueing.
  let chain = Promise.resolve();
  emit({ ev: 'vad-ready', inputs: s.inputNames, outputs: s.outputNames });
  return {
    window(w) { const wall = now(); chain = chain.then(() => step(w, wall)); },
    async bench(pcm) {
      for (let i = 0; i + WIN <= pcm.length; i += WIN) await step(pcm.subarray(i, i + WIN), 0);
      send(seg.flush(idx, 0));
      emit({ ev: 'vad-done', windows: idx, vadMs, utts: n });
    },
    stats() { emit({ ev: 'vad-stats', windows: idx, vadMs }); },
  };
}

let role;
onmessage = async (e) => {
  try {
    const d = e.data;
    if (d.cmd === 'vad') { role = await vadRole(d); d.micPort && (d.micPort.onmessage = (m) => role.window(m.data)); }
    else if (d.cmd === 'bench') await role.bench(d.pcm);
    else if (d.cmd === 'stats') role.stats();
  } catch (err) {
    emit({ ev: 'error', msg: String(err && (err.stack || err.message) || err) });
  }
};
