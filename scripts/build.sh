#!/usr/bin/env bash
#
# Build the clip service.
#
# The result is one self-contained static Go binary: the HTTP server, the
# Postgres driver and the rendered page (a //go:embed'd HTML file) are all
# inside it. Deploying is therefore "copy the file, restart the unit" — and
# because the page is embedded, the assets a running process serves can never
# drift from the code it is running.
#
# There is no frontend build step on purpose. The clipboard page is a single
# HTML document with a nonce'd inline script, served by this same binary; a
# bundler would add a toolchain, a build order and a stale-artifact failure mode
# in exchange for nothing this page needs. (The teleport service in its own
# repository does have a Vue build; it is a different shape of application.)
#
# Usage: scripts/build.sh [output-path]
#        Default output: bin/clip-linux-amd64
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-$REPO/bin/clip-linux-amd64}"

# `git describe --always --dirty` gives e.g. "278faa5" or "278faa5-dirty",
# which is exactly the granularity wanted: a short SHA, plus a marker when the
# working tree has uncommitted changes (so nobody mistakes a dirty build for a
# clean one).
VERSION="$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo unknown)"

echo "==> compiling clip"
mkdir -p "$(dirname "$OUT")"
cd "$REPO"

# CGO_ENABLED=0 matters: it is what makes the result a static executable with no
# libc dependency, so it runs on the server regardless of its glibc version.
# (pgx is pure Go, so nothing here needs cgo.)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o "$OUT" \
  ./cmd/clip

echo "  version:  $VERSION"
echo "  size:     $(du -h "$OUT" | cut -f1)"
echo "  sha256:   $(sha256sum "$OUT" | cut -d' ' -f1)"
echo
echo "  verify:   $("$OUT" version)"
echo "  page:     $("$OUT" version --frontend)"
