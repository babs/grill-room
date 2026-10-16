// Forwards the mic, already at the context's 16 kHz, in 512-sample windows (Silero's step).
class Tap extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buf = new Float32Array(512);
    this.n = 0;
    this.out = null;
    this.port.onmessage = (e) => { this.out = e.data.port; };
  }

  process(inputs) {
    const ch = inputs[0][0];
    if (!ch || !this.out) return true;
    for (let i = 0; i < ch.length; i++) {
      this.buf[this.n++] = ch[i];
      if (this.n === 512) {
        this.out.postMessage(this.buf, [this.buf.buffer]);
        this.buf = new Float32Array(512);
        this.n = 0;
      }
    }
    return true;
  }
}
registerProcessor('tap', Tap);
