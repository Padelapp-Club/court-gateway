#!/bin/sh
# Reproducible release build: the same source, Go version and inputs give
# byte-identical binaries, so anyone can rebuild a release and compare its
# SHA-256 with ours.
#
#   VERSION           version to embed (default: contents of VERSION)
#   RELEASE_KEY       base64 ed25519 public key of release manifests, or
#   RELEASE_KEY_FILE  a file with it, outside the source tree (the release
#                     page publishes it as release.pub). Neither: a
#                     self-built binary that never updates itself. The key
#                     is never read from the source tree implicitly.
#   DEFAULT_HUB_URL   the hub a release connects to unless setup is given
#                     --hub-url; empty (a self-build): setup needs --hub-url
#   OUT               output directory (default: dist)
#   TARGETS           space-separated os/arch list (default: all release targets)
#
# Usage: scripts/build.sh            then compare dist/SHA256SUMS
set -eu

cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(cat VERSION)}"
RELEASE_KEY="${RELEASE_KEY:-}"
if [ -n "${RELEASE_KEY_FILE:-}" ]; then
	case "$(cd "$(dirname "$RELEASE_KEY_FILE")" && pwd)/" in
	"$(pwd)"/*)
		echo "build.sh: RELEASE_KEY_FILE must be outside the source tree" >&2
		exit 2
		;;
	esac
	RELEASE_KEY="$(tr -d ' \n' < "$RELEASE_KEY_FILE")"
fi
DEFAULT_HUB_URL="${DEFAULT_HUB_URL:-}"
OUT="${OUT:-dist}"
TARGETS="${TARGETS:-linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64}"

export CGO_ENABLED=0
export GOFLAGS=-mod=readonly
export GOAMD64=v1 GOARM64=v8.0
LDFLAGS="-s -w -buildid= -X main.version=${VERSION} -X main.releaseKey=${RELEASE_KEY} -X main.defaultHubURL=${DEFAULT_HUB_URL}"

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
