// The shared helpers internal/uikit/kit/ui.js sets on window before the pages' modules run.
interface Window {
  grillConfirm(title: string, text: string, action: string): Promise<boolean>;
}
interface Window {
  grillAgo(t: string): string;
}
