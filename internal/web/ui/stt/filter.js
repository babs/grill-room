// The STT worker's plausibility filter for Parakeet (stt/grill_stt/pipeline.py, 007-AC3), on the
// device's lines (010-AC9).

const MAX_RATIO = 2.4;
const MIN_LATIN = 0.9;

/**
 * compressionRatio is the text's UTF-8 length over its zlib-compressed length, as Python's
 * zlib.compress: a looped phrase compresses well. CompressionStream's 'deflate' is the zlib format.
 * @param {string} text
 */
export async function compressionRatio(text) {
  const raw = new TextEncoder().encode(text);
  if (!raw.length) return 0;
  const z = await new Response(new Blob([raw]).stream().pipeThrough(new CompressionStream('deflate'))).arrayBuffer();
  return raw.length / z.byteLength;
}

/**
 * latinShare is the share of the text's letters in the Latin script; a text without letters is
 * all Latin, as on the server.
 * @param {string} text
 */
export function latinShare(text) {
  const letters = text.match(/\p{L}/gu) || [];
  if (!letters.length) return 1;
  return letters.filter((c) => /\p{Script=Latin}/u.test(c)).length / letters.length;
}

/**
 * implausible is the first check a line fails, or '' when it is fit to post.
 * @param {string} text
 * @returns {Promise<'' | 'empty' | 'repetition' | 'script'>}
 */
export async function implausible(text) {
  if (!text.trim()) return 'empty';
  if (await compressionRatio(text) > MAX_RATIO) return 'repetition';
  if (latinShare(text) < MIN_LATIN) return 'script';
  return '';
}
