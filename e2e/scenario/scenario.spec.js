import { test, expect } from '@playwright/test';
import { mkdirSync, readFileSync, readdirSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { basename, extname, join } from 'node:path';
import { launch, joinAudio, dropProfiles, admin, audioItem } from '../voice/helpers.js';
import { snoozePush, snoozeKey } from '../quiet-push.js';
import { upload, annotateSelection, pairDaemon, liveDaemon, logLines, messagesRequests, removeSandbox, adminAPI } from '../helpers.js';

// The scenario driver (004-AC16; the library's lab runs, 004-AC17, go through it): it plays the
// plan grill-scenario wrote, step by step, and records what happened in result.json. It asserts
// nothing about the outcome: grill-scenario scores it.
const plan = JSON.parse(readFileSync(process.env.GRILL_SCENARIO_PLAN, 'utf8'));
const micDir = join(plan.run, 'mic');
const mime = { '.md': 'text/markdown', '.txt': 'text/plain', '.html': 'text/html', '.pdf': 'application/pdf' };

// Each browser plays <name>.wav as its microphone; pointing the link at the next round's file and
// reopening the microphone plays that round, since Chrome reads the file each time capture starts.
function point(name, round) {
  const link = join(micDir, `${name}.wav`);
  rmSync(link, { force: true });
  symlinkSync(plan.rounds[round - 1].tracks[name], link);
}

// probe runs in every page: the instant the fake mic opened (a round's time zero), each chain
// count shown, and when each agent synthesis appeared.
function probe() {
  const md = navigator.mediaDevices;
  if (md?.getUserMedia) {
    const gum = md.getUserMedia.bind(md);
    md.getUserMedia = async (...args) => {
      const stream = await gum(...args);
      window.scMicAt = Date.now();
      return stream;
    };
  }
  window.scChain = [];
  window.scAgent = [];
  document.addEventListener('DOMContentLoaded', () => {
    const chain = document.getElementById('chain-count');
    if (chain) {
      new MutationObserver(() => {
        const m = chain.textContent.match(/auto (\d+)\//);
        if (m && window.scChain.at(-1) !== Number(m[1])) window.scChain.push(Number(m[1]));
      }).observe(chain, { childList: true, subtree: true, characterData: true });
    }
    const list = document.getElementById('messages');
    if (!list) return;
    const seen = new WeakSet();
    new MutationObserver(() => {
      for (const li of list.querySelectorAll('li.msg.agent')) {
        if (seen.has(li)) continue;
        seen.add(li);
        window.scAgent.push({ persona: li.querySelector('.meta strong')?.textContent.split(' ').at(-1), at: Date.now() });
      }
    }).observe(list, { childList: true, subtree: true });
  });
}

const messages = (page) => page.locator('#messages li.msg').evaluateAll((els) => els.map((li) => {
  const kind = [...li.classList].find((c) => c !== 'msg');
  const sender = li.querySelector('strong')?.textContent;
  return {
    kind, sender, persona: kind === 'agent' ? sender?.split(' ').at(-1) : undefined,
    body: li.querySelector('.body')?.textContent, model: li.querySelector('.model')?.textContent,
    effort: li.querySelector('.effort')?.textContent, ts: Date.parse(li.querySelector('time')?.dateTime),
  };
}));

// watchCache notes when each file of a daemon's workspace caches first appears: a turn fetched it.
function watchCache(dir) {
  const seen = new Map();
  const scan = (d, inCache) => {
    let entries = [];
    try { entries = readdirSync(d, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const p = join(d, e.name);
      if (e.isDirectory()) scan(p, inCache || e.name === 'workspace-cache');
      else if (inCache && !seen.has(p)) {
        try { seen.set(p, { ts: Date.now(), size: statSync(p).size }); } catch { /* evicted meanwhile */ }
      }
    }
  };
  const timer = setInterval(() => scan(dir, false), 200);
  return { seen, stop: () => clearInterval(timer) };
}

// stop ends a daemon; one deaf to SIGTERM for 30 s gets SIGKILL, so cleanup still runs.
async function stop(d) {
  d.proc.kill('SIGTERM');
  const timer = setTimeout(() => d.proc.kill('SIGKILL'), 30_000);
  await d.exited;
  clearTimeout(timer);
  d.stopped = true;
  removeSandbox(d.sandbox);
}

// sentence makes the long-session filler: varied, dull, deterministic.
const topics = ['the cache', 'the login page', 'the export', 'the release notes', 'the mobile layout', 'the audit log', 'the quota', 'the import preview'];
const verbs = ['needs a test for', 'should log', 'breaks on', 'is slow on', 'must refuse', 'could reuse'];
const objects = ['an empty room', 'a long name', 'a phone', 'two tabs', 'a slow network', 'an old archive'];
const sentence = (i) => `Point ${i}: ${topics[i % topics.length]} ${verbs[(i * 7) % verbs.length]} ${objects[(i * 5) % objects.length]}.`;

test(plan.name, async ({ request }) => {
  rmSync(micDir, { recursive: true, force: true });
  mkdirSync(micDir, { recursive: true });
  const result = { name: plan.name, model: plan.max_model, rounds: [], presses: [], turns: [], requests: 0 };
  const browsers = {};
  const pages = {};
  const daemons = {};
  const monitor = {};
  const caches = {};
  const roomPath = new URL(plan.url).pathname.match(/^\/r\/([^/]+)/);
  let room = roomPath?.[1];
  const keep = Boolean(room);
  const requests = () => Object.values(daemons).reduce((n, d) => n + messagesRequests(d), 0);
  const guard = () => {
    const n = requests();
    if (n > plan.max_requests) throw new Error(`budget exceeded: ${n} Claude requests, budget ${plan.max_requests}`);
  };
  // collect reads a daemon's monitoring (turn causes, models, context) while it still runs.
  const collect = async (by) => {
    try {
      monitor[by] = (await (await adminAPI(request, daemons[by].log)).get('/api/monitor/turns')).turns;
    } catch (e) {
      console.log(`monitoring of ${by}'s daemon unread: ${e.message}`);
    }
  };
  let personasSet = false;
  let first;
  try {
    if (!room) {
      const res = await request.post('/api/rooms', { headers: admin, data: { name: 'Scenario ' + plan.name } });
      expect(res.status(), await res.text()).toBe(201);
      room = (await res.json()).id;
    }
    result.room = room;
    const api = `/api/rooms/${room}`;
    for (const name of plan.participants) {
      if (plan.speakers.includes(name)) point(name, 1);
      browsers[name] = await launch(plan.speakers.includes(name) ? join(micDir, `${name}.wav`) : undefined);
      const inv = await (await request.post(`${api}/invites`, { headers: admin })).json();
      const ctx = await browsers[name].newContext();
      await ctx.addInitScript(snoozePush, snoozeKey);
      await ctx.addInitScript(probe);
      const page = await ctx.newPage();
      const url = new URL(inv.url);
      await page.goto(url.pathname + url.hash);
      await page.getByLabel('Your name').fill(name);
      await page.getByRole('button', { name: 'Join the room' }).click();
      await expect(page.locator('#me')).toHaveText(name);
      pages[name] = page;
    }
    // The first participant's page is the one that watches the room for the record.
    first = pages[plan.participants[0]];

    const setPersonas = async (page) => {
      const agent = async () => (await page.request.get(`${api}/agent`)).json();
      let st = await agent();
      const lib = await (await page.request.get(`${api}/templates`)).json();
      for (const p of plan.personas) {
        let cur = st.personas.find((x) => x.name === p.name);
        if (!cur) {
          const t = lib.find((x) => x.preset === p.template || x.title === p.template);
          expect(t, `template ${p.template} in the library`).toBeTruthy();
          const res = await page.request.post(`${api}/personas`, { data: { template_id: t.id } });
          expect(res.status(), await res.text()).toBe(201);
          st = await agent();
          cur = st.personas.find((x) => x.name === p.name);
          expect(cur, `template ${p.template} gives a persona named ${p.name}`).toBeTruthy();
        }
        const data = { model: p.model, effort: p.effort, every_n: p.every_n };
        if (p.text) data.text = p.text;
        const res = await page.request.patch(`${api}/personas/${cur.id}`, { data });
        expect(res.status(), await res.text()).toBe(200);
      }
      for (const p of plan.personas) {
        await expect(first.locator(`button.persona[data-name="${p.name}"] .led`)).toHaveAttribute('aria-label', `${p.name} ready`, { timeout: 300_000 });
      }
    };

    let round = 0;
    for (const [i, step] of plan.steps.entries()) {
      console.log(`step ${i + 1}: ${step.kind}`);
      switch (step.kind) {
        case 'voice': {
          round++;
          const rnd = plan.rounds[round - 1];
          for (const name of plan.speakers) point(name, round);
          if (round === 1) {
            await Promise.all(plan.speakers.map((name) => joinAudio(pages[name])));
          } else {
            // One at a time, as people do: the audio room never empties.
            for (const name of plan.speakers) {
              await audioItem(pages[name], 'Leave audio');
              await expect(pages[name].getByRole('button', { name: 'Join audio' })).toBeVisible();
              await joinAudio(pages[name]);
            }
          }
          const micAt = {};
          for (const name of plan.speakers) micAt[name] = await pages[name].evaluate(() => window.scMicAt);
          await expect(first.locator('#stt-status')).toHaveText(/^\d+ voices? (is|are) being written down/, { timeout: 30_000 });
          const lastEnd = Math.max(...rnd.lines.map((l) => l.end));
          // The recorded speech plays in real time: wait for its end. Not an assertion.
          await first.waitForTimeout(Math.max(0, Math.max(...Object.values(micAt)) + lastEnd * 1000 - Date.now()));
          // The worker lags behind speech: wait until no line has landed for 15 s, 2 minutes at most.
          const count = () => first.locator('#messages li.msg.voice').count();
          let n = await count();
          for (let quiet = Date.now(), cap = Date.now() + 120_000; Date.now() - quiet < 15_000 && Date.now() < cap;) {
            await first.waitForTimeout(2000); // polls the voice lines, bounded by the cap
            const c = await count();
            if (c !== n) [n, quiet] = [c, Date.now()];
          }
          result.rounds.push({ round, micAt });
          break;
        }
        case 'daemon': {
          const env = { GRILL_ROOM_MAX_MODEL: plan.max_model, GRILL_ROOM_MAX_EFFORT: plan.max_effort };
          if (plan.fake_agent) {
            // The fake claude in the fake sandbox image (make sandbox-fake-image): no Claude request.
            const dir = join(plan.run, 'fake-claude', step.by);
            mkdirSync(dir, { recursive: true });
            writeFileSync(join(dir, '.credentials.json'),
              JSON.stringify({ claudeAiOauth: { accessToken: 'scenario-fake', expiresAt: Date.now() + 86400e3 } }), { mode: 0o600 });
            Object.assign(env, { CLAUDE_CONFIG_DIR: dir, GRILL_ROOM_SANDBOX_IMAGE: process.env.GRILL_ROOM_SANDBOX_FAKE_IMAGE || 'grill-room-sandbox:fake' });
          }
          const d = liveDaemon(join(plan.run, 'daemons', step.by), `${new URL(plan.url).origin}/r/${room}`, env);
          daemons[step.by] = d;
          caches[step.by] = watchCache(join(plan.run, 'daemons', step.by, 'state'));
          await pairDaemon(pages[step.by], d);
          if (!personasSet) {
            await setPersonas(pages[step.by]);
            personasSet = true;
          }
          break;
        }
        case 'stop_daemon': {
          const d = daemons[step.by];
          await collect(step.by);
          await stop(d);
          break;
        }
        case 'upload': {
          const name = basename(step.file);
          await upload(pages[step.by], name, mime[extname(name)] || 'application/octet-stream', readFileSync(join(plan.dir, step.file)));
          await expect(first.locator('#documents')).toContainText(name);
          break;
        }
        case 'annotate': {
          const page = pages[step.by];
          await page.locator('#documents').getByRole('button', { name: step.doc }).first().click();
          await annotateSelection(page, step.quote, step.comment);
          await expect(first.locator('#annot-list')).toContainText(step.comment);
          break;
        }
        case 'say': {
          await pages[step.by].locator('#body').fill(step.text);
          await pages[step.by].locator('#body').press('Enter');
          await expect(first.locator('#messages')).toContainText(step.text.slice(0, 40));
          break;
        }
        case 'press': {
          guard();
          const index = (await first.evaluate(() => window.scAgent)).filter((s) => s.persona === step.persona).length;
          result.presses.push({ persona: step.persona, by: step.by, at: Date.now(), index });
          await pages[step.by].getByRole('button', { name: 'Ask ' + step.persona }).click();
          break;
        }
        case 'wait': {
          if (step.seconds) {
            await first.waitForTimeout(step.seconds * 1000); // the script's own pause, part of the session
            break;
          }
          const until = Date.now() + step.timeout * 1000;
          while ((await first.locator('#messages li.msg.agent').count()) < step.turns) {
            guard();
            if (Date.now() > until) throw new Error(`waited ${step.timeout} s for ${step.turns} syntheses`);
            await first.waitForTimeout(1000); // polls the syntheses, bounded by the step's timeout
          }
          break;
        }
        case 'flood': {
          // The fast driver of a long session: lines posted through the API, eight at a time.
          let next = 0;
          const worker = async () => {
            for (let n = next++; n < step.messages; n = next++) {
              const by = step.by[n % step.by.length];
              const res = await pages[by].request.post(`${api}/messages`, { data: { body: sentence(n + 1) } });
              expect(res.status(), await res.text()).toBe(201);
            }
          };
          await Promise.all(Array.from({ length: 8 }, worker));
          await expect(first.locator('#messages')).toContainText(sentence(step.messages).slice(0, 30), { timeout: 60_000 });
          break;
        }
        case 'handover': {
          const label = `Make ${step.to} the agent host`;
          let clicked = false;
          for (const page of Object.values(pages)) {
            const b = page.getByRole('button', { name: label });
            if (await b.count()) {
              await b.click();
              clicked = true;
              break;
            }
          }
          expect(clicked, `a host page offers "${label}"`).toBe(true);
          await expect(first.locator('#participants li', { hasText: step.to }).locator('.host-badge')).toHaveAttribute('aria-label', 'host');
          break;
        }
        default:
          throw new Error('unknown step ' + step.kind);
      }
    }
  } catch (e) {
    result.error = e.message.split('\n')[0];
    throw e;
  } finally {
    if (first) {
      result.messages = await messages(first).catch(() => []);
      result.agentSeen = await first.evaluate(() => window.scAgent).catch(() => []);
      result.chainSeen = await first.evaluate(() => window.scChain).catch(() => []);
    }
    for (const [by, d] of Object.entries(daemons)) {
      if (d.stopped) continue;
      await collect(by);
      await stop(d);
    }
    for (const [by, d] of Object.entries(daemons)) {
      caches[by].stop();
      const log = logLines(d);
      // The supervisor logs ISO times; a single-room daemon logged epoch seconds.
      const at = (l) => (typeof l.ts === 'number' ? l.ts * 1000 : Date.parse(l.ts));
      const started = log.filter((l) => l.msg === 'turn_started');
      for (const [i, s] of started.entries()) {
        const done = log.find((l) => l.msg === 'turn_done' && l.turn === s.turn);
        const mon = (monitor[by] || []).find((t) => t.turn_id === s.turn) || {};
        // A file belongs to the last turn started before it appeared.
        const next = started[i + 1] ? at(started[i + 1]) : Infinity;
        const files = [...caches[by].seen.values()].filter((f) => f.ts >= at(s) && f.ts < next);
        result.turns.push({
          daemon: by, turn: s.turn, persona: s.persona, resume: s.resume,
          status: mon.status || (done && !done.error ? 'ok' : 'failed'), error: done?.error || mon.error,
          model: mon.model, effort: mon.effort, cause: mon.cause, duration_ms: mon.duration_ms ?? (done ? Math.round(at(done) - at(s)) : undefined),
          queue_ms: mon.queue_ms, context_tokens: mon.context_tokens, context_window: mon.context_window,
          fetched_files: files.length, fetched_bytes: files.reduce((n, f) => n + f.size, 0),
        });
      }
    }
    result.requests = Object.values(daemons).reduce((n, d) => n + messagesRequests(d), 0);
    writeFileSync(join(plan.run, 'result.json'), JSON.stringify(result, null, 1) + '\n');
    if (room && !keep) await request.delete(`/api/rooms/${room}`, { headers: admin });
    if (Object.keys(pages).length) await dropProfiles(request, Object.values(pages)).catch((e) => console.log('profiles:', e.message));
    for (const b of Object.values(browsers)) await b.close();
  }
});
