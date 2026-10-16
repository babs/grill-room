// Whistle through the community ONNX port (mrfakename/whistle-ONNX, fp32, greedy decoding in its js/whistle.js):
// the only way to see Whistle without the engine's beam search, which no engine flag or API turns off.
// usage: node onnx-greedy.mjs <port-dir> <data-dir> <set> [keywords]
//   port-dir: the port's js/, onnx/ and whistle.pack; data-dir: corpus/<set>.f32(.json) and corpus/<set>.json
//   writes <data-dir>/out/whistle-onnx-greedy.kw-<none|piper>.<set>.jsonl, rows as score.py reads them
import fs from 'node:fs';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

const [port, data, set, kw] = process.argv.slice(2);
const ort = createRequire(`${process.cwd()}/`)('onnxruntime-node');
const { parsePack } = await import(pathToFileURL(`${port}/js/pack.js`));
const { Tokenizer, Engram, transcribe } = await import(pathToFileURL(`${port}/js/whistle.js`));
const { OrtBackend } = await import(pathToFileURL(`${port}/js/backend.js`));

const b = fs.readFileSync(`${port}/whistle.pack`);
const pack = parsePack(b.buffer.slice(b.byteOffset, b.byteOffset + b.length));
const m = pack.man;
const fb = new Float32Array(pack.buf.slice(pack.base + m.melfb.src, pack.base + m.melfb.src + 257 * 80 * 4));
const ctx = { tok: new Tokenizer(m.tok), fb, engram: new Engram(pack) };
const opts = { intraOpNumThreads: 4, interOpNumThreads: 1 };
const be = new OrtBackend(ort, await ort.InferenceSession.create(`${port}/onnx/encoder.onnx`, opts),
  await ort.InferenceSession.create(`${port}/onnx/decoder.onnx`, opts));

const keywords = kw ? JSON.parse(fs.readFileSync(`${data}/corpus/glossary.json`, 'utf-8')).piper : [];
const meta = JSON.parse(fs.readFileSync(`${data}/corpus/${set}.json`, 'utf-8'));
const idx = new Map(JSON.parse(fs.readFileSync(`${data}/corpus/${set}.f32.json`, 'utf-8')).map(([id, off, n]) => [id, [off, n]]));
const raw = fs.readFileSync(`${data}/corpus/${set}.f32`);
const all = new Float32Array(raw.buffer, raw.byteOffset, raw.byteLength / 4);
fs.mkdirSync(`${data}/out`, { recursive: true });
const out = fs.openSync(`${data}/out/whistle-onnx-greedy.kw-${kw ? 'piper' : 'none'}.${set}.jsonl`, 'w');
await transcribe(be, ctx, new Float32Array(16000), { keywords }); // warm-up
for (const u of meta.utts) {
  const [off, n] = idx.get(u.id);
  const t = performance.now();
  const r = await transcribe(be, ctx, all.slice(off, off + n), { keywords });
  fs.writeSync(out, `${JSON.stringify({ ...u, hyp: r.text, wlang: r.language, infer: (performance.now() - t) / 1000 })}\n`);
}
fs.closeSync(out);
console.log(`done ${set} rss=${Math.round(process.memoryUsage().rss / 2 ** 20)}MB`);
