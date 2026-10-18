# pandoc's official static build: no libc or data files needed, so it runs on distroless static.
FROM pandoc/minimal:3.11@sha256:fc17da3ba8f0a70b2ad5d7d4d61fed7d25da2e091cc3139f4d371f03b18641df AS pandoc

# Empty unless `--build-context daemon-dist=<dir>` ships a prebuilt, signed daemon release.
FROM scratch AS daemon-dist

# Empty unless `--build-context daemon-prev=<dir>` ships the previous release, with its PROTO, for
# supervisors too old for this one's workers (docs/daemon-protocol.md).
FROM scratch AS daemon-prev

FROM golang:1.26-bookworm AS builder
WORKDIR /src
# go.sum* — glob, not a typo: a stdlib-only module has no go.sum and the build would fail hard.
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# The office fixtures must convert with the exact pandoc the image ships (pptx needs 3.8+).
COPY --from=pandoc /usr/local/bin/pandoc /usr/local/bin/pandoc
RUN GRILL_ROOM_TEST_PANDOC_REQUIRED=1 go test -run 'TestPandoc' ./internal/convert/
ARG VERSION="v0.0.0"
ARG COMMIT_HASH="00000000-dirty"
ARG BUILD_TIMESTAMP="1970-01-01T00:00:00+00:00"
ARG PROJECT_URL="project-name"
ARG PKG="./cmd/grill-room"
# Builder=$(go version) must be evaluated HERE, not passed as a build arg: an arg reports the
# host's toolchain, not this image's.
RUN CGO_ENABLED=0 go build -ldflags="-s -w \
    -X 'main.Version=${VERSION}' \
    -X 'main.CommitHash=${COMMIT_HASH}' \
    -X 'main.BuildTimestamp=${BUILD_TIMESTAMP}' \
    -X 'main.Builder=$(go version)' \
    -X 'main.ProjectURL=${PROJECT_URL}'" \
    -o /app "${PKG}"
# The daemon release this server hands out (D28): the prebuilt one with its bundles when given, of
# this image's version only, else every target cross-compiled here, unsigned.
RUN --mount=type=bind,from=daemon-dist,target=/daemon-dist \
    if [ -f /daemon-dist/VERSION ]; then \
      [ "$(cat /daemon-dist/VERSION)" = "${VERSION}" ] \
        || { echo "daemon-dist holds $(cat /daemon-dist/VERSION), the image is ${VERSION}" >&2; exit 1; }; \
      mkdir /daemon && cp /daemon-dist/VERSION /daemon-dist/grill-room-daemon-* /daemon/ && chmod -R a+rX /daemon; \
      if [ -f /daemon-dist/PROTO ]; then cp /daemon-dist/PROTO /daemon/; fi; \
    else \
      make release DIST=/daemon VERSION="${VERSION}" COMMIT_HASH="${COMMIT_HASH}" BUILD_TS="${BUILD_TIMESTAMP}" PROJECT_URL="${PROJECT_URL}"; \
    fi
RUN --mount=type=bind,from=daemon-prev,target=/daemon-prev-in \
    mkdir /daemon-prev && if [ -f /daemon-prev-in/VERSION ]; then \
      cp /daemon-prev-in/VERSION /daemon-prev-in/PROTO /daemon-prev-in/grill-room-daemon-* /daemon-prev/ && chmod -R a+rX /daemon-prev; \
    fi

FROM gcr.io/distroless/static-debian13:nonroot

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

ENV GRILL_ROOM_ADDR="0.0.0.0:7680" \
    GRILL_ROOM_METRICS_ADDR="0.0.0.0:7681" \
    GRILL_ROOM_PANDOC="/usr/local/bin/pandoc" \
    GRILL_ROOM_DAEMON_DIR="/daemon" \
    GRILL_ROOM_DAEMON_PREV_DIR="/daemon-prev"

COPY --from=pandoc /usr/local/bin/pandoc /usr/local/bin/pandoc
COPY --from=builder /daemon /daemon
COPY --from=builder /daemon-prev /daemon-prev
COPY --from=builder /app /app
EXPOSE 7680 7681
ENTRYPOINT ["/app"]
