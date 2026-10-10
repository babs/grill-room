APP_NAME    := grill-room
PKG         := ./cmd/grill-room
VERSION     ?= v0.0.0
COMMIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null || echo "0000000")
BUILD_TS    := $(shell date -Iseconds)
BUILDER     := $(shell go version 2>/dev/null || echo "unknown")
PROJECT_URL ?= https://github.com/babs/grill-room
LDFLAGS     := -s -w \
  -X 'main.Version=$(VERSION)' \
  -X 'main.CommitHash=$(COMMIT_HASH)' \
  -X 'main.BuildTimestamp=$(BUILD_TS)' \
  -X 'main.Builder=$(BUILDER)' \
  -X 'main.ProjectURL=$(PROJECT_URL)'

# Local dev settings (DEV_DOMAIN, TRAEFIK_RULES_DIR, ports); see .env.example.
-include .env
GRILL_ROOM_PORT ?= 7680
# host.docker.internal is the docker bridge gateway: publishing there keeps the app off the LAN.
GRILL_ROOM_BIND ?= $(shell docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null)
export GRILL_ROOM_BIND
TRAEFIK_TEMPLATE := deploy/local/traefik-grill-room.yaml
TRAEFIK_RULE      = $(TRAEFIK_RULES_DIR)/grill-room.yaml

SANDBOX_IMAGE ?= grill-room-sandbox:local
# The sandbox with a fake claude: agent turns end to end with no subscription use.
SANDBOX_FAKE_IMAGE ?= grill-room-sandbox:fake

.PHONY: build build-daemons run test test-docker e2e e2e-voice e2e-agent-live stt-test stt-image cover lint docker-build sandbox-image sandbox-fake-image fixtures dev-up dev-down traefik-install traefik-uninstall clean

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) $(PKG)
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME)-daemon ./cmd/grill-room-daemon

# The daemon for every supported target: Windows runs the linux build inside WSL.
DAEMON_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
build-daemons:
	for t in $(DAEMON_TARGETS); do \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME)-daemon-$${t%/*}-$${t#*/} ./cmd/grill-room-daemon || exit 1; \
	done

run: build
	./bin/$(APP_NAME)

test:
	go test -race -cover ./...

# Sandbox and agent turn tests against real Docker, a fake Anthropic upstream and a fake claude:
# no subscription use.
test-docker: sandbox-image sandbox-fake-image
	GRILL_ROOM_TEST_DOCKER=1 GRILL_ROOM_SANDBOX_IMAGE=$(SANDBOX_IMAGE) GRILL_ROOM_SANDBOX_FAKE_IMAGE=$(SANDBOX_FAKE_IMAGE) \
	  go test -race -run 'Docker' ./internal/sandbox/ ./internal/daemon/

# Browser tests against a fresh server, in the installed Chrome; agent turns run in Docker with the
# fake claude.
e2e: build sandbox-fake-image
	cd e2e && pnpm install --frozen-lockfile && pnpm test

# Voice tests against the compose stack behind the local traefik: run `make dev-up` first.
e2e-voice:
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c voice/playwright.config.js

# Agent turns with two real daemons against the compose stack: run `make dev-up sandbox-image` first.
# Spends about six real Claude requests (haiku) with this machine's Claude Code login.
e2e-agent-live: build
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c agent-live/playwright.config.js

# STT worker: lint, types and unit tests. The e2e-marked test runs too once the model is in the
# Hugging Face cache, and skips otherwise.
stt-test:
	cd stt && uv run ruff check . && uv run ruff format --check . && uv run mypy grill_stt tests scripts && uv run pytest

stt-image:
	docker build -t grill-stt:local \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  stt/

cover:
	go test -race -coverprofile=coverage.txt ./... && go tool cover -func=coverage.txt | tail -1

lint:
	golangci-lint run ./...

docker-build:
	docker build -t $(APP_NAME):local \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  --build-arg PKG=$(PKG) .

sandbox-fake-image: sandbox-image
	docker build -t $(SANDBOX_FAKE_IMAGE) -f sandbox/Dockerfile.fake \
	  --build-arg BASE=$(SANDBOX_IMAGE) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  .

sandbox-image:
	docker build -t $(SANDBOX_IMAGE) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  sandbox/

# Office fixtures for the pandoc tests, written by the pandoc release the image ships.
FIXTURES := internal/convert/testdata
fixtures:
	for f in docx odt pptx; do \
	  docker run --rm -u "$$(id -u):$$(id -g)" -v "$(CURDIR)/$(FIXTURES):/data" pandoc/minimal:3.11 fixture.md -o fixture.$$f || exit 1; \
	done

dev-up: traefik-install
	docker compose up -d --build

dev-down: traefik-uninstall
	docker compose down

traefik-install:
	@test -n "$(DEV_DOMAIN)" || { echo "DEV_DOMAIN is not set, see .env.example" >&2; exit 1; }
	@test -d "$(TRAEFIK_RULES_DIR)" || { echo "TRAEFIK_RULES_DIR '$(TRAEFIK_RULES_DIR)' is not a directory, see .env.example" >&2; exit 1; }
	DEV_DOMAIN='$(DEV_DOMAIN)' GRILL_ROOM_PORT='$(GRILL_ROOM_PORT)' \
	  envsubst '$${DEV_DOMAIN} $${GRILL_ROOM_PORT}' < $(TRAEFIK_TEMPLATE) > '$(TRAEFIK_RULE)'
	@echo "installed $(TRAEFIK_RULE): https://grill-room.$(DEV_DOMAIN)/"

traefik-uninstall:
	@test -n "$(TRAEFIK_RULES_DIR)" || { echo "TRAEFIK_RULES_DIR is not set, see .env.example" >&2; exit 1; }
	rm -f '$(TRAEFIK_RULE)'

clean:
	rm -rf bin/ coverage.txt
