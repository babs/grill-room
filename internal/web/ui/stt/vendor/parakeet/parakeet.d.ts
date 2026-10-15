// grill-room: the part of parakeet.js that stt/engine.js uses, so `make ui-check` types the engine
// without type-checking the vendored sources.
export class ParakeetModel {
  static fromUrls(cfg: {
    encoderUrl: string | Uint8Array;
    decoderUrl: string | Uint8Array;
    tokenizerUrl: string | Uint8Array;
    backend?: string;
    cpuThreads?: number;
    preprocessorBackend?: 'js' | 'onnx';
  }): Promise<ParakeetModel>;
  transcribe(audio: Float32Array, sampleRate: number, opts?: object): Promise<{ utterance_text?: string }>;
}
