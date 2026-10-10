FROM golang:1.26-bookworm AS builder
WORKDIR /src
# go.sum* — glob, not a typo: a stdlib-only module has no go.sum and the build would fail hard.
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
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
# distroless has no shell to create the data dir in the runtime stage.
RUN mkdir /data

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
    GRILL_ROOM_DB="/data/grill-room.db"

COPY --from=builder --chown=65532:65532 /data /data
COPY --from=builder /app /app
VOLUME /data
EXPOSE 7680 7681
ENTRYPOINT ["/app"]
