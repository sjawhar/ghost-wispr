.PHONY: build frontend-build embed-assets backend-build clean

build: frontend-build embed-assets backend-build

frontend-build:
	npm --prefix web run build

embed-assets:
	rm -rf cmd/ghost-wispr/static
	mkdir -p cmd/ghost-wispr/static
	cp -R web/dist/. cmd/ghost-wispr/static/

backend-build:
	go build -o ghost-wispr ./cmd/ghost-wispr

clean:
	rm -rf web/dist cmd/ghost-wispr/static ghost-wispr
