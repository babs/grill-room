import { test, expect } from './fixtures.js';

// The e2e renders with the fonts in fonts/ whatever the machine has (fonts.conf): the kit's font
// stacks measure reference text at the widths those fonts' glyphs give it, here as in CI.
const line = 'Hamburgefonstiv 0123456789';
// At 16 px, the sum of the text's advance widths in each font's hmtx table. Noto Sans, a desktop's
// usual system-ui, gives the line 227.44; a missing emoji font draws 🔥 as a fallback box.
const widths = { regular: 245.27, bold: 273.23, mono: 250.45, emoji: 19.92 };

test('the e2e renders with the pinned fonts', async ({ page }) => {
  await page.goto('/');
  const got = await page.evaluate((line) => {
    const css = getComputedStyle(document.documentElement);
    const measure = (font, text = line) => {
      const s = document.createElement('span');
      s.textContent = text;
      s.style.cssText = `position: absolute; white-space: pre; font: ${font}; font-kerning: none; font-variant-ligatures: none`;
      document.body.append(s);
      const w = s.getBoundingClientRect().width;
      s.remove();
      return w;
    };
    return {
      regular: measure(`400 16px ${css.getPropertyValue('--font')}`),
      bold: measure(`600 16px ${css.getPropertyValue('--font')}`),
      mono: measure(`400 16px ${css.getPropertyValue('--mono')}`),
      emoji: measure(`400 16px ${css.getPropertyValue('--font')}`, '🔥'),
    };
  }, line);
  for (const [k, w] of Object.entries(widths)) expect(got[k], k).toBeCloseTo(w, 0);
});
