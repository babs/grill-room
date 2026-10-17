FROM python:3.14-slim-trixie AS builder
# pipefail: fail the build if any stage of a piped RUN fails (e.g. bootstrap below),
# instead of silently taking the exit code of the last command in the pipe.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
COPY --from=ghcr.io/astral-sh/uv:0.12.0 /uv /usr/local/bin/uv
WORKDIR /app
COPY pyproject.toml uv.lock ./
# `-a requirements | uv pip install`: a uv venv has no pip. Call the .venv binary, not `uv run`,
# which would re-sync the dev group into this --no-dev venv.
RUN uv sync --locked --no-dev && \
    .venv/bin/opentelemetry-bootstrap -a requirements | uv pip install --requirement -
# Every Python package's own licence files, for /licenses (013-AC12).
RUN for d in .venv/lib/python3.14/site-packages/*.dist-info; do \
      n=$(basename "$d" .dist-info); \
      files=$(find "$d" \( -iname 'LICEN[CS]E*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) -type f); \
      [[ -z $files ]] && continue; \
      mkdir -p "/licenses/python/$n" && cp $files "/licenses/python/$n/"; \
    done

# The weights and voices, fetched by revision and sha256, then what the pod would compute at
# every start (013-AC10). The gated weights need a Hugging Face read token: a BuildKit secret,
# mounted for this RUN only, so no layer, history entry or environment variable holds it.
FROM builder AS models
COPY grill_tts ./grill_tts
ENV HF_HOME=/models/hf HF_HUB_DISABLE_PROGRESS_BARS=1 HF_HUB_DISABLE_TELEMETRY=1
RUN --mount=type=secret,id=hf,required=true \
    .venv/bin/python -m grill_tts.fetch /run/secrets/hf
# Offline from here: anything fetch.py did not pin fails the build instead of downloading.
ARG BUILD_THREADS="2"
RUN HF_HUB_OFFLINE=1 OMP_NUM_THREADS="${BUILD_THREADS}" .venv/bin/python -m grill_tts.prepare /models && \
    .venv/bin/python -c "import json, sys; vs = json.load(open('grill_tts/voices.json')); \
      print('\n'.join(f\"{v['name']} ({v['id']}): {v['credit']}. Licence {v['licence']}. Source https://huggingface.co/{v['source'].replace('@', '/blob/')}\" for v in vs))" \
      > /licenses-voices.txt && \
    # safetensors writes 0600: the app user must read what root prepared.
    chmod -R a+rX /models

FROM python:3.14-slim-trixie

ARG BUILD_TIMESTAMP="1970-01-01T00:00:00+00:00"
ARG COMMIT_HASH="00000000-dirty"
ARG PROJECT_URL="project-name"
ARG VERSION="v0.0.0"

LABEL org.opencontainers.image.source=${PROJECT_URL}
LABEL org.opencontainers.image.created=${BUILD_TIMESTAMP}
LABEL org.opencontainers.image.version=${VERSION}
LABEL org.opencontainers.image.revision=${COMMIT_HASH}

ENV VERSION="${VERSION}" \
    COMMIT_HASH="${COMMIT_HASH}" \
    BUILD_TIMESTAMP="${BUILD_TIMESTAMP}" \
    PROJECT_URL="${PROJECT_URL}"

# Security: non-root; code, venv and models stay root-owned so the pod cannot alter them.
RUN useradd -r -s /usr/sbin/nologin -d /app app
WORKDIR /app
COPY --from=builder /app/.venv /app/.venv
COPY --from=builder /licenses /licenses
COPY --from=models /models /models
COPY --from=models /licenses-voices.txt /licenses/voices.txt
COPY licenses /licenses
COPY grill_tts/models.lock.json /licenses/models.lock.json
COPY grill_tts ./grill_tts
# `COPY grill_tts` does NOT bring run.sh: without this line the image dies at start.
COPY --chmod=755 run.sh .
USER app
# HF_HUB_OFFLINE: everything is baked in; a download attempt is a bug, so make it fail.
ENV PATH="/app/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HF_HOME=/models/hf \
    HF_HUB_OFFLINE=1 \
    HF_HUB_DISABLE_TELEMETRY=1 \
    APP_MODELS_DIR=/models
EXPOSE 9100
CMD ["./run.sh"]
