// Where the participant's voice is transcribed (010-AC17): on the device, back on the server while
// the device is too slow, or on the server until the page reloads once the engine failed.

/** @typedef {'device' | 'slow' | 'failed'} Mode */

const WINDOW_MS = 60_000;
const MIN_UTTERANCES = 3;
const LEAVE_P95_MS = 4_000;
const RETURN_P95_MS = 3_000;
const MAX_QUEUE = 2;
const RETURN_GAP_MS = 120_000;
const MAX_UNPOSTED = 3;
// How long after a fall back the worker may still skip the voice: the false beat may wait up to 2 s
// behind a beat on its way, then the worker resumes within 3 s of it (010-AC15).
const RESUME_MS = 5_000;

/**
 * deadline is how long the engine may take on an utterance of this many seconds before it counts
 * as failed: a hung engine would otherwise hold the queue forever.
 * @param {number} seconds
 */
export const deadline = (seconds) => Math.max(10_000, 4_000 * seconds);

/**
 * shed drops the oldest utterances decoded only to measure the device, beyond MAX_UNPOSTED: while
 * the server holds the voice they would only grow the queue. Utterances to post are all kept.
 * @template {{post: boolean}} T
 * @param {T[]} queue
 * @returns {T[]}
 */
export function shed(queue) {
  let extra = queue.filter((u) => !u.post).length - MAX_UNPOSTED;
  return queue.filter((u) => u.post || extra-- <= 0);
}

/**
 * p95 is the nearest-rank 95th percentile.
 * @param {number[]} xs
 */
export function p95(xs) {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.ceil(0.95 * s.length) - 1];
}

/**
 * Policy decides from the end-of-speech-to-line latencies and the queue length. now is the clock,
 * in ms, injected for the tests.
 */
export class Policy {
  /** @param {() => number} now */
  constructor(now) {
    this.now = now;
    /** @type {Mode} */
    this.mode = 'device';
    /** @type {{at: number, ms: number}[]} */
    this.samples = [];
    this.queue = 0;
    /** @type {number | null} */
    this.returnedAt = null;
    /** @type {number | null} */
    this.handedBackAt = null;
  }

  /**
   * posts says whether the device posts an utterance whose speech started at start. After a hand-back
   * it still posts what started before the worker can have resumed: a duplicate, which the server
   * drops, beats a line nobody wrote down.
   * @param {number} start
   */
  posts(start) {
    if (this.mode === 'device') return true;
    return this.mode === 'slow' && this.handedBackAt !== null && start < this.handedBackAt + RESUME_MS;
  }

  /** recent is the latencies of the last 60 s. */
  recent() {
    const since = this.now() - WINDOW_MS;
    this.samples = this.samples.filter((s) => s.at > since);
    return this.samples.map((s) => s.ms);
  }

  /**
   * record adds an utterance's latency; it returns the mode after it.
   * @param {number} ms
   */
  record(ms) {
    this.samples.push({ at: this.now(), ms });
    return this.decide();
  }

  /**
   * waiting sets the number of utterances waiting for the engine; it returns the mode after it.
   * @param {number} n
   */
  waiting(n) {
    this.queue = n;
    return this.decide();
  }

  // fail keeps the voice on the server until the page reloads.
  fail() {
    this.mode = 'failed';
    return this.mode;
  }

  /** @returns {Mode} */
  decide() {
    if (this.mode === 'failed') return this.mode;
    const xs = this.recent();
    const enough = xs.length >= MIN_UTTERANCES;
    if (this.mode === 'device') {
      if ((enough && p95(xs) > LEAVE_P95_MS) || this.queue > MAX_QUEUE) {
        this.mode = 'slow';
        this.handedBackAt = this.now();
      }
    } else if (enough && p95(xs) <= RETURN_P95_MS && this.queue === 0
      && (this.returnedAt === null || this.now() - this.returnedAt >= RETURN_GAP_MS)) {
      this.mode = 'device';
      this.returnedAt = this.now();
    }
    return this.mode;
  }
}
