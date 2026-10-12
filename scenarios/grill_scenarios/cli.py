"""grill-scenario: replays a YAML scenario against a grill-room instance and reports on it (004-AC16)."""

import argparse
import json
import os
import shutil
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path

import structlog
from pydantic import ValidationError

from . import audio, report, schema, score, voices

HERE = Path(__file__).resolve().parent.parent
E2E = HERE.parent / "e2e"
MODEL_RANK = list(schema.MODELS)

log = structlog.get_logger()


def setup_logging() -> None:
    renderer = structlog.dev.ConsoleRenderer() if sys.stderr.isatty() else structlog.processors.JSONRenderer()
    structlog.configure(
        processors=[
            structlog.processors.add_log_level,
            structlog.processors.TimeStamper(fmt="iso"),
            renderer,
        ],
        logger_factory=structlog.PrintLoggerFactory(sys.stderr),
    )


def confirm(text: str, yes: bool) -> bool:
    print(text, flush=True)
    if yes:
        return True
    if not sys.stdin.isatty():
        print("Not a terminal: pass --yes to run without asking.", file=sys.stderr)
        return False
    return input("Run it? [y/N] ").strip().lower() in ("y", "yes")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="grill-scenario", description=__doc__)
    ap.add_argument("scenario", type=Path)
    ap.add_argument(
        "--url",
        required=True,
        help="the instance (https://host) or one of its rooms (https://host/r/<id>), kept after the run",
    )
    ap.add_argument("--model", choices=schema.MODELS, help="run every persona on this model instead")
    ap.add_argument("--yes", action="store_true", help="do not ask before spending Claude requests")
    ap.add_argument("--cache", type=Path, default=HERE / ".cache", help="voice models and synthesised clips")
    ap.add_argument("--out", type=Path, default=HERE / "out", help="where each run writes its folder")
    ap.add_argument(
        "--fake-agent",
        action="store_true",
        help="run turns with the fake claude image (make sandbox-fake-image): no Claude request",
    )
    ap.add_argument(
        "--check", action="store_true", help="validate, print the budget and synthesise, run nothing"
    )
    args = ap.parse_args(argv)
    setup_logging()

    try:
        s = schema.load(args.scenario)
        if args.model:
            s = schema.with_model(s, args.model)
    except (ValidationError, ValueError, OSError) as e:
        print(f"{args.scenario}: {e}", file=sys.stderr)
        return 2
    bad = voices.refused(s.voices(), voices.listed(voices.VOICES_MD.read_text(encoding="utf-8")))
    if bad:
        print("Refused voices (004-AC18):\n  " + "\n  ".join(bad), file=sys.stderr)
        return 2

    est = schema.estimate(s)
    models = sorted({p.model for p in s.personas}, key=MODEL_RANK.index)
    turns = ", ".join(f"{n} {name}" for name, n in s.assertions.turns.items())
    text = (
        f"Scenario {s.name!r}: about {est} real Claude requests ({turns} turns), "
        f"stopped at {s.budget.max_requests}; model {', '.join(models)}."
    )
    if args.fake_agent:
        text += " With --fake-agent: no request; agent assertions will fail."
    if not confirm(text, args.yes or args.check or args.fake_agent):
        return 1

    run = args.out / f"{args.scenario.stem}-{datetime.now(UTC):%Y%m%dT%H%M%SZ}"
    run.mkdir(parents=True)
    rounds = audio.render(s, run / "audio", audio.piper(args.cache))
    log.info("audio_ready", rounds=len(rounds), seconds=[r["seconds"] for r in rounds])
    if args.check:
        return 0

    plan = {
        "fake_agent": args.fake_agent,
        "name": s.name,
        "url": args.url.rstrip("/"),
        "dir": str(args.scenario.resolve().parent),
        "run": str(run),
        "max_requests": s.budget.max_requests,
        "max_model": models[-1],
        # The daemon's cap: high unless a persona asks for more, so no turn runs clamped.
        "max_effort": max(["high"] + [p.effort for p in s.personas if p.effort], key=schema.EFFORTS.index),
        "participants": [p.name for p in s.participants],
        "speakers": [p.name for p in s.participants if p.voices],
        "personas": [p.model_dump() for p in s.personas],
        "steps": [{"kind": st.kind, **getattr(st, st.kind).model_dump()} for st in s.steps],
        "rounds": rounds,
    }
    (run / "plan.json").write_text(json.dumps(plan, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    pnpm = shutil.which("pnpm")
    if not pnpm:
        print("pnpm is not installed: the driver runs on e2e/'s Playwright", file=sys.stderr)
        return 2
    log.info("driver_started", run=str(run))
    proc = subprocess.run(
        [pnpm, "exec", "playwright", "test", "-c", "scenario/playwright.config.js"],
        cwd=E2E,
        env={**os.environ, "GRILL_SCENARIO_PLAN": str(run / "plan.json")},
        check=False,
    )
    log.info("driver_done", exit_code=proc.returncode)
    result_file = run / "result.json"
    if not result_file.exists():
        print(f"the driver wrote no {result_file}", file=sys.stderr)
        return 1
    res = json.loads(result_file.read_text(encoding="utf-8"))
    r = score.check(s, rounds, res)
    (run / "report.json").write_text(json.dumps(r, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    md = report.markdown(s.name, plan["url"], est, s.budget.max_requests, r)
    (run / "report.md").write_text(md, encoding="utf-8")
    for c in r["checks"]:
        print(f"{'ok  ' if c['ok'] else 'FAIL'} {c['check']}: {c['detail']}")
    print(f"{'PASS' if r['ok'] else 'FAIL'}: {run / 'report.md'}")
    return 0 if r["ok"] and proc.returncode == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
