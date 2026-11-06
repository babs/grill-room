#!/usr/bin/env python3
"""S18: turns one `meeting-capture` run into the answers the spike needs.

usage: uv run ./analyze.py <capture-dir> [--lang fr]

Reads what the patched client wrote (rtp.jsonl, ssrc-<n>.pay, trouter.jsonl, rtcp.jsonl, *.sdp)
and writes <capture-dir>/analysis/:
- summary.md: streams seen (SSRC), contributing sources (CSRC) per packet, Trouter callback paths,
  roster entries that carry a source id, RTCP packet types;
- ssrc-<n>.wav: each received stream, 16 kHz mono PCM16, placed on the RTP clock (gaps are silence);
- csrc-<n>.wav: the received audio kept only while that source is the first CSRC, which [MS-RTP]
  defines as the dominant speaker of a mixer packet (CSRC values are MSIs): the per-speaker
  split if Teams sends one mixed stream;
- manifest.json: every WAV as a track for `python -m grill_stt.replay` (spec 007);
- speakers.json: track id to roster display name, where a source id maps to one participant.
Stdlib only, so it runs anywhere `python3` does.
"""

import argparse
import json
import sys
import wave
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any

PCMU, PCMA = 0, 8
RATE_IN, RATE_OUT = 8000, 16000


def ulaw(b: int) -> int:
    b = ~b & 0xFF
    t = (((b & 0x0F) << 3) + 0x84) << ((b & 0x70) >> 4)
    return 0x84 - t if b & 0x80 else t - 0x84


def alaw(b: int) -> int:
    b ^= 0x55
    t = (b & 0x0F) << 4
    seg = (b & 0x70) >> 4
    t = t + 8 if seg == 0 else (t + 0x108) << (seg - 1)
    return t if b & 0x80 else -t


DECODE = {PCMU: [ulaw(i) for i in range(256)], PCMA: [alaw(i) for i in range(256)]}


def rows(path: Path) -> list[dict[str, Any]]:
    """Every JSON line; an abnormal exit can leave a cut last line, which is skipped with a warning."""
    if not path.exists():
        return []
    out: list[dict[str, Any]] = []
    for n, line in enumerate(path.read_text(errors="replace").splitlines(), 1):
        try:
            out.append(json.loads(line))
        except json.JSONDecodeError:
            if line.strip():
                print(f"{path.name}:{n}: skipped an unreadable line", file=sys.stderr)
    return out


def upsample(pcm: list[int]) -> bytes:
    # ponytail: linear 8->16 kHz, enough for STT on telephone-band audio; use a real resampler if WER suffers
    out = bytearray()
    for i, s in enumerate(pcm):
        n = pcm[i + 1] if i + 1 < len(pcm) else s
        out += s.to_bytes(2, "little", signed=True) + ((s + n) // 2).to_bytes(2, "little", signed=True)
    return bytes(out)


def write_wav(path: Path, pcm: list[int]) -> None:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(RATE_OUT)
        w.writeframes(upsample(pcm))


def place(packets: list[dict[str, Any]], payload: bytes) -> tuple[list[int], list[int | None]]:
    """Decodes one stream onto the capture's time origin, so every WAV lines up with the others.

    Each packet goes where the signed RTP timestamp step from the previous packet puts it, so wraps
    and late packets land right. A step outside -2 s..+60 s (sender restart, clock jump) falls
    back to the packet's capture time. Returns the samples and each packet's offset, by index.
    """
    pcm: list[int] = []
    offsets: list[int | None] = [None] * len(packets)
    pos, last_ts, last_at = 0, None, 0
    for i, p in enumerate(packets):
        chunk = payload[pos : pos + p["plen"]]
        pos += p["plen"]
        table = DECODE.get(p["pt"])
        if table is None:
            continue
        wall = p["t"] * RATE_IN // 1000
        step = 0
        if last_ts is None:
            at = wall
        else:
            step = (p["ts"] - last_ts) & 0xFFFFFFFF
            if step >= 1 << 31:
                step -= 1 << 32
            at = last_at + step if -2 * RATE_IN <= step <= 60 * RATE_IN else wall
        if at < 0:
            continue
        if at > len(pcm):
            pcm.extend([0] * (at - len(pcm)))
        pcm[at : at + len(chunk)] = [table[b] for b in chunk]
        offsets[i] = at
        last_ts, last_at = p["ts"], at
    return pcm, offsets


def find_sources(node: Any, path: str = "") -> list[tuple[str, str, Any]]:
    """Every (owner displayName, key path, value) whose key names a media source: the roster-to-CSRC link."""
    hits: list[tuple[str, str, Any]] = []

    def walk(n: Any, p: str, owner: str) -> None:
        if isinstance(n, dict):
            owner = str(n.get("displayName") or owner)
            for k, v in n.items():
                kl = k.lower()
                if any(w in kl for w in ("sourceid", "msi", "csrc", "ssrc", "mediasource")) and not isinstance(
                    v, (dict, list)
                ):
                    hits.append((owner, f"{p}.{k}", v))
                walk(v, f"{p}.{k}", owner)
        elif isinstance(n, list):
            for i, v in enumerate(n):
                walk(v, f"{p}[{i}]", owner)

    walk(node, path, "")
    return hits


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("capture", type=Path)
    ap.add_argument("--lang", default="fr", help="language tag written to the replay manifest")
    a = ap.parse_args()
    cap: Path = a.capture
    rtp = rows(cap / "rtp.jsonl")
    if not rtp:
        print(f"no rtp.jsonl rows in {cap}: did the call get media?", file=sys.stderr)
    out = cap / "analysis"
    out.mkdir(exist_ok=True)

    by_ssrc: dict[int, list[dict[str, Any]]] = defaultdict(list)
    for p in rtp:
        by_ssrc[p["ssrc"]].append(p)

    md = [f"# S18 capture analysis: {cap.name}", ""]
    md += ["## Streams (SSRC)", "", "| SSRC | packets | PT | seconds of audio | packets with CSRC |", "|---|---|---|---|---|"]
    tracks: list[dict[str, str]] = []
    csrc_pcm: dict[int, list[int]] = defaultdict(list)
    csrc_count: Counter[int] = Counter()
    csrc_sets: Counter[tuple[int, ...]] = Counter()
    first_count: Counter[int] = Counter()
    for ssrc, pk in sorted(by_ssrc.items(), key=lambda kv: -len(kv[1])):
        payload_file = cap / f"ssrc-{ssrc}.pay"
        payload = payload_file.read_bytes() if payload_file.exists() else b""
        pcm, offsets = place(pk, payload)
        with_csrc = sum(1 for p in pk if p.get("csrc"))
        pts = ",".join(str(x) for x in sorted({p["pt"] for p in pk}))
        seconds = sum(p["plen"] for p in pk if p["pt"] in DECODE) / RATE_IN
        md.append(f"| {ssrc} | {len(pk)} | {pts} | {seconds:.1f} | {with_csrc} |")
        if pcm:
            name = f"ssrc-{ssrc}.wav"
            write_wav(out / name, pcm)
            tracks.append({"id": f"ssrc-{ssrc}", "set": "s18", "item": f"ssrc-{ssrc}", "lang": a.lang, "wav": name})
        for p, at in zip(pk, offsets, strict=True):
            cs = tuple(p.get("csrc") or ())
            if not cs:
                continue
            csrc_sets[cs] += 1
            csrc_count.update(cs)
            first_count[cs[0]] += 1
            if at is None:
                continue
            buf = csrc_pcm[cs[0]]
            if len(buf) < len(pcm):
                buf.extend([0] * (len(pcm) - len(buf)))
            seg = pcm[at : at + p["plen"]]  # shorter when the payload file was cut by an abnormal exit
            buf[at : at + len(seg)] = seg

    md += ["", "## Contributing sources (CSRC, MSI values per MS-RTP)", ""]
    if csrc_count:
        md += ["| CSRC | packets listing it | packets as dominant (first) |", "|---|---|---|"]
        md += [f"| {c} | {n} | {first_count[c]} |" for c, n in csrc_count.most_common()]
        md += ["", "Most frequent CSRC sets:", ""]
        md += [f"- {list(s)}: {n}" for s, n in csrc_sets.most_common(10)]
        for c, pcm in csrc_pcm.items():
            name = f"csrc-{c}.wav"
            write_wav(out / name, pcm)
            tracks.append({"id": f"csrc-{c}", "set": "s18-csrc", "item": f"csrc-{c}", "lang": a.lang, "wav": name})
    else:
        md.append("No packet carried a CSRC list.")

    exts = Counter(p.get("ext_profile") for p in rtp if p.get("ext_profile") is not None)
    md += ["", "## RTP header extensions", ""]
    md += [f"- profile 0x{prof:04x}: {n} packets" for prof, n in exts.items()] or ["None."]

    tr = rows(cap / "trouter.jsonl")
    paths = Counter(r.get("path") or r.get("kind", "?") for r in tr)
    md += ["", "## Trouter frames by callback path", "", "| path | frames |", "|---|---|"]
    md += [f"| `{p}` | {n} |" for p, n in paths.most_common()]
    src_hits: list[tuple[str, str, Any]] = []
    for r in tr:
        if isinstance(r.get("body"), (dict, list)):
            src_hits += find_sources(r["body"], r.get("path") or "")
    md += ["", "## Source ids found in Trouter bodies (roster, csrcInfo, dominantSpeakerInfo)", ""]
    if src_hits:
        md += ["| owner | key | value |", "|---|---|---|"]
        md += [f"| {o or '?'} | `{k[-80:]}` | {v} |" for o, k, v in dict.fromkeys(src_hits)]
    else:
        md.append("None: look at trouter.jsonl by hand for the roster shape.")

    # A source id (MSI) seen with exactly one owner names that CSRC's speaker in the transcript.
    owners: dict[str, set[str]] = defaultdict(set)
    for o, _, v in src_hits:
        if o and str(v).isdigit():
            owners[str(v)].add(o)
    speakers = {f"csrc-{v}": next(iter(o)) for v, o in owners.items() if len(o) == 1}
    (out / "speakers.json").write_text(json.dumps(speakers, indent=1, ensure_ascii=False) + "\n")

    rtcp = rows(cap / "rtcp.jsonl")
    md += ["", "## RTCP packet types", ""]
    md += [f"- PT {pt}: {n}" for pt, n in Counter(r["pt"] for r in rtcp).most_common()] or ["None captured."]

    (out / "manifest.json").write_text(json.dumps({"tracks": tracks}, indent=1) + "\n")
    (out / "summary.md").write_text("\n".join(md) + "\n")
    print("\n".join(md))
    print(f"\nwrote {out}/summary.md, {len(tracks)} WAV tracks and manifest.json")
    return 0


if __name__ == "__main__":
    sys.exit(main())
