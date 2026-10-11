# Sandbox for the agent turn: Claude Code CLI and the tools skills use, no credential, no route out
# but the daemon's proxy and the egress allowlist proxy (squid, run from this same image).
# The daemon writes the dummy credentials and the .claude.json stub before every turn.
# Built with no context (the daemon embeds this file): everything comes from pinned images or
# downloads checked against a sha256 here.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS builder
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
    && rm -rf /var/lib/apt/lists/*
# Security: the installer refuses root; install as a throwaway user, keep only the binary.
RUN useradd -m -d /build builder
USER builder
WORKDIR /build
# Pinned: the stub keys and the proxy's endpoint list were measured on this release (spikes S2, S3).
ARG CLAUDE_VERSION="2.1.296"
RUN curl -fsSL https://claude.ai/install.sh | bash -s "${CLAUDE_VERSION}" \
    && cp -L /build/.local/bin/claude /build/claude-bin

# The checksums belong to these versions: bumping one without its sums fails the build.
ARG NODE_VERSION="24.21.0"
ARG YQ_VERSION="4.54.1"
RUN case "$(dpkg --print-architecture)" in \
      amd64) node_arch=x64 \
             node_sha=fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6 \
             yq_arch=amd64 \
             yq_sha=8e34fc298390875de416e6a4afcb8cabeceb25d9aa8506c1a2f9353cf702ea5f ;; \
      arm64) node_arch=arm64 \
             node_sha=6ad1325edbdb5649c379b75a237147a666c95d4f9ae8d340fef2d1575d289ad2 \
             yq_arch=arm64 \
             yq_sha=189088da0c6429ec5178dfaab1a114805f6cab0b61b165ab236efedf1d57a71b ;; \
      *) echo "unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && curl -fsSLo node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node_arch}.tar.xz" \
    && echo "${node_sha}  node.tar.xz" | sha256sum -c - \
    && mkdir node \
    && tar -xJf node.tar.xz -C node --strip-components=1 \
         --exclude="node-v${NODE_VERSION}-linux-${node_arch}/include" \
         --exclude="node-v${NODE_VERSION}-linux-${node_arch}/share" \
         --exclude="node-v${NODE_VERSION}-linux-${node_arch}/*.md" \
         --exclude="node-v${NODE_VERSION}-linux-${node_arch}/LICENSE" \
    && curl -fsSLo yq "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${yq_arch}" \
    && echo "${yq_sha}  yq" | sha256sum -c - \
    && chmod 755 yq

FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f

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
# squid runs the egress container: the only way out, to the host's allowlist.
# procps: pkill stops a cancelled turn, which a killed `docker exec` client leaves running.
# The Python libraries come from Debian, signed by apt, so the agent's venv only layers on them.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
         ca-certificates curl git jq procps ripgrep socat squid \
         python3 python3-venv \
         python3-docx python3-matplotlib python3-openpyxl python3-pandas python3-pypdf \
         python3-lxml python3-pil python3-typing-extensions python3-xlsxwriter \
         python3-munkres \
    && rm -rf /var/lib/apt/lists/*
# Same pandoc as the room server's conversions.
COPY --from=pandoc/minimal:3.11@sha256:fc17da3ba8f0a70b2ad5d7d4d61fed7d25da2e091cc3139f4d371f03b18641df /usr/local/bin/pandoc /usr/local/bin/pandoc
COPY --from=ghcr.io/astral-sh/uv:0.13.0@sha256:cdc6093146eb3ff6a40107b38f008b789e050e77ad87865e381d9917da55a168 /uv /usr/local/bin/uv
COPY --from=builder /build/node/ /usr/local/
COPY --from=builder /build/yq /usr/local/bin/yq
COPY --from=builder /build/claude-bin /usr/local/bin/claude
# python-pptx is not in Debian; its dependencies are, installed above.
RUN printf 'python-pptx==1.0.2 --hash=sha256:160838e0b8565a8b1f67947675886e9fea18aa5e795db7ae531606d68e785cba\n' > /tmp/req.txt \
    && UV_NO_CACHE=1 uv pip install --system --break-system-packages --no-deps --require-hashes -r /tmp/req.txt \
    && rm /tmp/req.txt

RUN useradd -m -u 1000 -d /home/agent -s /usr/sbin/nologin agent \
    && mkdir /workspace && chown agent:agent /workspace
USER agent
# The root filesystem is read-only at run time: the venv lives in the home volume, which Docker
# fills from this image, so `uv pip install` and `pip install` can write to it.
RUN python3 -m venv --system-site-packages /home/agent/.venv
ENV HOME=/home/agent \
    DISABLE_AUTOUPDATER=1 \
    VIRTUAL_ENV=/home/agent/.venv \
    NPM_CONFIG_PREFIX=/home/agent/.npm-global \
    PATH=/home/agent/.venv/bin:/home/agent/.npm-global/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
WORKDIR /workspace
CMD ["sleep", "infinity"]
