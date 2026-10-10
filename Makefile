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

.PHONY: build run test e2e cover lint docker-build dev-up dev-down traefik-install traefik-uninstall clean

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) $(PKG)

run: build
	./bin/$(APP_NAME)

test:
	go test -race -cover ./...

# Browser tests against a fresh server, in the installed Chrome.
e2e: build
	cd e2e && pnpm install --frozen-lockfile && pnpm test

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
