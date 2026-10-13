#!/bin/sh
# Reproducible release build: the same source, Go version and inputs give
# byte-identical binaries, so anyone can rebuild a release and compare its
# SHA-256 with ours.
#
#   VERSION      version to embed (default: contents of VERSION)
#   RELEASE_KEY  base64 ed25519 public key of release manifests; empty
#                makes a self-built binary that never updates itself.
#                Official releases embed the key published in release.pub.
#   OUT          output directory (default: dist)
#   TARGETS      space-separated os/arch list (default: all release targets)
#
# Usage: scripts/build.sh            then compare dist/SHA256SUMS
set -eu

cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(cat VERSION)}"
RELEASE_KEY="${RELEASE_KEY:-}"
if [ -z "$RELEASE_KEY" ] && [ -f release.pub ]; then
	RELEASE_KEY="$(tr -d ' \n' < release.pub)"
fi
OUT="${OUT:-dist}"
TARGETS="${TARGETS:-linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64}"

export CGO_ENABLED=0
export GOFLAGS=-mod=readonly
export GOAMD64=v1 GOARM64=v8.0
LDFLAGS="-s -w -buildid= -X main.version=${VERSION} -X main.releaseKey=${RELEASE_KEY}"

mkdir -p "$OUT"
echo "go: $(go version)"
for target in $TARGETS; do
	os="${target%/*}"
	arch="${target#*/}"
	name="court-gateway-${VERSION}-${os}-${arch}"
	[ "$os" = windows ] && name="${name}.exe"
	GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o "$OUT/$name" ./cmd/court-gateway
	echo "built $OUT/$name"
done
(
	cd "$OUT"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum court-gateway-"${VERSION}"-* > SHA256SUMS
	else
		shasum -a 256 court-gateway-"${VERSION}"-* > SHA256SUMS
	fi
)
cat "$OUT/SHA256SUMS"
