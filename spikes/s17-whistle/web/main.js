// S17 page. ?mode=live|bench|load &keywords=0|1 [&secs=N]. Events go to the console as "[S17] {json}".
const q = new URLSearchParams(location.search);
const report = (o) => { console.log(`[S17] ${JSON.stringify(o)}`); document.getElementById('log').textContent += `${o.ev} ${o.text ?? ''}\n`; };

async function env() {
  const e = { cores: navigator.hardwareConcurrency, isolated: crossOriginIsolated, ua: navigator.userAgent, mem: navigator.deviceMemory, vw: innerWidth };
  if (navigator.gpu) {
    const a = await navigator.gpu.requestAdapter().catch(() => null);
    e.gpu = a ? { vendor: a.info?.vendor, arch: a.info?.architecture } : null;
  }
  return e;
}

async function main() {
  const mode = q.get('mode') || 'live';
  report({ ev: 'env', ...(await env()), mode });
  const sums = await (await fetch('/eng/SHA256SUMS.json')).json();
  const keywords = q.get('keywords') === '1' ? (await (await fetch('/data/glossary.json')).json()).piper : null;
  const vad = new Worker('/vad.js', { type: 'module' });
  const stt = new Worker('/stt.js');
  for (const w of [vad, stt]) w.onmessage = (e) => report(e.data);
  window.S17 = {};
  const link = new MessageChannel();
  const t0 = performance.now();
  const ready = new Promise((res) => stt.addEventListener('message', (e) => e.data.ev === 'stt-ready' && res()));
  stt.postMessage({ cmd: 'stt', sums, keywords, vadPort: link.port2 }, [link.port2]);
  await ready;
  report({ ev: 'loaded', ms: performance.now() - t0 });
  if (mode === 'load') return;

  const vadModel = new Uint8Array(await (await fetch('/eng/silero_vad_v6.onnx')).arrayBuffer());
  if (mode === 'bench') {
    vad.postMessage({ cmd: 'vad', model: vadModel, sttPort: link.port1 }, [link.port1]);
    const wav = await (await fetch('/data/all.wav')).arrayBuffer();
    const at = new Uint8Array(wav).findIndex((_, i, a) => a[i] === 100 && a[i + 1] === 97 && a[i + 2] === 116 && a[i + 3] === 97) + 8;
    const i16 = new Int16Array(wav, at, Math.min((wav.byteLength - at) >> 1, Number(q.get('secs') || 1e9) * 16000));
    const pcm = Float32Array.from(i16, (v) => v / 32768);
    vad.postMessage({ cmd: 'bench', pcm }, [pcm.buffer]);
    return;
  }
  const stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 1 } });
  const ctx = new AudioContext({ sampleRate: 16000 });
  await ctx.audioWorklet.addModule('/worklet.js');
  const tap = new AudioWorkletNode(ctx, 'tap');
  const mic = new MessageChannel();
  vad.postMessage({ cmd: 'vad', model: vadModel, sttPort: link.port1, micPort: mic.port2 }, [link.port1, mic.port2]);
  tap.port.postMessage({ port: mic.port1 }, [mic.port1]);
  const mute = ctx.createGain();
  mute.gain.value = 0;
  ctx.createMediaStreamSource(stream).connect(tap).connect(mute).connect(ctx.destination);
  report({ ev: 'mic', wall: performance.timeOrigin + performance.now() });
  window.S17.stats = () => vad.postMessage({ cmd: 'stats' });
}
main().catch((e) => report({ ev: 'error', msg: String(e.stack || e) }));
