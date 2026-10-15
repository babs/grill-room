// The shared helpers internal/uikit/kit/ui.js sets on window before the pages' modules run.
interface Window {
  grillConfirm(title: string, text: string, action: string): Promise<boolean>;
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
}
// The server's device transcription offer, in the "stt" event (010-AC5), and the page's device
// engine (stt/device.js) that the Phase 2 pipeline drives.
interface DeviceOfferFile { name: string; size: number; integrity: string }
interface DeviceOffer { revision: string; bytes: number; files: DeviceOfferFile[] }
interface Window {
  grillSTT?: { glossary: boolean; device: DeviceOffer | null };
  // room.js: whether this tab is in the audio; "grill:voice" follows each change.
  grillVoiceOn?: boolean;
  grillDeviceSTT: {
    readonly state: 'off' | 'loading' | 'testing' | 'ready';
    readonly checked: string | null | undefined;
    transcribe(pcm: Float32Array): Promise<{ text: string; ms: number }>;
    load?: object;
    selfTest?: { backend: string; rtfx: number; wer: number; text: string };
  };
}
