// The mic at the audio context's own rate to Silero's 16 kHz windows (010-AC9). Firefox refuses a
// 16 kHz context fed by a mic, so the context runs at its default rate and this resamples.

import { SR, WINDOW } from './segmenter.js';

// Fractional positions the kernel table holds.
const PHASES = 128;

/**
 * Resampler low-passes below the output's Nyquist frequency and interpolates, with a windowed-sinc
 * kernel: a naive drop of samples would fold everything above 8 kHz back into speech.
 */
export class Resampler {
  /** @param {number} from the input rate, in Hz */
  constructor(from) {
    this.step = from / SR;
    // Kernel taps either side of a sample: a higher input rate needs more for the same transition
    // band in Hz.
    const HALF = Math.ceil(32 * Math.max(1, this.step / 3));
    this.half = HALF;
    // A cutoff at 90 % of the output's Nyquist frequency, in cycles per input sample.
    const fc = Math.min(0.5, 0.45 / this.step);
    /** @type {Float32Array[]} one kernel per fractional position, each summing to 1 */
    this.table = [];
    for (let p = 0; p < PHASES; p++) {
      const k = new Float32Array(2 * HALF);
      let sum = 0;
      for (let i = 0; i < 2 * HALF; i++) {
        const x = i - HALF + 1 - p / PHASES;
        const sinc = x === 0 ? 2 * fc : Math.sin(2 * Math.PI * fc * x) / (Math.PI * x);
        // Blackman window over the kernel's span.
        const t = (x + HALF) / (2 * HALF);
        const w = 0.42 - 0.5 * Math.cos(2 * Math.PI * t) + 0.08 * Math.cos(4 * Math.PI * t);
        k[i] = sinc * w;
        sum += k[i];
      }
      for (let i = 0; i < k.length; i++) k[i] /= sum;
      this.table.push(k);
    }
    // The input not yet consumed, led by HALF - 1 samples of history (silence at the start).
    this.buf = new Float32Array(HALF - 1);
    this.pos = HALF - 1;
    // The output samples not yet making a whole window.
    this.out = new Float32Array(WINDOW);
    this.n = 0;
  }

  /**
   * push takes input samples and returns the whole 512-sample windows they complete.
   * @param {Float32Array} chunk
   * @returns {Float32Array[]}
   */
  push(chunk) {
    const HALF = this.half;
    const x = new Float32Array(this.buf.length + chunk.length);
    x.set(this.buf);
    x.set(chunk, this.buf.length);
    /** @type {Float32Array[]} */
    const done = [];
    let pos = this.pos;
    for (;;) {
      let i0 = Math.floor(pos);
      let ph = Math.round((pos - i0) * PHASES);
      // A fraction that rounds up to a whole phase is the next sample's phase 0.
      if (ph === PHASES) {
        i0++;
        ph = 0;
      }
      if (i0 + HALF >= x.length) break;
      const k = this.table[ph];
      const base = i0 - HALF + 1;
      let y = 0;
      for (let j = 0; j < k.length; j++) y += x[base + j] * k[j];
      this.out[this.n++] = y;
      if (this.n === WINDOW) {
        done.push(this.out);
        this.out = new Float32Array(WINDOW);
        this.n = 0;
      }
      pos += this.step;
    }
    const keep = Math.floor(pos) - HALF + 1;
    this.buf = x.slice(keep);
    this.pos = pos - keep;
    return done;
  }
}
