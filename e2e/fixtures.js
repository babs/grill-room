import { test as base, expect } from '@playwright/test';

// test closes, after each test, the browser contexts it opened with browser.newContext(): the
// browser lives as long as its worker, and contexts left open keep their pages and event streams
// running into every later test, until the CI runner runs out of memory.
export const test = base.extend({
  closeContexts: [async ({ browser }, use) => {
    await use();
    await Promise.all(browser.contexts().map((c) => c.close().catch(() => {})));
  }, { auto: true }],
});

export { expect };
