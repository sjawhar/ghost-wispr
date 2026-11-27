# The call bridge's image (dispatch://INFRA-5904, contract C7), built from the repository's root by the
# bridge-images job of .github/workflows/release.yml: ghost-wispr-bridge with no build tags, and
# ghost-wispr-bridge-test with BUILD_TAGS=callfault, the test pool's build. Each image it starts from
# is pinned by digest; deploy/bridge/release_images_test.sh checks the pins and the build.
FROM golang:1.25.14@sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/ internal/
COPY cmd/ghost-wispr-bridge/ cmd/ghost-wispr-bridge/
# callfault gives the test image's binary the run spec, the fake Pi and the network probe.
ARG BUILD_TAGS=
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "$BUILD_TAGS" -o /ghost-wispr-bridge ./cmd/ghost-wispr-bridge

FROM gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=build /ghost-wispr-bridge /ghost-wispr-bridge
USER nonroot
ENTRYPOINT ["/ghost-wispr-bridge"]
