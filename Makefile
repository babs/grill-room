# A dependency needing a newer Go must fail the build, not fetch a toolchain (spike S7).
export GOTOOLCHAIN := local

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

.PHONY: ui-tools ui-vendor ui-check build e2e-build release dev-release run test test-docker test-offline test-release test-live-accounts e2e e2e-repeat e2e-voice e2e-device-voice e2e-persona-voice e2e-agent-live e2e-mcp-live e2e-install e2e-lab e2e-lab-mail e2e-push e2e-supervisor e2e-crossinstance scenarios-test stt-test stt-image stt-image-test stt-image-e2e tts-test tts-image cover lint docker-build sandbox-image sandbox-fake-image sandbox-push k8s-push k8s-render k8s-check k8s-apply k8s-delete k8s-monitoring k8s-monitoring-delete fixtures icons dev-up dev-down traefik-install traefik-uninstall clean

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) $(PKG)
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME)-daemon ./cmd/grill-room-daemon

# The browser tests' server: the e2e tag lets GRILL_ROOM_TEST_GIT_ALLOW_LOOPBACK send plugin fetches
# to their local git server (tools/e2egit). Never built into a release or an image.
e2e-build:
	CGO_ENABLED=0 go build -tags e2e -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME)-e2e $(PKG)
	CGO_ENABLED=0 go build -o bin/e2egit ./tools/e2egit

# The daemon release the room server hands out (D28): one static binary per target, its sha256 file
# (sha256sum format, checked by install.sh), VERSION and PROTO, its worker protocol version.
DIST           ?= dist
DAEMON_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
# The room server refuses to start on a VERSION that is not semver: fail the build instead.
release:
	@echo '$(VERSION)' | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$$' \
	  || { echo "VERSION '$(VERSION)' is not a semver like v1.2.3" >&2; exit 1; }
	rm -rf '$(DIST)' && mkdir -p '$(DIST)'
	for t in $(DAEMON_TARGETS); do \
	  f=$(APP_NAME)-daemon-$${t%/*}-$${t#*/}; [ $${t%/*} = windows ] && f=$$f.exe; \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build -trimpath -ldflags="$(LDFLAGS)" -o '$(DIST)'/$$f ./cmd/grill-room-daemon || exit 1; \
	  (cd '$(DIST)' && { sha256sum $$f 2>/dev/null || shasum -a 256 $$f; } > $$f.sha256) || exit 1; \
	done
	echo '$(VERSION)' > '$(DIST)/VERSION'
	sed -n 's/^[[:space:]]*Proto[[:space:]]*=[[:space:]]*\([0-9][0-9]*\)$$/\1/p' internal/daemon/proto.go > '$(DIST)/PROTO'
	grep -Eq '^[0-9]+$$' '$(DIST)/PROTO' || { echo 'no worker protocol version in internal/daemon/proto.go' >&2; exit 1; }

# `make release` signed with a local cosign key pair (docs/release.md): COSIGN_KEY from .env, its
# password from COSIGN_PASSWORD. Offline: no transparency log entry, nothing leaves the machine.
dev-release: export COSIGN_PASSWORD := $(COSIGN_PASSWORD)
dev-release: release
	@test -n "$(COSIGN_KEY)" || { echo "COSIGN_KEY is not set, see .env.example" >&2; exit 1; }
	for f in '$(DIST)'/$(APP_NAME)-daemon-*; do \
	  case $$f in *.sha256) continue ;; esac; \
	  cosign sign-blob --yes --key '$(COSIGN_KEY)' --use-signing-config=false --tlog-upload=false \
	    --bundle "$$f.sigstore.json" "$$f" >/dev/null || exit 1; \
	done

run: build
	./bin/$(APP_NAME)

test:
	go test -race -cover -timeout 20m ./...

# Sandbox and agent turn tests against real Docker, a fake Anthropic upstream and a fake claude:
# no subscription use. The egress test needs Internet access to PyPI, npm and GitHub. -p 1: the
# packages' sandboxes would otherwise compete for Docker and time out the turn waits.
test-docker: sandbox-image sandbox-fake-image
	GRILL_ROOM_TEST_DOCKER=1 GRILL_ROOM_SANDBOX_IMAGE=$(SANDBOX_IMAGE) GRILL_ROOM_SANDBOX_FAKE_IMAGE=$(SANDBOX_FAKE_IMAGE) \
	  go test -race -p 1 -run 'Docker' ./internal/sandbox/ ./internal/daemon/ ./cmd/grill-room-daemon/

# Browser tests against a fresh server, in the installed Chrome (firefoxSpecs in Playwright's
# Firefox too: `pnpm exec playwright install firefox`); agent turns run in Docker with the
# fake claude. Magic links run against a second server and a throwaway mailpit container. The
# server hands out the daemon's own version, so its workers run the daemon's binary: an unsigned
# release at another version would be refused (003-AC2).
# The device and sandbox specs run after the others, each set alone (e2e/playwright.config.js says
# why). All three run even when one fails; the recipe fails if any did.
E2E_SPLIT = { pnpm exec playwright test --project=parallel --project=firefox --pass-with-no-tests $(1); p=$$?; \
	pnpm exec playwright test --project=device --pass-with-no-tests $(1) || p=1; \
	pnpm exec playwright test --project=sandbox --pass-with-no-tests $(1) && test $$p -eq 0; }

e2e: build e2e-build sandbox-fake-image
	$(MAKE) release DIST=bin/e2e-release VERSION=$(VERSION)
	cd e2e && pnpm install --frozen-lockfile && $(E2E_SPLIT) && pnpm exec playwright test -c mail/playwright.config.js

# CI's flake gate (tools/flake-gate.sh): the specs in SPECS, three times each, as `make e2e` runs them.
e2e-repeat: build e2e-build sandbox-fake-image
	$(MAKE) release DIST=bin/e2e-release VERSION=$(VERSION)
	cd e2e && pnpm install --frozen-lockfile && $(call E2E_SPLIT,--repeat-each=3 $(SPECS))

# Voice tests against the compose stack behind the local traefik: run `make dev-up` first.
e2e-voice:
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c voice/playwright.config.js

# 010: the device-to-server voice handover with LiveKit, the STT worker (grill-stt:local, `make
# stt-image`), the real device model and Chrome, on its own loopback stack (e2e/device-voice/run.sh).
# Already fetched model files are only checked.
DEVICE_STT_MODEL_DIR ?= $(CURDIR)/.device-stt-models
e2e-device-voice: build e2e-build
	bin/$(APP_NAME) models fetch --dir '$(DEVICE_STT_MODEL_DIR)'
	cd e2e && pnpm install --frozen-lockfile && GRILL_ROOM_E2E_MODEL_DIR='$(DEVICE_STT_MODEL_DIR)' device-voice/run.sh $(DEVICE_VOICE_ARGS)

# 013: a persona speaks in the call through LiveKit, with the STT worker (grill-stt:local) and the tts
# pod on its fake engine, on its own loopback stack (e2e/persona-voice/run.sh). With
# PERSONA_VOICE_TTS_IMAGE=grill-tts:local the pod runs the real engine instead.
e2e-persona-voice: build e2e-build
	cd e2e && pnpm install --frozen-lockfile && persona-voice/run.sh $(PERSONA_VOICE_ARGS)

# Agent turns with real daemons against the compose stack: run `make dev-up sandbox-image` first.
# Spends four to eight real Claude requests (haiku) with this machine's Claude Code login, plus at
# most four for the skill turn (skills-live.spec.js, three when last run).
e2e-agent-live: build
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c agent-live/playwright.config.js

# 012: a real Claude Code adds the server from the /profile token sheet and sets up a room through
# /mcp, against a server of its own on loopback. Spends at most six real Claude requests (haiku)
# with this machine's Claude Code login; claude runs on a throwaway config dir.
e2e-mcp-live: build
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c mcp-live/playwright.config.js

# The one-liner on a clean Debian container, pairing, self-update and tampered releases against the
# compose stack: run `make dev-up` first. Copies releases into the grill-room container and
# recreates it at the end. No Claude request.
e2e-install:
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c install/playwright.config.js

# A grill session against the lab deployment (`make k8s-apply`), audio once forced through TURN/TLS
# and once with default ICE. Its agent turn runs a real daemon here with haiku: at most six real
# Claude requests, and the sandbox image the room server advertises.
e2e-lab: build
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c lab/playwright.config.js

# 003-AC31: one supervisor serves a room on the compose stack and one on the lab deployment at once,
# on two accounts: this machine's Claude Code login and the setup token in SETUP_TOKEN_FILE. The
# compose stack must serve a release signed with the dev key DEV_KEY_PUB (docs/release.md, Dev
# signing). SUPERVISOR_BIN picks the supervisor binary, default bin/grill-room-daemon. Two haiku
# turns run at once: at most six real Claude requests.
SUPERVISOR_BIN ?= bin/grill-room-daemon
e2e-supervisor: build
	@test -n "$(SETUP_TOKEN_FILE)" && test -n "$(DEV_KEY_PUB)" || { echo "set SETUP_TOKEN_FILE and DEV_KEY_PUB" >&2; exit 2; }
	cd e2e && pnpm install --frozen-lockfile && GRILL_ROOM_E2E_SETUP_TOKEN_FILE='$(abspath $(SETUP_TOKEN_FILE))' \
	  GRILL_ROOM_E2E_DEV_KEY='$(abspath $(DEV_KEY_PUB))' GRILL_ROOM_E2E_SUPERVISOR='$(abspath $(SUPERVISOR_BIN))' \
	  pnpm exec playwright test -c supervisor/playwright.config.js

# 004-AC13: a room exported from the compose stack is imported on the lab deployment, where a haiku
# turn resumes its session, then moves back: two real turns, at most two Claude requests. Both
# instances must run this build (`make dev-up`, `make k8s-apply`).
e2e-crossinstance: build
	cd e2e && pnpm install --frozen-lockfile && pnpm exec playwright test -c crossinstance/playwright.config.js

# Magic links against the lab deployment and its mailpit (K8S_MAILPIT_URL in .env); no Claude request.
e2e-lab-mail:
	cd e2e && pnpm install --frozen-lockfile && GRILL_ROOM_E2E_LAB=1 pnpm exec playwright test -c mail/playwright.config.js

# 005-AC24: a mention arrives as a real Web Push notification through the browser vendor's push
# service, on the lab deployment (or PUSH_BASE). Push needs a headed Chrome: it runs under xvfb in
# the Playwright image, with --network host so PUSH_BASE may be a local server. No Claude request.
PLAYWRIGHT_IMAGE ?= mcr.microsoft.com/playwright:v1.64.0-noble
e2e-push:
	cd e2e && pnpm install --frozen-lockfile
	docker run --rm --name grill-room-test-push --network host --cpus 2 -e HOME=/tmp -e PUSH_BASE -e PUSH_ADMIN_TOKEN \
	  -v '$(CURDIR)/e2e':/repo/e2e:ro $(if $(wildcard .env),-v '$(CURDIR)/.env':/repo/.env:ro) -w /repo/e2e $(PLAYWRIGHT_IMAGE) \
	  sh -c 'npx -y playwright@1.64.0 install chrome >/tmp/install.log 2>&1 || { tail -5 /tmp/install.log; exit 1; }; \
	    xvfb-run -a npx playwright test -c push/playwright.config.js --output /tmp/out'

# STT worker: lint, types and unit tests. The e2e-marked test runs too once the model is in the
# Hugging Face cache, and skips otherwise.
stt-test:
	cd stt && uv run ruff check . && uv run ruff format --check . && uv run mypy grill_stt tests scripts && uv run pytest

# The tts pod (spec 013): lint, types and unit tests, on the fake engine. No weights needed.
tts-test:
	cd tts && uv run ruff check . && uv run ruff format --check . && uv run mypy grill_tts tests && uv run pytest

# The tts image holds the gated Pocket TTS weights: it goes to a private registry only (spec 013).
# HF_TOKEN_FILE is a file with a Hugging Face read token whose account accepted Kyutai's terms; it
# reaches the build as a BuildKit secret only.
TTS_BUILD_THREADS ?= 2
tts-image:
	@test -n "$(HF_TOKEN_FILE)" && test -r "$(HF_TOKEN_FILE)" || { echo "HF_TOKEN_FILE must name a readable token file, see .env.example" >&2; exit 1; }
	DOCKER_BUILDKIT=1 docker build -t grill-tts:local \
	  --secret id=hf,src='$(HF_TOKEN_FILE)' \
	  --build-arg BUILD_THREADS=$(TTS_BUILD_THREADS) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  tts/

# The scenario tool (scenarios/, 004-AC16): lint, types and unit tests. No audio, no Claude request.
scenarios-test:
	cd scenarios && uv run ruff check . && uv run ruff format --check . && uv run mypy grill_scenarios tests && uv run pytest

# transcribe.cpp compile jobs: CI passes its core count, the default spares a shared workstation.
STT_BUILD_JOBS ?= 2
stt-image:
	docker build -t grill-stt:local \
	  --build-arg BUILD_JOBS=$(STT_BUILD_JOBS) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT_HASH=$(COMMIT_HASH) \
	  --build-arg BUILD_TIMESTAMP=$(BUILD_TS) \
	  --build-arg PROJECT_URL=$(PROJECT_URL) \
	  stt/

# 007: the e2e tests inside the built image with no network, so both engines must load their baked-in
# models offline. STT_TEST_CPUS caps the container on a shared workstation.
STT_TEST_CPUS ?= 2
stt-image-test: stt-image
	$(MAKE) stt-image-e2e

# The same against an already built grill-stt:local (CI builds it with its layer cache).
stt-image-e2e:
	docker build -t grill-stt:test -f stt/Dockerfile.test stt/
	docker run --rm --name grill-stt-test-e2e --network none --read-only --tmpfs /tmp --cpus $(STT_TEST_CPUS) \
	  -e GRILL_STT_IMAGE_TEST=1 -v '$(CURDIR)/stt/pyproject.toml':/app/pyproject.toml:ro \
	  -v '$(CURDIR)/stt/tests':/app/tests:ro -v '$(CURDIR)/e2e/voice/fixtures':/e2e/voice/fixtures:ro \
	  grill-stt:test python -m pytest -p no:cacheprovider -m e2e tests

cover:
	go test -race -coverprofile=coverage.txt ./... && go tool cover -func=coverage.txt | tail -1

# The supervisor against real GitHub releases (003-AC2, 003-AC7): one instance per release in
# RELEASES, and one serving a dev-signed local build. Needs gh and the network.
RELEASES ?= v0.0.1 v0.0.2
test-release:
	for v in $(RELEASES); do gh release download "$$v" -R babs/grill-room -D bin/releases/$$v --clobber || exit 1; done
	GRILL_ROOM_TEST_RELEASES='$(foreach v,$(RELEASES),$(CURDIR)/bin/releases/$(v))' go test -race -count=1 -run TestReleases -v ./cmd/grill-room-daemon/

# Two real haiku turns, one on this machine's Claude Code login, one on the setup token in
# SETUP_TOKEN_FILE, then the token scan of workers and sandboxes and the privacy grep of the rooms
# (003-AC14, 003-AC17). TMPDIR must be short: the supervisor's socket lives under it.
test-live-accounts: sandbox-image
	@test -n "$(SETUP_TOKEN_FILE)" || { echo "set SETUP_TOKEN_FILE to a file holding a claude setup-token token"; exit 2; }
	GRILL_ROOM_TEST_SETUP_TOKEN_FILE='$(abspath $(SETUP_TOKEN_FILE))' GRILL_ROOM_TEST_SANDBOX_IMAGE=$(SANDBOX_IMAGE) \
	  go test -count=1 -run TestLiveAccounts -v ./cmd/grill-room-daemon/

# The trust tests in a container with no network (003-AC3): the embedded trusted root verifies the
# v0.0.1 release bundle, and TUF is unreachable.
test-offline:
	CGO_ENABLED=0 go test -c -o bin/trust.test ./internal/trust/
	docker run --rm --network none -e GRILL_ROOM_TEST_OFFLINE=1 -v '$(CURDIR)/internal/trust:/src:ro' \
	  -v '$(CURDIR)/bin/trust.test:/trust.test:ro' -w /src debian:stable-slim /trust.test -test.v -test.run 'Offline|Release|DevKey'

# The admin pages' libraries, pinned in tools/ui/package.json and copied unmodified with their
# licenses: no CDN, no build step. One copy serves the daemon's page and the server's /admin, whose
# import maps point the bare imports to it.
UI_VENDOR := internal/uivendor/esm
UI_MODULES := preact/dist/preact.mjs:preact.js preact/hooks/dist/hooks.mjs:preact-hooks.js \
  @preact/signals-core/dist/signals-core.mjs:signals-core.js @preact/signals/dist/signals.mjs:signals.js \
  htm/dist/htm.mjs:htm.js htm/preact/index.mjs:htm-preact.js
ui-tools:
	cd tools/ui && pnpm install --frozen-lockfile

ui-vendor: ui-tools
	rm -rf '$(UI_VENDOR)' && mkdir -p '$(UI_VENDOR)'
	for m in $(UI_MODULES); do cp "tools/ui/node_modules/$${m%%:*}" '$(UI_VENDOR)'/"$${m#*:}" || exit 1; done
	for p in preact htm @preact/signals @preact/signals-core; do \
	  cp "tools/ui/node_modules/$$p/LICENSE" '$(UI_VENDOR)'/LICENSE-"$$(echo $$p | tr -d @ | tr / -)" || exit 1; \
	  echo "$$p $$(node -p "require('./tools/ui/node_modules/$$p/package.json').version")"; \
	done > '$(UI_VENDOR)/VERSIONS'

# The admin pages' JSDoc types, checked by tsc (003 Phase 7, 004-AC19).
ui-check: ui-tools
	cd tools/ui && pnpm exec tsc -p tsconfig.json

lint: ui-check
	golangci-lint run ./...
	@out=$$(git ls-files -co --exclude-standard '*.go' | xargs gofmt -l); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	git ls-files -co --exclude-standard '*.sh' | xargs shellcheck

# DAEMON_DIST=<dir> ships that daemon release (a GitHub release or `make dev-release`, bundles
# included) instead of building an unsigned one in the image. DAEMON_PREV_DIST=<dir> adds the
# previous release, for supervisors too old for this one's workers.
docker-build:
	docker build -t $(APP_NAME):local $(if $(DAEMON_DIST),--build-context daemon-dist='$(DAEMON_DIST)') $(if $(DAEMON_PREV_DIST),--build-context daemon-prev='$(DAEMON_PREV_DIST)') \
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

# push tags local image $(1) as $(REGISTRY)/grill-room/$(2):$(VERSION), pushes it and prints the
# digest-pinned reference as the .env line $(3)=<ref>. REGISTRY comes from .env.
define push
@test -n "$(REGISTRY)" || { echo "REGISTRY is not set, see .env.example" >&2; exit 1; }
docker tag $(1) $(REGISTRY)/grill-room/$(2):$(VERSION)
docker push $(REGISTRY)/grill-room/$(2):$(VERSION)
@docker image inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' $(REGISTRY)/grill-room/$(2):$(VERSION) \
  | grep '^$(REGISTRY)/grill-room/$(2)@' | sed 's|@|:$(VERSION)@|; s/^/$(3)=/'
endef

# The sandbox image the room server advertises to daemons (GRILL_ROOM_SANDBOX_IMAGE).
sandbox-push: sandbox-image
	$(call push,$(SANDBOX_IMAGE),sandbox,GRILL_ROOM_SANDBOX_IMAGE)

# Lab deployment (deploy/k8s/): plain manifests rendered from .env, applied with kubectl to an
# existing namespace. Set KUBECONFIG explicitly: the targets refuse to fall back to the default one.
K8S_NAMESPACE ?= grill-room
K8S_OUT       := deploy/k8s/out
K8S_VARS      := K8S_DOMAIN K8S_STORAGE_CLASS GRILL_ROOM_IMAGE GRILL_STT_IMAGE GRILL_TTS_IMAGE GRILL_ROOM_ADMIN_TOKEN STT_TOKEN TTS_TOKEN LIVEKIT_API_KEY LIVEKIT_API_SECRET
KUBECTL        = kubectl -n '$(K8S_NAMESPACE)'
# Optional: an empty GRILL_ROOM_SMTP_ADDR turns email sign-in off. The NetworkPolicy opens
# K8S_SMTP_PORT to the namespace K8S_SMTP_NAMESPACE, where the relay runs.
K8S_SMTP_NAMESPACE ?= mailpit
K8S_SMTP_PORT      ?= 1025
K8S_OPTIONAL  := GRILL_ROOM_SANDBOX_IMAGE GRILL_ROOM_TRUSTED_PROXIES GRILL_ROOM_SMTP_ADDR GRILL_ROOM_SMTP_FROM K8S_SMTP_NAMESPACE K8S_SMTP_PORT
# Exported to the render recipe only: no other recipe sees the secrets in its environment.
$(foreach v,$(K8S_VARS) $(K8S_OPTIONAL),$(eval k8s-render: export $(v) := $$($(v))))

# The room server, STT and tts images, pushed for the lab; prints the .env lines to paste. REGISTRY
# must be private: the tts image holds gated weights (spec 013).
k8s-push: docker-build stt-image tts-image
	$(call push,$(APP_NAME):local,server,GRILL_ROOM_IMAGE)
	$(call push,grill-stt:local,stt,GRILL_STT_IMAGE)
	$(call push,grill-tts:local,tts,GRILL_TTS_IMAGE)

# Secrets land in $(K8S_OUT) (untracked, owner-only): the templates hold none.
k8s-render:
	@for v in $(K8S_VARS); do test -n "$$(printenv "$$v")" || { echo "$$v is not set, see .env.example" >&2; exit 1; }; done
	@for v in GRILL_ROOM_IMAGE GRILL_STT_IMAGE GRILL_TTS_IMAGE; do printenv "$$v" | grep -Eq '@sha256:[0-9a-f]{64}$$' \
	  || { echo "$$v must be pinned by digest: run make k8s-push" >&2; exit 1; }; done
	@for v in GRILL_ROOM_ADMIN_TOKEN STT_TOKEN TTS_TOKEN LIVEKIT_API_KEY LIVEKIT_API_SECRET; do printenv "$$v" | grep -Eq '^[A-Za-z0-9._~+/=-]+$$' \
	  || { echo "$$v holds characters the Secret template cannot quote: use [A-Za-z0-9._~+/=-]" >&2; exit 1; }; done
	@printenv K8S_SMTP_PORT | grep -Eq '^[0-9]+$$' || { echo "K8S_SMTP_PORT must be a port number" >&2; exit 1; }
	@printenv GRILL_ROOM_SMTP_FROM | grep -q '"' && { echo "GRILL_ROOM_SMTP_FROM must not hold a double quote" >&2; exit 1; } || true
	rm -rf '$(K8S_OUT)' && (umask 077 && mkdir -p '$(K8S_OUT)')
	@for f in deploy/k8s/*.yaml; do \
	  (umask 077 && ROOM_HOST=grill-room.$$K8S_DOMAIN LIVEKIT_HOST=grill-room-livekit.$$K8S_DOMAIN TURN_HOST=grill-room-turn.$$K8S_DOMAIN \
	    envsubst '$${K8S_STORAGE_CLASS} $${GRILL_ROOM_IMAGE} $${GRILL_STT_IMAGE} $${GRILL_TTS_IMAGE} $${GRILL_ROOM_SANDBOX_IMAGE} $${GRILL_ROOM_TRUSTED_PROXIES} $${GRILL_ROOM_SMTP_ADDR} $${GRILL_ROOM_SMTP_FROM} $${K8S_SMTP_NAMESPACE} $${K8S_SMTP_PORT} $${ROOM_HOST} $${LIVEKIT_HOST} $${TURN_HOST} $${GRILL_ROOM_ADMIN_TOKEN} $${STT_TOKEN} $${TTS_TOKEN} $${LIVEKIT_API_KEY} $${LIVEKIT_API_SECRET}' \
	    < $$f > '$(K8S_OUT)'/$${f##*/}) || exit 1; \
	done
	@echo "rendered $(K8S_OUT): https://grill-room.$$K8S_DOMAIN/"

k8s-check:
	@test -n "$$KUBECONFIG" || { echo "KUBECONFIG is not set: point it at the lab cluster's file" >&2; exit 1; }
	@$(KUBECTL) get ns '$(K8S_NAMESPACE)' -o name >/dev/null || { echo "namespace $(K8S_NAMESPACE) is missing: create it first" >&2; exit 1; }

k8s-apply: k8s-render k8s-check
	$(KUBECTL) apply -f '$(K8S_OUT)'
	$(KUBECTL) rollout status deploy --timeout=10m

# Removes what k8s-apply created, the data volume included. The namespace stays.
k8s-delete: k8s-render k8s-check
	$(KUBECTL) delete --ignore-not-found -f '$(K8S_OUT)'

# 007-AC19: the STT queue alert and its scrape, for a cluster running the VictoriaMetrics operator.
K8S_MONITORING_NAMESPACE ?= monitoring
k8s-monitoring: k8s-check
	for f in deploy/monitoring/*.yaml; do K8S_MONITORING_NAMESPACE='$(K8S_MONITORING_NAMESPACE)' envsubst '$${K8S_MONITORING_NAMESPACE}' \
	  < "$$f" | $(KUBECTL) apply -f - || exit 1; done

k8s-monitoring-delete: k8s-check
	for f in deploy/monitoring/*.yaml; do K8S_MONITORING_NAMESPACE='$(K8S_MONITORING_NAMESPACE)' envsubst '$${K8S_MONITORING_NAMESPACE}' \
	  < "$$f" | $(KUBECTL) delete --ignore-not-found -f - || exit 1; done

# Office fixtures for the pandoc tests, written by the pandoc release the image ships.
FIXTURES := internal/convert/testdata
# Favicons and app icons from the logo (D32), into both embedded UIs. Needs Inkscape and ImageMagick.
# The apple-touch-icon is opaque: iOS paints transparent corners black and rounds them itself.
# The notification badge is the logo's shapes in white, without the tile: Android keeps only its alpha.
LOGO      := design/icons/round2/r2-03-b-three-quarter.svg
ICON_DIRS := internal/web/ui internal/daemon/ui
icons:
	tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	for n in 16 32 48 192 512; do inkscape '$(LOGO)' --export-type=png -w $$n -h $$n -o "$$tmp/$$n.png" || exit 1; done && \
	inkscape '$(LOGO)' --export-type=png -w 180 -h 180 --export-background='#159F8C' --export-background-opacity=1 -o "$$tmp/180.png" && \
	magick "$$tmp/16.png" "$$tmp/32.png" "$$tmp/48.png" "$$tmp/favicon.ico" && \
	sed '/<rect /d' '$(LOGO)' | inkscape --pipe --export-type=png -w 96 -h 96 -o "$$tmp/badge.png" && \
	magick "$$tmp/badge.png" -fill white -colorize 100 internal/web/ui/icons/badge-96.png && \
	for d in $(ICON_DIRS); do \
	  mkdir -p "$$d/icons" && cp '$(LOGO)' "$$d/logo.svg" && cp "$$tmp/favicon.ico" "$$d/favicon.ico" && \
	  cp "$$tmp/32.png" "$$d/icons/icon-32.png" && cp "$$tmp/180.png" "$$d/icons/apple-touch-icon.png" && \
	  cp "$$tmp/192.png" "$$d/icons/icon-192.png" && cp "$$tmp/512.png" "$$d/icons/icon-512.png" || exit 1; \
	done

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
