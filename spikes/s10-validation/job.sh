#!/usr/bin/env bash
# Runs inside a lab Job, everything on the shared volume $D.
# usage: job.sh setup                  venv, transcribe.cpp build, models, corpora (once)
#        job.sh run "<run.py args>"... one run per argument; "c2 <engine> <set> <gl>" runs two
#                                       processes at once, 2 threads each, one speaker track each
#        job.sh score                  score.py over $D/out
set -euo pipefail

export D=/data HF_HOME=/data/hf UV_CACHE_DIR=/data/uv UV_LINK_MODE=copy HOME=/tmp PYTHONUNBUFFERED=1
readonly PY="$D/venv/bin/python" TCPP_TAG=v0.4.0

setup() {
    [[ -x "$PY" ]] || uv venv -q -p 3.12 "$D/venv"
    uv pip install -q --python "$PY" "faster-whisper==1.2.1" "av>=16,<17" "sherpa-onnx==1.13.8" "onnxruntime==1.31.0" \
        "piper-tts==1.8.0" "soundfile==0.14.0" "pyarrow==26.0.0" "huggingface_hub==2.2.0" soxr cmake
    if [[ ! -x "$D/tcpp/build/bin/transcribe-cli" ]]; then
        rm -rf "$D/tcpp"
        git clone -q --depth 1 --branch "$TCPP_TAG" --recurse-submodules --shallow-submodules \
            https://github.com/handy-computer/transcribe.cpp.git "$D/tcpp"
        # Same flags as the workstation bench: AVX2 only, like the lab CPUs.
        "$D/venv/bin/cmake" -S "$D/tcpp" -B "$D/tcpp/build" -DCMAKE_BUILD_TYPE=Release -DGGML_NATIVE=OFF -DGGML_AVX=ON \
            -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON -DGGML_AVX_VNNI=OFF -DGGML_AVX512=OFF -DGGML_BMI2=ON >/dev/null
        "$D/venv/bin/cmake" --build "$D/tcpp/build" -j 4 --target transcribe-cli >"$D/tcpp-build.log" 2>&1
    fi
    "$PY" - <<'PY'
from faster_whisper import download_model
from huggingface_hub import hf_hub_download, snapshot_download

for repo, fn in [("parakeet-ultra-gguf", "parakeet-ultra-Q8_0.gguf"), ("Qwen3-ASR-0.6B-gguf", "Qwen3-ASR-0.6B-Q8_0.gguf")]:
    print(hf_hub_download(f"handy-computer/{repo}", fn), flush=True)
print(download_model("small"), flush=True)
print(snapshot_download("Alexqwqw/parakeet-ultra-sherpa-onnx-int8", local_dir="/data/sherpa/ultra",
                        allow_patterns=["*.onnx", "tokens.txt", "bpe.vocab"]), flush=True)
PY
    sha256sum "$D"/sherpa/ultra/*.onnx "$D/sherpa/ultra/tokens.txt" "$D/sherpa/ultra/bpe.vocab"
    mkdir -p "$D/corpus"
    cp /bench/glossary.json "$D/corpus/glossary.json"
    for set in piper devoxx summre; do
        [[ -f "$D/corpus/$set.json" ]] || "$PY" /bench/prep.py "$set"
    done
}

throttle() { grep -E 'nr_throttled|throttled_usec' /sys/fs/cgroup/cpu.stat | tr '\n' ' '; }

run() {
    local spec
    for spec in "$@"; do
        echo "=== $(date -u +%T) node=${NODE_NAME:-?} $spec | before: $(throttle)"
        read -r -a args <<<"$spec"
        # One failing run must not hide the others' results.
        if [[ "${args[0]}" == c2 ]]; then
            "$PY" /bench/run.py "${args[@]:1}" --threads 2 --track 0 &
            "$PY" /bench/run.py "${args[@]:1}" --threads 2 --track 1 &
            wait -n || echo "FAIL $spec"
            wait -n || echo "FAIL $spec"
        else
            "$PY" /bench/run.py "${args[@]}" || echo "FAIL $spec"
        fi
        echo "=== $(date -u +%T) end | after: $(throttle)"
    done
}

case "${1:-}" in
    setup) setup ;;
    run) shift; run "$@" ;;
    score) "$PY" /bench/score.py --json ;;
    *) sed -n 3,7p "$0" >&2; exit 2 ;;
esac
