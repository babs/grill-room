// A port of the STT worker's Segmenter (stt/grill_stt/vad.py), window by window: it cuts the
// participant's own speech into utterances with the worker's parameters (010-AC9).

export const SR = 16000;
// Samples per Silero decision, 32 ms.
export const WINDOW = 512;

/**
 * An utterance cut by the segmenter.
 * @typedef {{pcm: Float32Array, silentS: number, firstVoiced: number, lastVoiced: number}} Utterance
 */

/**
 * roundEven is Python's round(): halves go to the even neighbour, so 400 ms is 12 windows, as on
 * the server, not 13.
 * @param {number} x
 */
function roundEven(x) {
  const r = Math.round(x);
  return r - x === 0.5 && r % 2 ? r - 1 : r;
}

/** @param {number} ms */
const windows = (ms) => Math.max(1, roundEven((ms * SR) / 1000 / WINDOW));

export class Segmenter {
  constructor() {
    this.threshold = 0.5;
    this.neg = 0.35;
    this.silenceWindows = windows(800);
    this.minWindows = windows(400);
    this.maxWindows = windows(15_000);
    this.padWindows = windows(200);
    /** @type {Float32Array[]} */
    this.preroll = [];
    /** @type {Float32Array[]} */
    this.speech = [];
    this.voiced = 0;
    this.silent = 0;
    // firstVoiced and lastVoiced are the times the caller gave with the first and the last voiced
    // windows: the start and the end of speech.
    this.firstVoiced = 0;
    this.lastVoiced = 0;
  }

  /**
   * push takes one window, its speech probability and the time it arrived; it returns the
   * utterance that ended with it, or null.
   * @param {Float32Array} w
   * @param {number} p
   * @param {number} [at]
   * @returns {Utterance | null}
   */
  push(w, p, at = 0) {
    if (!this.speech.length) {
      this.preroll.push(w);
      if (this.preroll.length > this.padWindows) this.preroll.shift();
      if (p >= this.threshold) {
        this.speech = this.preroll;
        this.preroll = [];
        this.voiced = 1;
        this.silent = 0;
        this.firstVoiced = at;
        this.lastVoiced = at;
      }
      return null;
    }
    this.speech.push(w);
    if (p >= this.threshold) {
      this.voiced++;
      this.silent = 0;
      this.lastVoiced = at;
    } else if (p < this.neg) this.silent++;
    if (this.silent >= this.silenceWindows || this.speech.length >= this.maxWindows) return this.cut();
    return null;
  }

  /**
   * flush emits the speech in progress, when the audio stops.
   * @returns {Utterance | null}
   */
  flush() {
    return this.speech.length ? this.cut() : null;
  }

  /** @returns {Utterance | null} */
  cut() {
    // Keep 200 ms of the trailing silence: the engine clips the last word without it.
    const keep = this.speech.length - Math.max(0, this.silent - this.padWindows);
    const parts = this.speech.slice(0, keep);
    const silentS = (this.silent * WINDOW) / SR;
    const voiced = this.voiced;
    this.speech = [];
    this.voiced = 0;
    this.silent = 0;
    // A click or a cough: engines turn these into invented sentences.
    if (voiced < this.minWindows) return null;
    const pcm = new Float32Array(parts.length * WINDOW);
    parts.forEach((x, i) => pcm.set(x, i * WINDOW));
    return { pcm, silentS, firstVoiced: this.firstVoiced, lastVoiced: this.lastVoiced };
  }
}
