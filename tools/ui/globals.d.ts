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
