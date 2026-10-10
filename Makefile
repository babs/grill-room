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

.PHONY: build release run test test-docker e2e e2e-voice e2e-agent-live e2e-install stt-test stt-image cover lint docker-build sandbox-image sandbox-fake-image sandbox-push fixtures dev-up dev-down traefik-install traefik-uninstall clean

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) $(PKG)
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME)-daemon ./cmd/grill-room-daemon

# The daemon release the room server hands out (D28): one static binary per target, its sha256 file
# (sha256sum format, checked by install.sh) and VERSION. Windows runs the linux build inside WSL.
DIST           ?= dist
DAEMON_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
# The room server refuses to start on a VERSION that is not semver: fail the build instead.
release:
	@echo '$(VERSION)' | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$$' \
	  || { echo "VERSION '$(VERSION)' is not a semver like v1.2.3" >&2; exit 1; }
	rm -rf '$(DIST)' && mkdir -p '$(DIST)'
	for t in $(DAEMON_TARGETS); do \
	  f=$(APP_NAME)-daemon-$${t%/*}-$${t#*/}; \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build -trimpath -ldflags="$(LDFLAGS)" -o '$(DIST)'/$$f ./cmd/grill-room-daemon || exit 1; \
	  (cd '$(DIST)' && { sha256sum $$f 2>/dev/null || shasum -a 256 $$f; } > $$f.sha256) || exit 1; \
	done
	echo '$(VERSION)' > '$(DIST)/VERSION'

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
	$(MAKE) release DIST=bin/e2e-release VERSION=v0.0.1
	cd e2e && pnpm install --frozen-lockfile && pnpm test

# Voice tests against the compose stack behind the local traefik: run `make dev-up` first.
e2e-voice:
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c voice/playwright.config.js

# Agent turns with two real daemons against the compose stack: run `make dev-up sandbox-image` first.
# Spends about six real Claude requests (haiku) with this machine's Claude Code login.
e2e-agent-live: build
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c agent-live/playwright.config.js

# The one-liner on a clean Debian container, pairing, self-update and tampered releases against the
# compose stack: run `make dev-up` first. Copies releases into the grill-room container and
# recreates it at the end. No Claude request.
e2e-install:
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c install/playwright.config.js

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
	git ls-files -co --exclude-standard '*.sh' | xargs shellcheck

docker-build:
	docker build -t $(APP_NAME):local \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  --build-arg PKG=$(PKG) .

sandbox-fake-image: sandbox-image
	docker build -t $(SANDBOX_FAKE_IMAGE) -f internal/sandbox/Dockerfile.fake \
	  --build-arg BASE=$(SANDBOX_IMAGE) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  .

# Built with no context, as the daemon builds its embedded copy when no registry image is advertised.
sandbox-image:
	docker build -t $(SANDBOX_IMAGE) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  - < internal/sandbox/Dockerfile

# Pushes the sandbox image as $(REGISTRY)/grill-room/sandbox:$(VERSION) and prints the digest-pinned
# reference for the room server's GRILL_ROOM_SANDBOX_IMAGE. REGISTRY comes from .env.
sandbox-push: sandbox-image
	@test -n "$(REGISTRY)" || { echo "REGISTRY is not set, see .env.example" >&2; exit 1; }
	docker tag $(SANDBOX_IMAGE) $(REGISTRY)/grill-room/sandbox:$(VERSION)
	docker push $(REGISTRY)/grill-room/sandbox:$(VERSION)
	@docker image inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' $(REGISTRY)/grill-room/sandbox:$(VERSION) \
	  | grep '^$(REGISTRY)/grill-room/sandbox@' | sed 's/^/GRILL_ROOM_SANDBOX_IMAGE=/'

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
	rm -rf bin/ '$(DIST)' coverage.txt
