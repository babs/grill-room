# Model pins, shared by the stages that fetch them and the one that reports them.
ARG WHISPER_REVISION="536b0662742c02347bc0e980a01041f333bce120"
ARG PARAKEET_REVISION="39eeb55181f0d354fd934f06e92fd8d5037fed8e"

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
# The models ship in the image: a default-deny egress policy leaves no way to fetch them at start,
# and a pinned revision keeps every pod on the same weights.
ARG WHISPER_MODEL="small"
ARG WHISPER_REVISION
RUN .venv/bin/python -c "import sys; from faster_whisper import download_model; \
    download_model(sys.argv[1], output_dir='/models/whisper-small', revision=sys.argv[2])" \
    "${WHISPER_MODEL}" "${WHISPER_REVISION}" && \
    rm -rf /root/.cache

# transcribe.cpp v0.4.0, the build spike S10d validated, as a shared library for the ctypes binding.
FROM debian:trixie-slim AS tcpp
RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential cmake git ca-certificates && \
    rm -rf /var/lib/apt/lists/*
ARG TCPP_COMMIT="f1dcd15858c4bbba87ca97862b4e448b2cf15b8f"
# Compile jobs: CI passes its core count; the default spares a shared workstation.
ARG BUILD_JOBS="2"
WORKDIR /src
RUN git init -q . && \
    git fetch -q --depth 1 https://github.com/handy-computer/transcribe.cpp.git "${TCPP_COMMIT}" && \
    git checkout -q FETCH_HEAD
# The lab nodes are AVX2 without AVX-512 or AVX-VNNI: never GGML_NATIVE, or an image built on a newer
# CPU dies with SIGILL there. No system BLAS, as in S10d: the decoder keeps its scalar loop.
# $ORIGIN: libtranscribe finds its ggml libraries beside it wherever they are copied.
RUN cmake -S . -B build -DCMAKE_BUILD_TYPE=Release \
      -DTRANSCRIBE_BUILD_SHARED=ON -DTRANSCRIBE_BUILD_TESTS=OFF -DTRANSCRIBE_BUILD_EXAMPLES=OFF \
      -DTRANSCRIBE_INSTALL=OFF -DTRANSCRIBE_USE_SYSTEM_BLAS=OFF \
      -DGGML_NATIVE=OFF -DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON -DGGML_BMI2=ON \
      -DGGML_AVX_VNNI=OFF -DGGML_AVX512=OFF \
      -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON '-DCMAKE_INSTALL_RPATH=$ORIGIN' && \
    cmake --build build -j "${BUILD_JOBS}" --target transcribe && \
    mkdir -p /out/lib /out/licenses/transcribe.cpp && \
    find build -name '*.so*' -exec cp -a {} /out/lib/ \; && \
    cp LICENSE THIRD-PARTY-LICENSES.md /out/licenses/transcribe.cpp/ && \
    cp ggml/LICENSE /out/licenses/transcribe.cpp/LICENSE.ggml && \
    cp src/third_party/miniz/LICENSE /out/licenses/transcribe.cpp/LICENSE.miniz && \
    cp src/third_party/silero_vad/LICENSE /out/licenses/transcribe.cpp/LICENSE.silero_vad && \
    test -f /out/lib/libtranscribe.so

# The S10d GGUF, by revision; BuildKit refuses a file whose sha256 differs (spec 007, AC12).
FROM scratch AS model
ARG PARAKEET_REPO="handy-computer/parakeet-ultra-gguf"
ARG PARAKEET_REVISION
ARG PARAKEET_FILE="parakeet-ultra-Q8_0.gguf"
ARG PARAKEET_SHA256="007a59761e9258779f189df396b50d12b83bb5b79e66df4b955c230d2f2a0a59"
# 0644: ADD from a URL writes 0600 for root, which the app user cannot read.
ADD --checksum=sha256:${PARAKEET_SHA256} --chmod=644 \
    https://huggingface.co/${PARAKEET_REPO}/resolve/${PARAKEET_REVISION}/${PARAKEET_FILE} /parakeet/${PARAKEET_FILE}

FROM python:3.14-slim-trixie

ARG BUILD_TIMESTAMP="1970-01-01T00:00:00+00:00"
ARG COMMIT_HASH="00000000-dirty"
ARG PROJECT_URL="project-name"
ARG VERSION="v0.0.0"
ARG WHISPER_REVISION
ARG PARAKEET_REVISION

LABEL org.opencontainers.image.source=${PROJECT_URL}
LABEL org.opencontainers.image.created=${BUILD_TIMESTAMP}
LABEL org.opencontainers.image.version=${VERSION}
LABEL org.opencontainers.image.revision=${COMMIT_HASH}

ENV VERSION="${VERSION}" \
    COMMIT_HASH="${COMMIT_HASH}" \
    BUILD_TIMESTAMP="${BUILD_TIMESTAMP}" \
    PROJECT_URL="${PROJECT_URL}"

# Security: non-root, and the code, venv and models stay root-owned so the worker cannot alter them.
# The worker writes nothing: no chown, which would also copy the 0.5 GB venv into another layer.
RUN useradd -r -s /usr/sbin/nologin -d /app app
WORKDIR /app
COPY --from=builder /models /models
COPY --from=model /parakeet /models/parakeet
COPY --from=tcpp /out/lib /opt/transcribe/lib
COPY --from=tcpp /out/licenses /licenses
COPY licenses /licenses
COPY --from=builder /app/.venv /app/.venv
COPY grill_stt ./grill_stt
# `COPY grill_stt` does NOT bring run.sh: without this line the image dies at start.
COPY --chmod=755 run.sh .
USER app
# HF_HUB_OFFLINE: the models are baked in; any download attempt is a bug, so make it fail.
ENV PATH="/app/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HF_HUB_OFFLINE=1 \
    TRANSCRIBE_LIBRARY=/opt/transcribe/lib/libtranscribe.so \
    APP_PARAKEET_MODEL=/models/parakeet/parakeet-ultra-Q8_0.gguf \
    APP_PARAKEET_REVISION=${PARAKEET_REVISION} \
    APP_MODEL=/models/whisper-small \
    APP_WHISPER_REVISION=${WHISPER_REVISION}
EXPOSE 9100
CMD ["./run.sh"]
