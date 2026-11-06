#!/usr/bin/env python3
"""Self-check for analyze.py on a synthetic capture: `uv run ./test_analyze.py` (stdlib only)."""

import json
import subprocess
import sys
import tempfile
import wave
from pathlib import Path

HERE = Path(__file__).resolve().parent
SEC, EDGE = 16000 * 2, 2


def capture(d: Path) -> None:
    rtp, pay_a, pay_b = [], bytearray(), bytearray()
    for i in range(100):  # stream A, PCMU, dominant speaker 2886 then 2890
        csrc = [2886] if i < 50 else [2890, 2886]
        rtp.append({"t": i * 20, "ssrc": 10, "seq": 65500 + i & 0xFFFF, "ts": 4294960000 + i * 160 & 0xFFFFFFFF,
                    "pt": 0, "m": False, "csrc": csrc, "ext_profile": None, "ext": "", "plen": 160})
        pay_a += bytes([0x10]) * 160  # loud
    for i in range(50):  # stream B, PCMA, starts 1 s in, no CSRC
        rtp.append({"t": 1000 + i * 20, "ssrc": 20, "seq": i, "ts": i * 160, "pt": 8, "m": False,
                    "csrc": [], "ext_profile": 0xBEDE, "ext": "10ff", "plen": 160})
        pay_b += bytes([0x55]) * 160
    (d / "rtp.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rtp))
    (d / "ssrc-10.pay").write_bytes(pay_a)
    (d / "ssrc-20.pay").write_bytes(pay_b)
    roster = {"participants": {"8:orgid:x": {"displayName": "Alice",
                                             "endpointDetails": [{"mediaStreams": [{"sourceId": 2886}]}]}}}
    (d / "trouter.jsonl").write_text(json.dumps({"t": 5, "path": "/conversation/rosterUpdate/", "body": roster}) + "\n")
    (d / "rtcp.jsonl").write_text(json.dumps({"t": 9, "leg": "outgoing", "pt": 206, "fmt": 15, "hex": "8f"}) + "\n")


def pkt(t: int, ssrc: int, seq: int, ts: int, csrc: list[int]) -> dict[str, object]:
    return {"t": t, "ssrc": ssrc, "seq": seq & 0xFFFF, "ts": ts & 0xFFFFFFFF, "pt": 0, "m": False,
            "csrc": csrc, "ext_profile": None, "ext": "", "plen": 160}


def hard_cases(d: Path) -> None:
    """Shapes from review: a 70 s gap, a late packet right after a timestamp wrap, reused seq numbers,
    and a cut last line."""
    rows = [pkt(0, 30, 0, 0, [1]), pkt(70_000, 30, 1, 70 * 8000 + 2**31, [1])]  # gap + sender clock jump
    base = 2**32 - 320
    rows += [pkt(0, 40, 5, base, [2]), pkt(20, 40, 6, base + 160, [2]), pkt(40, 40, 7, base + 320, [2]),
             pkt(80, 40, 9, base + 640, [2]), pkt(95, 40, 8, base + 480, [2]), pkt(100, 40, 10, base + 800, [2])]
    rows += [pkt(i * 20, 50, i % 3, i * 160, [3 if i < 6 else 4]) for i in range(12)]  # seq reused
    text = "".join(json.dumps(r) + "\n" for r in rows) + '{"t": 1, "ssrc": 50, "se'
    (d / "rtp.jsonl").write_text(text)
    (d / "ssrc-30.pay").write_bytes(bytes([0x10]) * 320)
    (d / "ssrc-40.pay").write_bytes(bytes([0x10]) * 960)
    (d / "ssrc-50.pay").write_bytes(bytes([0x10]) * 1920)


def frames(p: Path) -> bytes:
    with wave.open(str(p)) as w:
        assert (w.getframerate(), w.getnchannels(), w.getsampwidth()) == (16000, 1, 2)
        return w.readframes(w.getnframes())


def main() -> int:
    with tempfile.TemporaryDirectory(prefix="s18-analyze-") as tmp:
        d = Path(tmp)
        capture(d)
        out = subprocess.run([sys.executable, "-I", str(HERE / "analyze.py"), str(d)], capture_output=True, text=True)
        assert out.returncode == 0, out.stderr
        a = d / "analysis"
        # A wraps seq and RTP ts mid-stream: 100 packets must still land contiguously (2 s at 16 kHz).
        assert len(frames(a / "ssrc-10.wav")) == 2 * SEC
        # B starts 1 s after the capture start and lasts 1 s: shared time origin. SEC is one second of
        # 16 kHz PCM16; EDGE drops the one sample the 8->16 kHz interpolation smears across a boundary.
        b = frames(a / "ssrc-20.wav")
        assert len(b) == 2 * SEC and not any(b[: SEC - EDGE]) and any(b[SEC:])
        # Dominant speaker = first CSRC: 2886 owns the first second, 2890 the second.
        c1, c2 = frames(a / "csrc-2886.wav"), frames(a / "csrc-2890.wav")
        assert any(c1[:SEC]) and not any(c1[SEC:])
        assert not any(c2[: SEC - EDGE]) and any(c2[SEC:])
        summary = (a / "summary.md").read_text()
        for needle in ("| Alice |", "2886", "/conversation/rosterUpdate/", "PT 206", "profile 0xbede"):
            assert needle in summary, needle
        assert json.loads((a / "speakers.json").read_text()) == {"csrc-2886": "Alice"}
        tracks = json.loads((a / "manifest.json").read_text())["tracks"]
        assert {t["id"] for t in tracks} == {"ssrc-10", "ssrc-20", "csrc-2886", "csrc-2890"}
    with tempfile.TemporaryDirectory(prefix="s18-analyze-hard-") as tmp:
        d = Path(tmp)
        hard_cases(d)
        out = subprocess.run([sys.executable, "-I", str(HERE / "analyze.py"), str(d)], capture_output=True, text=True)
        assert out.returncode == 0, out.stderr
        assert "skipped an unreadable line" in out.stderr
        a = d / "analysis"
        g = frames(a / "ssrc-30.wav")  # the packet after 70 s of silence is kept, at 70 s
        assert len(g) == 70 * SEC + 160 * 4 and any(g[70 * SEC :])
        w = frames(a / "ssrc-40.wav")  # 6 packets across the wrap; seq 8 arrives 35 ms late but its timestamp puts it back
        assert len(w) == 6 * 160 * 4 and all(w[i : i + 2] != b"\0\0" for i in range(0, len(w) - EDGE, 2))
        c3, c4 = frames(a / "csrc-3.wav"), frames(a / "csrc-4.wav")  # by packet, not by seq
        half = 6 * 160 * 4
        assert any(c3[: half - EDGE]) and not any(c3[half:]) and not any(c4[: half - EDGE]) and any(c4[half:])
    print("analyze.py self-check: ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
