# Sandbox for the agent turn: Claude Code CLI, no credential, no route out but the daemon's proxy.
# The daemon writes the dummy credentials and the .claude.json stub before every turn.
FROM debian:trixie-slim AS builder
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
# Security: the installer refuses root; install as a throwaway user, keep only the binary.
RUN useradd -m -d /build builder
USER builder
WORKDIR /build
# Pinned: the stub keys and the proxy's endpoint list were measured on this release (spikes S2, S3).
ARG CLAUDE_VERSION="2.1.296"
RUN curl -fsSL https://claude.ai/install.sh | bash -s "${CLAUDE_VERSION}" \
    && cp -L /build/.local/bin/claude /build/claude-bin

FROM debian:trixie-slim

ARG BUILD_TIMESTAMP="1970-01-01T00:00:00+00:00"
ARG COMMIT_HASH="00000000-dirty"
ARG PROJECT_URL="grill-room/sandbox"
ARG VERSION="v0.0.0"

LABEL org.opencontainers.image.source=${PROJECT_URL}
LABEL org.opencontainers.image.created=${BUILD_TIMESTAMP}
LABEL org.opencontainers.image.version=${VERSION}
LABEL org.opencontainers.image.revision=${COMMIT_HASH}

ENV VERSION="${VERSION}" \
    COMMIT_HASH="${COMMIT_HASH}" \
    BUILD_TIMESTAMP="${BUILD_TIMESTAMP}" \
    PROJECT_URL="${PROJECT_URL}"

# socat runs the relay container: the sandbox's internal network reaches the host proxy through it.
# procps: pkill stops a cancelled turn, which a killed `docker exec` client leaves running.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git procps socat \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /build/claude-bin /usr/local/bin/claude

RUN useradd -m -u 1000 -d /home/agent -s /usr/sbin/nologin agent \
    && mkdir /workspace && chown agent:agent /workspace
ENV HOME=/home/agent \
    DISABLE_AUTOUPDATER=1
USER agent
WORKDIR /workspace
CMD ["sleep", "infinity"]
