import { test, expect, chromium } from '@playwright/test';
import { createHmac } from 'node:crypto';
import { mkdirSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const { DEV_DOMAIN, LIVEKIT_API_KEY, LIVEKIT_API_SECRET } = process.env;
const seconds = Number(process.env.S4_SECONDS || 10);
// The UMD build is not in the package exports map, so resolve it by path.
const sdkPath = fileURLToPath(new URL('node_modules/livekit-client/dist/livekit-client.umd.js', import.meta.url));
const origin = `https://livekit.${DEV_DOMAIN}`;

// HS256 JWT with a LiveKit video grant: node:crypto avoids the server SDK dependency.
function token(identity, room) {
  const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url');
  const now = Math.floor(Date.now() / 1000);
  const body = `${b64({ alg: 'HS256', typ: 'JWT' })}.${b64({
    iss: LIVEKIT_API_KEY, sub: identity, nbf: now - 5, exp: now + 900,
    video: { room, roomJoin: true, canPublish: true, canSubscribe: true },
  })}`;
  return `${body}.${createHmac('sha256', LIVEKIT_API_SECRET).update(body).digest('base64url')}`;
}

async function join(browser, identity, room, policy) {
  const page = await (await browser.newContext()).newPage();
  // Keep the PeerConnections: their getStats lists every candidate pair, a receiver's only the selected one.
  await page.addInitScript(() => {
    const PC = window.RTCPeerConnection;
    window.pcs = [];
    window.RTCPeerConnection = class extends PC { constructor(...a) { super(...a); window.pcs.push(this); } };
  });
  // LiveKit's root over HTTPS gives a secure context for getUserMedia, no extra page server.
  await page.goto(`${origin}/`);
  await page.addScriptTag({ path: sdkPath });
  await page.evaluate(async ({ url, tok, policy }) => {
    const room = new LivekitClient.Room();
    // Attached audio is played out, so the jitter buffer stats are populated.
    room.on('trackSubscribed', (t) => document.body.append(t.attach()));
    await room.connect(url, tok, policy ? { rtcConfig: { iceTransportPolicy: policy } } : {});
    window.room = room;
  }, { url: origin.replace('https', 'wss'), tok: token(identity, room), policy });
  return page;
}

// Stats of the audio track's sender or receiver, which include its transport and selected pair.
const snapshot = (page, side) => page.evaluate(async (side) => {
  let track;
  if (side === 'pub') {
    track = [...window.room.localParticipant.audioTrackPublications.values()][0]?.track;
  } else {
    for (const p of window.room.remoteParticipants.values())
      for (const pub of p.audioTrackPublications.values()) track = pub.track ?? track;
  }
  const report = await track?.getRTCStatsReport();
  if (!report) return null;
  const all = [...report.values()];
  const byId = new Map(all.map((s) => [s.id, s]));
  const pair = byId.get(all.find((s) => s.type === 'transport')?.selectedCandidatePairId);
  const cand = (id) => {
    const c = byId.get(id);
    return c && { type: c.candidateType, protocol: c.protocol, relayProtocol: c.relayProtocol, address: c.address, port: c.port, url: c.url };
  };
  const rtp = all.find((s) => s.type === (side === 'pub' ? 'outbound-rtp' : 'inbound-rtp') && s.kind === 'audio');
  const rr = all.find((s) => s.type === 'remote-inbound-rtp' && s.kind === 'audio');
  const pcStats = (await Promise.all(window.pcs.map((pc) => pc.getStats()))).flatMap((r) => [...r.values()]);
  const pcById = new Map(pcStats.map((s) => [s.id, s]));
  const kind = (id) => { const c = pcById.get(id); return c && `${c.candidateType}/${c.relayProtocol ?? c.protocol}/${c.address}:${c.port}`; };
  return {
    pairs: pcStats.filter((s) => s.type === 'candidate-pair').map((p) => `${kind(p.localCandidateId)} -> ${kind(p.remoteCandidateId)} ${p.state}${p.nominated ? ' nominated' : ''}`),
    local: cand(pair?.localCandidateId), remote: cand(pair?.remoteCandidateId),
    pairRttMs: pair?.currentRoundTripTime * 1000,
    bytes: rtp?.bytesReceived ?? rtp?.bytesSent,
    packets: rtp?.packetsReceived ?? rtp?.packetsSent,
    packetsLost: rtp?.packetsLost ?? rr?.packetsLost,
    jitterMs: (rtp?.jitter ?? rr?.jitter) * 1000,
    rtcpRttMs: rr?.roundTripTime * 1000,
    jbDelayMs: rtp?.jitterBufferEmittedCount ? (rtp.jitterBufferDelay / rtp.jitterBufferEmittedCount) * 1000 : undefined,
  };
}, side);

// The path must not be direct UDP to the SFU: relay over TCP/TLS, or ICE over TCP.
const noUdp = (c) => (c.type === 'relay' ? c.relayProtocol !== 'udp' : c.protocol === 'tcp');

const cases = [
  { name: 'A-relay', policy: 'relay' },
  // UDP dropped in LiveKit's netns (compose.yaml): ICE picks among what TCP reaches.
  { name: 'B-no-udp', policy: undefined },
];

for (const { name, policy } of cases) {
  test(name, async () => {
    // The installed Chrome, no browser download.
    const browser = await chromium.launch({
      channel: 'chrome',
      args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
    });

    const room = `s4-${name}-${Date.now()}`;
    const pub = await join(browser, 'publisher', room, policy);
    await pub.evaluate(() => window.room.localParticipant.setMicrophoneEnabled(true));
    const sub = await join(browser, 'subscriber', room, policy);

    await expect.poll(async () => (await snapshot(sub, 'sub'))?.bytes ?? 0, { timeout: 30_000 }).toBeGreaterThan(0);
    const series = [];
    for (let i = 0; i <= seconds; i++) {
      series.push({ sub: await snapshot(sub, 'sub'), pub: await snapshot(pub, 'pub') });
      await sub.waitForTimeout(1000);
    }
    const first = series[0].sub;
    const last = series.at(-1);
    const avg = (xs) => Math.round((xs.reduce((a, b) => a + b, 0) / xs.length) * 10) / 10;
    const vals = (side, k) => series.map((s) => s[side]?.[k]).filter(Number.isFinite);
    const result = {
      case: name, seconds,
      subPath: { local: last.sub.local, remote: last.sub.remote },
      pubPath: { local: last.pub.local, remote: last.pub.remote },
      subPairs: last.sub.pairs,
      bytesReceivedDelta: last.sub.bytes - first.bytes,
      packetsReceived: last.sub.packets, packetsLost: last.sub.packetsLost,
      jitterMs: { avg: avg(vals('sub', 'jitterMs')), max: Math.max(...vals('sub', 'jitterMs')) },
      jitterBufferDelayMs: Math.round(last.sub.jbDelayMs),
      icePairRttMs: { sub: avg(vals('sub', 'pairRttMs')), pub: avg(vals('pub', 'pairRttMs')) },
      pubRtcpRttMs: avg(vals('pub', 'rtcpRttMs')),
    };
    console.log(JSON.stringify(result, null, 2));
    mkdirSync('test-results', { recursive: true });
    writeFileSync(`test-results/${name}.json`, `${JSON.stringify(result, null, 2)}\n`);

    expect(result.bytesReceivedDelta).toBeGreaterThan(0);
    for (const side of [last.sub, last.pub]) {
      expect(noUdp(side.local)).toBe(true);
      if (policy === 'relay') {
        expect(side.local).toMatchObject({ type: 'relay', relayProtocol: 'tls' });
        expect(side.local.url).toBe(`turns:turn.${DEV_DOMAIN}:443?transport=tcp`);
      }
    }
    await browser.close();
  });
}
