FROM debian:trixie-slim AS builder
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
# Security: the installer refuses root; install as a throwaway user, keep only the binary.
RUN useradd -m -d /build builder
USER builder
WORKDIR /build
RUN curl -fsSL https://claude.ai/install.sh | bash \
    && cp -L /build/.local/bin/claude /build/claude-bin

FROM debian:trixie-slim

ARG BUILD_TIMESTAMP="1970-01-01T00:00:00+00:00"
ARG COMMIT_HASH="00000000-dirty"
ARG PROJECT_URL="grill-room/spikes/s3"
ARG VERSION="v0.0.0"

LABEL org.opencontainers.image.source=${PROJECT_URL}
LABEL org.opencontainers.image.created=${BUILD_TIMESTAMP}
LABEL org.opencontainers.image.version=${VERSION}
LABEL org.opencontainers.image.revision=${COMMIT_HASH}

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /build/claude-bin /usr/local/bin/claude
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

RUN useradd -m -u 1000 -d /home/agent -s /usr/sbin/nologin agent \
    && mkdir /workspace && chown agent:agent /workspace
ENV HOME=/home/agent \
    DISABLE_AUTOUPDATER=1
USER agent
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["claude", "--version"]
