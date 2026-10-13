#!/usr/bin/env bash
# Runs inside the lab Job: builds the venvs on the cache volume, renders the audio once, then runs
# each spec "backend:model:glossary" in its own process so the RAM peak is per model.
set -euo pipefail

readonly BENCH=/bench CACHE=/cache
export UV_CACHE_DIR="$CACHE/uv" HF_HOME="$CACHE/hf" UV_LINK_MODE=copy

venv() {
    local name="$1"
    shift
    local dir="$CACHE/venv-$name"
    [[ -x "$dir/bin/python" ]] || uv venv -q "$dir"
    case "$name" in
        whisper) uv pip install -q --python "$dir" "faster-whisper>=1.2,<1.3" "av>=16,<17" "piper-tts>=1.8" ;;
        onnx) uv pip install -q --python "$dir" "onnx-asr[cpu,hub]>=0.12" ;;
        torch)
            # The PyPI torch wheel pulls the CUDA stack.
            uv pip install -q --python "$dir" torch torchaudio --index-url https://download.pytorch.org/whl/cpu
            uv pip install -q --python "$dir" "transformers>=4.53" sentencepiece numpy
            ;;
        *) echo "unknown venv $name" >&2; return 1 ;;
    esac
    echo "$dir/bin/python"
}

if [[ ! -f "$CACHE/audio.npz" ]]; then
    "$(venv whisper)" "$BENCH/bench.py" audio "$CACHE"
fi

for spec in "$@"; do
    IFS=: read -r backend model glossary <<<"$spec"
    echo "SPEC $spec threads=$THREADS every=${EVERY:-1}"
    group="$backend"
    [[ "$backend" == kyutai || "$backend" == granite ]] && group=torch
    # One failing model must not hide the others' results.
    if ! "$(venv "$group")" "$BENCH/bench.py" run "$CACHE" "$backend" "$model" "$glossary" \
        --threads "$THREADS" --every "${EVERY:-1}"; then
        echo "FAIL $spec"
    fi
done
