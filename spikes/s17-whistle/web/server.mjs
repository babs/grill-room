// Static origin for the S17 page: app, engine, ORT (VAD only), corpus. usage: node server.mjs <port> <eng> <ort> <data>
import { createReadStream, statSync } from 'node:fs';
import { createServer } from 'node:http';
import { dirname, extname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';

const [port, eng, ort, data] = process.argv.slice(2);
const ROOTS = { '/eng/': `${eng}/`, '/ort/': `${ort}/`, '/data/': `${data}/`, '/': `${dirname(fileURLToPath(import.meta.url))}/` };
const TYPES = { '.html': 'text/html', '.js': 'text/javascript', '.mjs': 'text/javascript', '.wasm': 'application/wasm', '.json': 'application/json' };

createServer((req, res) => {
  const path = decodeURIComponent(new URL(req.url, 'http://x').pathname);
  const prefix = Object.keys(ROOTS).find((p) => path.startsWith(p));
  const file = normalize(join(ROOTS[prefix], path.slice(prefix.length) || 'index.html'));
  // Security: no escape from the mapped root.
  if (!file.startsWith(ROOTS[prefix])) { res.writeHead(403).end(); return; }
  let st;
  try { st = statSync(file); } catch { res.writeHead(404).end(); return; }
  res.writeHead(200, {
    'Content-Type': TYPES[extname(file)] || 'application/octet-stream',
    'Content-Length': st.size,
    'Cross-Origin-Opener-Policy': 'same-origin',
    'Cross-Origin-Embedder-Policy': 'require-corp',
    'Cross-Origin-Resource-Policy': 'same-origin',
    'Cache-Control': 'no-store',
  });
  createReadStream(file).pipe(res);
}).listen(Number(port), '127.0.0.1', () => console.log(`s17 server on ${port}`));
