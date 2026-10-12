# Scenarios

`scenarios/` replays a scripted grill session against a running instance: synthetic voices on fake microphones, chat lines, uploads, annotations, persona presses, a host hand-over. Real daemons on this machine run the agent turns. The run ends with a report that says pass or fail for each assertion. Use it for demos, for a non-regression pass on the lab before a release, and for tuning the STT and agent thresholds of spec 003.

## Run one

You need `uv`, `pnpm` with `e2e/` installed (`cd e2e && pnpm install`), the installed Chrome, Docker, `make build`, a Claude Code login on this machine, and the instance's admin token as `GRILL_ROOM_ADMIN_TOKEN` in the repo's `.env`.

```sh
cd scenarios
uv run grill-scenario two-persona-chain.yaml --url https://grill-room.<domain>
```

Before anything starts, the tool prints how many Claude requests the run should spend and the hard stop, then asks. `--yes` skips the question. The driver stops the run once the daemons have spent more than the budget line allows.

| Option | Does |
|---|---|
| `--url` | the instance, or one of its rooms (`/r/<id>`). With an instance, the run creates its room and deletes it after; a given room is kept |
| `--model opus` | runs every persona on that model instead of the file's |
| `--check` | validates, prints the budget, synthesises the audio, runs nothing |
| `--fake-agent` | runs turns on the fake claude image (`make sandbox-fake-image`): no request, so content assertions fail. For trying a scenario's steps for free |

A run writes `scenarios/out/<scenario>-<time>/`: `report.md`, `report.json`, the driver's `result.json`, the audio, and the daemons' state and logs. Voice models and synthesised lines are cached in `scenarios/.cache/`. Neither is committed.

The run deletes its room and the profiles its participants got. It stops its daemons, which remove their sandboxes.

## The library

| File | Covers | Requests (haiku) |
|---|---|---|
| `dark-mode-grill.yaml` | spike S5's 2.5 min FR/EN session: 18 lines, a language switch, an overlap, an annotation, one Grill turn revising `plan.md` | 4 |
| `two-persona-chain.yaml` | Grill calls the Secretary with `@Secretary`, then the Secretary runs again on its every-round trigger | 6 |
| `long-session.yaml` | 480 chat lines through the API in seconds: three transcript chunks, a word found in the first one, the second turn fetching only the chunk that grew, context use | 3 to 4 |
| `host-handover.yaml` | Claire's daemon runs a turn, Marc pairs his own, Claire hands him the host role and stops hers, and Grill recalls her word from the resumed session on Marc's machine | 2 |

## Writing one

A scenario is YAML. `grill_scenarios/schema.py` defines it, and the tool refuses a file that does not validate, naming the field.

```yaml
name: Host hand-over
budget: {max_requests: 3}          # the hard stop
gap: 1.2                           # default silence between lines, seconds
participants:
  - name: Claire
    voices: {fr: {model: fr_FR-siwis-medium}}   # one Piper voice per language spoken
  - name: Marc
    voices: {en: {model: en_US-joe-medium}, fr: {model: fr_FR-mls-medium, speaker: 84}}
personas:
  # template: a preset key (secretary) or library title, for a persona the room lacks.
  # effort (low to max) is optional; requests_per_turn: 1 with no tool, one more per round of
  # tool calls. The budget comes from it.
  - {name: Grill, model: haiku, every_n: 0, requests_per_turn: 1}
steps:
  - daemon: {by: Claire}           # starts a supervisor here and pairs it from Claire's page
  - voice:                         # one round: every speaker gets a track on a shared timeline
      lead: 10                     # silence first, so the STT worker subscribes in time
      lines:
        - {by: Claire, lang: fr, text: "..."}
        - {by: Marc, lang: en, gap: -1.5, text: "..."}   # negative gap: talks over Claire
  - upload: {by: Claire, file: plan.md}                  # relative to the scenario file
  - annotate: {by: Marc, doc: plan.md, quote: "...", comment: "..."}
  - say: {by: Claire, text: "..."}                       # a chat line; @name in it calls a persona
  - press: {by: Claire, persona: Grill}
  - wait: {turns: 1, timeout: 600}                       # until the room holds 1 synthesis; or {seconds: 5}
  - flood: {by: [Claire, Marc], messages: 450}           # filler lines through the API
  - handover: {to: Marc}                                 # the host clicks "Make Marc the agent host"
  - stop_daemon: {by: Claire}
assertions:
  turns: {Grill: 2}                # exact completed turns per persona; the budget estimate uses it
  voice: {max_wer: 0.2, min_attribution: 1.0, min_delivered: 1.0, max_latency_s: 10}
  syntheses:                       # the nth synthesis of a persona
    - {persona: Grill, nth: 2, contains: ["OSPREY-42"], not_contains: ["@secretary"],
       resumed: true, ran_on: Marc, cause: "pressed by"}
  chain: {min: 1, max: 1}          # highest chain count the room showed
  max_turn_s: 180                  # press to synthesis
  min_context_pct: 1               # context use of the last turn
  max_fetched_files_after_first: 2 # workspace files each later turn fetched
```

`contains` and `not_contains` are case-insensitive regular expressions. A human chat line that writes `@Secretary` queues the Secretary by itself, so a line asking Grill to call it should spell the at sign out.

The estimate is each persona's asserted turns times its `requests_per_turn`. A file whose estimate exceeds `budget.max_requests` does not load.

## Voices

`scenarios/VOICES.md` lists every Piper voice a scenario may use, with the license from its model card. The tool refuses a voice that is not in that table, or whose license is not CC0, CC-BY or public domain, before it synthesises anything. Add a voice by adding its row. Check the model card first.

Lines are synthesised at 16 kHz, the rate the STT worker reads, as in spike S5's rerun.

## How it works

`grill-scenario` (Python) validates the file, applies the license gate, prints the budget, synthesises the audio and writes `plan.json`. It then runs the Playwright driver `e2e/scenario/scenario.spec.js`, which reuses `e2e/`'s helpers: one Chrome per participant with its WAV as microphone, real daemons paired through their approval page, the supervisor's monitoring for each turn's model, cause and context. The driver only records what happened. The Python side scores `result.json`: WER and attribution against the script, latency from the end of speech to the line, and each assertion.

`make scenarios-test` runs ruff, mypy and the unit tests (schema, budget, license gate, timeline, scoring). CI runs it too. It spends no request.
