# syntax=docker/dockerfile:1
# Multi-stage, static, minimal: the final image is distroless/static
# (CA certificates and time zones, no shell, no package manager) plus one
# binary. Build: docker build -t court-gateway:local .
FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=
# the release key comes only as this build argument (the release page
# publishes it as release.pub); without it the image is self-built and
# never updates itself
ARG RELEASE_KEY=
# the hub a release connects to unless setup is given --hub-url
ARG DEFAULT_HUB_URL=
# set by BuildKit (docker buildx build --platform ...); the legacy builder
# leaves them empty and the build stage's own platform is used
ARG TARGETOS
ARG TARGETARCH
# the same flags as scripts/build.sh, so the binary inside the image is
# byte-identical to the release binary of the same platform
RUN V="${VERSION:-$(cat VERSION)}" && \
    CGO_ENABLED=0 GOOS="${TARGETOS:-$(go env GOOS)}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" GOFLAGS=-mod=readonly GOAMD64=v1 GOARM64=v8.0 \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= -X main.version=${V} -X main.releaseKey=${RELEASE_KEY} -X main.defaultHubURL=${DEFAULT_HUB_URL}" \
      -o /out/court-gateway ./cmd/court-gateway

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/court-gateway /court-gateway
# Config is mounted at /config; secrets come as environment variables
# (compose env_file). /court-gateway is never replaced: it starts the newest
# verified update kept in the state directory (versions/), or itself. The
# state directory lives in the container layer on purpose: no volume, no
# CRM data, and a recreated container simply starts from the image.
ENV GATEWAY_CONFIG=/config/gateway.yaml \
    GATEWAY_STATE_DIR=/home/nonroot/state
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
  CMD ["/court-gateway", "healthcheck"]
ENTRYPOINT ["/court-gateway"]
CMD ["serve"]
