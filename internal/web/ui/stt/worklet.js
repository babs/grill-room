// The microphone tap (010-AC9): it forwards the mic, at the context's own rate, to the pipeline
// worker in chunks of SIZE samples. Without an input (a muted mic is disconnected) it sends nothing.
// The audio thread allocates nothing per quantum: the pipeline sends each chunk back for reuse.
const SIZE = 2048;

class Tap extends AudioWorkletProcessor {
  constructor() {
    super();
    /** @type {Float32Array<ArrayBuffer>[]} */
    this.pool = [];
    this.buf = new Float32Array(SIZE);
    this.n = 0;
    /** @type {MessagePort | null} */
    this.out = null;
    this.port.onmessage = (e) => {
      const out = /** @type {MessagePort} */ (e.data.port);
      out.onmessage = (m) => { this.pool.push(m.data); };
      this.out = out;
    };
  }

  /** @param {Float32Array[][]} inputs */
  process(inputs) {
    const ch = inputs[0][0];
    if (!ch || !this.out) return true;
    for (let i = 0; i < ch.length; i++) {
      this.buf[this.n++] = ch[i];
      if (this.n === SIZE) {
        this.out.postMessage(this.buf, [this.buf.buffer]);
        // Only until the first chunks come back.
        this.buf = this.pool.pop() || new Float32Array(SIZE);
        this.n = 0;
      }
    }
    return true;
  }
}
registerProcessor('tap', Tap);
