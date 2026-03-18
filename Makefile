DEPLOY_DIR := /opt/ghost-wispr
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)

.PHONY: build frontend-build embed-assets backend-build clean run deploy setup

build: frontend-build embed-assets backend-build

frontend-build:
	npm --prefix web run build

embed-assets:
	rm -rf cmd/ghost-wispr/static
	mkdir -p cmd/ghost-wispr/static
	cp -R web/dist/. cmd/ghost-wispr/static/

backend-build:
	go build -ldflags '$(LDFLAGS)' -o ghost-wispr ./cmd/ghost-wispr

clean:
	rm -rf web/dist cmd/ghost-wispr/static ghost-wispr

# Dev: build and run locally on :8081
run: build
	GHOST_WISPR_ADDR=:8081 ./ghost-wispr

# Production: build, deploy to /opt, restart service
deploy: build
	systemctl --user stop ghost-wispr || true
	cp ghost-wispr $(DEPLOY_DIR)/ghost-wispr
	systemctl --user start ghost-wispr
	@sleep 2 && curl -s localhost:8080/api/version

# First-time Pi setup (one-time)
setup:
	./setup.sh
