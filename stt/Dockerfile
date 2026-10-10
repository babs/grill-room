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
# The model ships in the image: a default-deny egress policy leaves no way to fetch it at start,
# and a pinned revision keeps every pod on the same weights.
ARG WHISPER_MODEL="small"
ARG WHISPER_REVISION="536b0662742c02347bc0e980a01041f333bce120"
RUN .venv/bin/python -c "import sys; from faster_whisper import download_model; \
    download_model(sys.argv[1], output_dir='/models/whisper', revision=sys.argv[2])" \
    "${WHISPER_MODEL}" "${WHISPER_REVISION}" && \
    rm -rf /root/.cache

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

# Security: non-root, and the code, venv and model stay root-owned so the worker cannot alter them.
# The worker writes nothing: no chown, which would also copy the 0.5 GB venv into another layer.
RUN useradd -r -s /usr/sbin/nologin -d /app app
WORKDIR /app
COPY --from=builder /models /models
COPY --from=builder /app/.venv /app/.venv
COPY grill_stt ./grill_stt
# `COPY grill_stt` does NOT bring run.sh: without this line the image dies at start.
COPY --chmod=755 run.sh .
USER app
# HF_HUB_OFFLINE: the model is baked in; any download attempt is a bug, so make it fail.
ENV PATH="/app/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HF_HUB_OFFLINE=1 \
    APP_MODEL=/models/whisper
EXPOSE 9100
CMD ["./run.sh"]
