// A port of the STT worker's Segmenter (stt/grill_stt/vad.py), window by window: it cuts the
// participant's own speech into utterances with the worker's parameters (010-AC9).

export const SR = 16000;
// Samples per Silero decision, 32 ms.
export const WINDOW = 512;

/**
 * An utterance cut by the segmenter; gapS and forced are vad.py's Cut's (018-AC2).
 * @typedef {{pcm: Float32Array, silentS: number, gapS: number | null, forced: boolean, firstVoiced: number, lastVoiced: number}} Utterance
 * @typedef {{w: Float32Array, p: number, at: number}} Win
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
    // 018-AC3: the cut at the cap lands on the quietest of the last 3 s, the worker's default.
    this.searchWindows = roundEven((3000 * SR) / 1000 / WINDOW);
    this.padWindows = windows(200);
    /** @type {Win[]} */
    this.preroll = [];
    /** @type {Win[]} */
    this.speech = [];
    this.silent = 0;
    // seen counts the windows pushed; start is the index of speech[0] among them.
    this.seen = 0;
    this.start = 0;
    // The window after the last utterance's last voiced one.
    /** @type {number | null} */
    this.prevEnd = null;
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
    this.seen++;
    if (!this.speech.length) {
      this.preroll.push({ w, p, at });
      if (this.preroll.length > this.padWindows) this.preroll.shift();
      if (p >= this.threshold) {
        this.speech = this.preroll;
        this.preroll = [];
        this.start = this.seen - this.speech.length;
        this.silent = 0;
      }
      return null;
    }
    this.speech.push({ w, p, at });
    if (p >= this.threshold) this.silent = 0;
    else if (p < this.neg) this.silent++;
    if (this.silent >= this.silenceWindows) return this.emit();
    if (this.speech.length >= this.maxWindows) return this.cutAtCap();
    return null;
  }

  /**
   * flush emits the speech in progress, when the audio stops; the next utterance has no gap, nor
   * any audio from before the flush.
   * @returns {Utterance | null}
   */
  flush() {
    const u = this.speech.length ? this.emit() : null;
    this.prevEnd = null;
    this.preroll = [];
    return u;
  }

  /** @returns {Utterance | null} */
  emit() {
    // Keep 200 ms of the trailing silence: the engine clips the last word without it.
    const keep = this.speech.length - Math.max(0, this.silent - this.padWindows);
    const u = this.utterance(keep, (this.silent * WINDOW) / SR, false);
    this.speech = [];
    this.silent = 0;
    return u;
  }

  /**
   * cutAtCap cuts on the quietest window of the search (018-AC3); the windows after it open the
   * next utterance, so no audio is dropped or repeated.
   * @returns {Utterance | null}
   */
  cutAtCap() {
    const n = this.speech.length;
    // The latest of equal minima: steady speech keeps the hard cut.
    let i = n - 1;
    for (let j = n - 2; j >= Math.max(0, n - this.searchWindows); j--) if (this.speech[j].p < this.speech[i].p) i = j;
    const u = this.utterance(i + 1, ((n - i - 1) * WINDOW) / SR, true);
    this.speech = this.speech.slice(i + 1);
    this.start += i + 1;
    this.silent = 0;
    for (const x of this.speech) {
      if (x.p >= this.threshold) this.silent = 0;
      else if (x.p < this.neg) this.silent++;
    }
    return u;
  }

  /**
   * utterance is the first keep windows of the speech, unless too little of it is voiced.
   * @param {number} keep
   * @param {number} silentS
   * @param {boolean} forced
   * @returns {Utterance | null}
   */
  utterance(keep, silentS, forced) {
    const parts = this.speech.slice(0, keep);
    let first = -1;
    let last = -1;
    let voiced = 0;
    parts.forEach((x, j) => {
      if (x.p < this.threshold) return;
      if (first < 0) first = j;
      last = j;
      voiced++;
    });
    // A click or a cough: engines turn these into invented sentences.
    if (voiced < this.minWindows) return null;
    const pause = this.prevEnd === null ? null : this.start + first - this.prevEnd;
    // 018-AC2: only a forced cut leaves so short a pause: the speaker went on talking through it.
    const gapS = pause === null ? null : pause < this.silenceWindows ? 0 : (pause * WINDOW) / SR;
    this.prevEnd = this.start + last + 1;
    const pcm = new Float32Array(parts.length * WINDOW);
    parts.forEach((x, j) => pcm.set(x.w, j * WINDOW));
    // The start and the end of speech: the times the caller gave with the first and the last voiced windows.
    return { pcm, silentS, gapS, forced, firstVoiced: parts[first].at, lastVoiced: parts[last].at };
  }
}
