import { test, expect, devices } from '@playwright/test';
import { join as joinPath } from 'node:path';
import { createRoom, invite, join, joinAudio, inboundAudio, launch, admin, fixture, dropProfiles } from '../voice/helpers.js';
import { upload, annotateSelection, pairDaemon, liveDaemon, messagesRequests, removeSandbox, ask } from '../helpers.js';

const plan = '# Rollout plan\n\nThe rollout needs a rollback step before the freeze.\n';

// Every PeerConnection the page opens gathers relay candidates only: media must go through TURN,
// which LiveKit advertises as TURN/TLS on 443, the one path open to a participant outside the LAN.
function forceRelay() {
  const PC = window.RTCPeerConnection;
  const relay = (c) => ({ ...c, iceTransportPolicy: 'relay' });
  window.RTCPeerConnection = class extends PC {
    constructor(c, ...rest) { super(relay(c), ...rest); }
    setConfiguration(c) { return super.setConfiguration(relay(c)); }
  };
}

// audioStats reads the first remote audio track's receiver stats and the local end of its selected
// candidate pair.
const audioStats = (page) => page.evaluate(async () => {
  for (const p of voice.remoteParticipants.values()) {
    for (const pub of p.audioTrackPublications.values()) {
      const all = [...((await pub.track?.getRTCStatsReport())?.values() ?? [])];
      const rtp = all.find((s) => s.type === 'inbound-rtp' && s.kind === 'audio');
      if (!rtp) continue;
      const byId = new Map(all.map((s) => [s.id, s]));
      const pair = byId.get(all.find((s) => s.type === 'transport')?.selectedCandidatePairId);
      const c = byId.get(pair?.localCandidateId) ?? {};
      return {
        packets: rtp.packetsReceived, lost: rtp.packetsLost, jitterMs: rtp.jitter * 1000,
        local: { type: c.candidateType, protocol: c.protocol, relayProtocol: c.relayProtocol, url: c.url },
      };
    }
  }
  return null;
});

// Audio flows to each page, keeps flowing, and arrives with little loss and jitter.
async function assertAudio(pages, relay) {
  for (const [name, page] of Object.entries(pages)) {
    await expect.poll(() => inboundAudio(page), { timeout: 30_000 }).toBeGreaterThan(0);
    const before = await inboundAudio(page);
    await page.waitForTimeout(3000);
    expect(await inboundAudio(page)).toBeGreaterThan(before + 3000);
    const st = await audioStats(page);
    console.log(`${name} receives:`, JSON.stringify(st));
    expect(st.packets).toBeGreaterThan(100);
    expect(st.lost / (st.packets + st.lost)).toBeLessThan(0.05);
    expect(st.jitterMs).toBeLessThan(50);
    if (relay) {
      expect(st.local).toMatchObject({ type: 'relay', relayProtocol: 'tls' });
      expect(st.local.url).toMatch(/^turns:[^:]+:443\b/);
    }
  }
}

// 001-AC38: a whole grill session on the lab deployment through its public hostnames: two
// participants, an upload, an annotation, audio relayed over TURN/TLS on 443, a voice line, and one
// agent turn run by a real daemon on this machine.
test('grill session on the lab, audio over TURN/TLS', async ({ request, baseURL }) => {
  const aliceBrowser = await launch(fixture.wav);
  const bobBrowser = await launch();
  const room = await createRoom(request, 'Lab session');
  const daemon = liveDaemon(joinPath('test-results', 'lab', 'daemon'), `${baseURL}/r/${room.id}`);
  const joined = [];
  try {
    const alice = await join(aliceBrowser, await invite(request, room.id), 'Alice', forceRelay);
    const bob = await join(bobBrowser, await invite(request, room.id), 'Bob', forceRelay);
    joined.push(alice, bob);

    await upload(alice, 'plan.md', 'text/markdown', plan);
    await expect(alice.frameLocator('#doc-frame iframe').locator('h1')).toHaveText('Rollout plan');
    await bob.locator('#documents').getByRole('button', { name: 'plan.md' }).click();
    await annotateSelection(bob, 'rollback step', 'How long does a rollback take?');
    await expect(alice.locator('#annot-list')).toContainText('How long does a rollback take?');

    // Bob listens first, so the worker is in the room when Alice's recorded speech starts.
    await joinAudio(bob);
    await expect(bob.locator('#stt-status')).toHaveText(/^\d+ voices? (is|are) being written down/, { timeout: 30_000 });
    await joinAudio(alice);
    await assertAudio({ alice, bob }, true);
    const first = fixture.utterances[0];
    const line = bob.locator('.msg.voice', { has: bob.locator('strong', { hasText: 'Alice' }) }).first();
    await expect(line).toBeVisible({ timeout: (first.end + 15) * 1000 });
    console.log('voice line:', await line.locator('.body').textContent());

    await pairDaemon(alice, daemon);
    await expect(alice.locator('button.persona[data-name="Grill"] .led')).toHaveAttribute('aria-label', 'Grill ready', { timeout: 60_000 });
    const synthesis = await ask(alice, room.id, 'Without using any tool, reply in one short sentence: what is this room about?');
    console.log('synthesis:', synthesis);
    expect(synthesis.trim()).not.toBe('');
    const requests = messagesRequests(daemon);
    console.log(`real Claude requests: ${requests}`);
    expect(requests).toBeLessThanOrEqual(6);
  } finally {
    daemon.proc.kill('SIGTERM');
    await daemon.exited;
    removeSandbox(daemon.sandbox);
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
    await dropProfiles(request, joined);
    await aliceBrowser.close();
    await bobBrowser.close();
  }
});

// 001-AC38: with the browser free to pick any path, audio still flows; the log shows which one.
test('audio on the lab with default ICE', async ({ request }) => {
  const aliceBrowser = await launch(fixture.wav);
  const bobBrowser = await launch();
  const room = await createRoom(request, 'Lab audio');
  const joined = [];
  try {
    const alice = await join(aliceBrowser, await invite(request, room.id), 'Alice');
    const bob = await join(bobBrowser, await invite(request, room.id), 'Bob');
    joined.push(alice, bob);
    await joinAudio(bob);
    await joinAudio(alice);
    await assertAudio({ alice, bob }, false);
  } finally {
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
    await dropProfiles(request, joined);
    await aliceBrowser.close();
    await bobBrowser.close();
  }
});

// Live delivery through the lab ingress (HTTP/2), on a page emulating a phone: a message typed by
// one participant shows on the other's page without a reload, also after the page comes back from
// the background. No daemon, no Claude request.
test('messages reach a phone page live on the lab', async ({ request }) => {
  const browser = await launch();
  const room = await createRoom(request, 'Lab live');
  const joined = [];
  try {
    const alice = await join(browser, await invite(request, room.id), 'Alice');
    const bob = await join(browser, await invite(request, room.id), 'Bob');
    joined.push(alice, bob);
    const { defaultBrowserType, ...pixel } = devices['Pixel 7'];
    const phone = await browser.newContext(pixel);
    const inv = new URL((await invite(request, room.id)).url);
    const carol = await phone.newPage();
    await carol.goto(inv.pathname + inv.hash);
    await carol.getByLabel('Your name').fill('Carol');
    await carol.getByRole('button', { name: 'Join the room' }).click();
    joined.push(carol);
    await expect(carol.locator('#messages')).toContainText('Carol joined');

    const say = async (page, text) => {
      await page.getByLabel('Message').fill(text);
      await page.getByLabel('Message').press('Enter');
    };
    await say(bob, 'live one');
    await expect(carol.locator('#messages')).toContainText('live one', { timeout: 3000 });
    await expect(alice.locator('#messages')).toContainText('live one', { timeout: 3000 });

    await carol.evaluate(() => {
      Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await say(alice, 'live two');
    await expect(carol.locator('#messages')).toContainText('live two', { timeout: 3000 });
    await expect(carol.locator('.msg', { hasText: 'live one' })).toHaveCount(1);
  } finally {
    await request.delete(`/api/rooms/${room.id}`, { headers: admin });
    await dropProfiles(request, joined);
    await browser.close();
  }
});
