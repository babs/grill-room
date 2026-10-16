// The shared helpers internal/uikit/kit/ui.js sets on window before the pages' modules run.
interface Window {
  grillConfirm(title: string, text: string, action: string, warning?: boolean): Promise<boolean>;
}
interface Window {
  grillAgo(t: string): string;
}
interface Window {
  grillDate(t: string | number | Date): string;
  grillTime(t: string | number | Date): string;
  grillIconHTML(name: string): string;
  grillIcon(name: string): Element;
  grillPlace(pop: HTMLElement, anchor: Element): void;
  grillTip: { hide(): void; refresh(): void; show(el: Element): void; shownFor(): Element | null };
  grillToast(text: string): void;
  grillShowTab(nav: Element | null): void;
}
// The server's device transcription offer, in the "stt" event (010-AC5), and the page's device
// engine (stt/device.js) that the Phase 2 pipeline drives.
interface DeviceOfferFile { name: string; size: number; integrity: string }
interface DeviceOffer { revision: string; bytes: number; files: DeviceOfferFile[] }
interface Window {
  grillSTT?: { glossary: boolean; device: DeviceOffer | null };
  // room.js: whether this tab is in the audio; "grill:voice" follows each change.
  grillVoiceOn?: boolean;
  // room.js: the live mic's track, null while muted; "grill:mic" follows each change. grillTab names
  // the page in its presence beats.
  grillMic?: MediaStreamTrack | null;
  // room.js: what the Diagnostics sheet reads of the room page (010-AC22).
  grillDiag: { roomName(): string; stream(): string; voice(): Promise<{ state: string; stats: RTCStatsReport | null }> };
  grillTab: string;
  grillDeviceSTT: {
    readonly state: 'off' | 'loading' | 'testing' | 'ready';
    readonly checked: string | null | undefined;
    transcribe(pcm: Float32Array): Promise<{ text: string; ms: number }>;
    // The pipeline worker's last counters (stt/pipeline.js), for the tests and the bench.
    readonly pipeline: Record<string, any>;
    // What the Diagnostics sheet reads (010-AC22).
    readonly info: {
      offer: DeviceOffer | null; offered: boolean; refused: boolean; on: boolean; inAudio: boolean; delayed: boolean;
      failed: boolean; untapped: boolean;
      live: boolean; handovers: { at: number; mode: string }[]; backend?: object;
    };
    load?: object;
    selfTest?: { backend: string; rtfx: number; wer: number; text: string };
  };
}
// The AudioWorklet scope of stt/worklet.js, which the DOM library leaves out.
declare class AudioWorkletProcessor {
  readonly port: MessagePort;
}
declare function registerProcessor(name: string, ctor: new () => AudioWorkletProcessor): void;
