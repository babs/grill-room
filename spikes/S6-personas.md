# S6: two personas on the lab

**Verdict: pass.** The S5 session ran on the lab with Grill (on demand, opus/high) and Secretary (every 2 rounds, opus/medium). Grill called `@Secretary` once and the room showed `Chain 1/25`. Secretary then ran a second time on its every-2-rounds trigger. All five syntheses name their persona, model and effort. A press during a running turn queued and ran next. A shared skill ran its script in one Grill turn. STT matches the S5 rerun. 12 real Claude requests, all Opus.

One assertion of mine was wrong: it expected English headings in the minutes, and Secretary writes in the room's language, French here. The minutes hold the four sections. I fixed the assertion and checked it against the recorded minutes. A rerun would have broken the budget.

## Setup

- Same script, voices, plan and lab node as S5 (`s5-simulated-session/`). Three browsers with fake mics, a real daemon on the dev machine paired through the approval page.
- Claire set both personas in the personas panel, as a participant would: Grill opus/high, Secretary added from the library at opus/medium, every 2 rounds. No `GRILL_ROOM_AGENT_MODEL`. The daemon's cap was opus/high, so nothing was clamped.
- `s6-personas/s6.spec.js` drives it and writes `result.json`. The stored prompts came from a read-only copy of the server's SQLite.

## Turns

| # | persona | trigger | ran | daemon turn | press to synthesis | requests | workspace fetched |
|---|---|---|---|---|---|---|---|
| 1 | Grill | button | opus 5.5, high | 33.4 s | 35.6 s | 3 | 3 files, 3.2 KB (full, empty cache) |
| 2 | Secretary | `@Secretary` in turn 1 | opus 5.5, medium | 20.1 s | 20.1 s after turn 1 | 3 | 2 files, 8.2 KB |
| 3 | Grill + skill | button, queued during turn 2 | opus 5.5, high | 5.7 s | 25.3 s (20.0 s queued) | 3 | 2 files, 8.5 KB |
| 4 | Grill | button | opus 5.5, high | 4.9 s | 7.3 s | 1 | 2 files, 6.4 KB |
| 5 | Secretary | every 2 rounds | opus 5.5, medium | 16.2 s | 15.9 s after turn 4 | 2 | 1 file, 7.0 KB |

- **Chain.** Claire asked Grill in plain words, with no `@`, to hand over to the Secretary at the end. Grill's synthesis ended with `@Secretary`. The chain count went `0/25` → `1/25` (the call) → `0/25` (Claire's next press) → `1/25` (every 2 rounds). The store records both Secretary turns with no participant: an agent queued them. Turn 4's synthesis names no one, so turn 5 came from the every-N trigger.
- **Queue.** Claire pressed Grill while Secretary was running. The queue line read "🔥 Grill, asked by Claire" and Grill's LED said "queued, position 1". Turn 3 started 0.08 s after turn 2 ended.
- **Skill.** `s6-stamp` was uploaded to the shared library, switched on for Grill before turn 3 and off after it. The daemon logged `turn_skills`, and `docs/stamp.md` holds the stamp only its script computes. It ran on Opus inside a Grill turn, so no separate haiku turn was needed.
- **Workspace.** Only turn 1 fetched the full tar. The plan upload never came back. Every later turn fetched the transcript day file whole (2.7, 4.5, 5.7, 6.3 and 7.0 KB), since each synthesis extends it, plus the documents written since. For a long day that cost grows with the transcript. The run charged a file to two turns when a queued turn started within a second of the previous one's end. The table is recomputed from the windows of turns 3 and 5, which did not overlap, and the 10 cached files add up to the same 33.2 KB. The spec now charges each file to one turn.

## Presence preamble (turn 2, Secretary, as stored)

```
## Who is here

Room: S6 dark mode grill
You: @Secretary, model opus, effort medium, on demand and every 2 rounds
Agent @Grill: model opus, effort high, idle, turns done: 1, on demand
  last said: "J'ai créé docs/synthese-mode-sombre.md. Il contient les 6 décisions de la séance, les…
Humans present: Claire, Marc, Emma
Host: Claire
Speaking now: nobody
Chain budget: 24 of 25 agent turns left
Write @name in your synthesis to give another agent a turn; it counts against the chain budget.
Writing @Secretary does nothing: self-call is off.
```

- Every field is right. Turn 5 has the same shape with `turns done: 3` and Grill's turn 4 line. Turn 1 lists Secretary as `idle, turns done: 0`. "Speaking now: nobody" is true because each turn ran after its round.
- Missing: the prompt says "Someone asked for your turn" even when an agent did, and never says why the persona runs (a call from Grill, or every 2 rounds). Secretary did the right job anyway. Naming the trigger is a cheap addition for spec 003.

## Secretary's output

`docs/minutes.md` reads as minutes: Résumé, Décisions (9, each with who decided), Actions (owner and task, "délai à confirmer" when none was said), Questions ouvertes. It marked two STT mishearings "(à confirmer)" with the likely words ("Art mode" for "dark mode", "All deliver" for "I'll deliver"). Turn 5 added the two round-2 decisions and moved the rollout question to Decisions.

## STT (regression check against the S5 rerun)

| | S5 rerun | S6 |
|---|---|---|
| WER all / fr / en | 0.08 / 0.07 / 0.11 | 0.08 / 0.06 / 0.10 |
| WER Claire / Marc / Emma | 0.03 / 0.11 / 0.15 | 0.02 / 0.11 / 0.13 |
| attribution | 18/18 | 18/18 |
| voice lines per script line | 1.00 | 1.00 |
| latency min / median / p95 / max | 3.5 / 4.7 / 5.9 / 6.6 s | 4.2 / 4.7 / 7.9 / 9.5 s |

No regression in accuracy or attribution. The tail latency is higher, but every line stayed under 10 s (spec 001 AC7). Emma's overlap line is the 9.5 s max.

## Budget and cleanup

- S6 used 12 real Claude requests, all Opus, out of 15. The phase's `make e2e-lab` run used 1 more, on haiku.
- The S6 room, the shared skill and the three participants' profiles are deleted. The spec now deletes the profiles itself, and so do the lab e2e tests. The daemon is stopped, with no sandbox container, network or volume left. Generated audio stays outside the repo.
