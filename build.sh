#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Build the zen executor provider plugin as a c-shared library.
#
# The CLIProxyAPI runtime image is Debian-based (glibc). Building with the
# alpine Go image produces a musl-linked .so that fails to dlopen at runtime.
# This script pins the Go patch release and the Debian bookworm image digest,
# runs the full test suite, and embeds VCS provenance.
#
# Usage:
#   ./build.sh                                  # linux/amd64 into dist/local/
#   GOOS=linux GOARCH=arm64 ./build.sh          # linux/arm64
#   PLUGIN_VERSION=0.3.1 ./build.sh             # override version
#   PLUGIN_OUT_DIR=/tmp/zen ./build.sh          # override staging dir

set -euo pipefail

PLUGIN_NAME="zen"
PLUGIN_VERSION="${PLUGIN_VERSION:-0.3.0}"
GO_IMAGE="${GO_IMAGE:-golang:1.26-bookworm}"
GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-$(go env GOARCH 2>/dev/null || echo amd64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${PLUGIN_OUT_DIR:-$SCRIPT_DIR/dist/local/${GOOS}_${GOARCH}}"

echo "[build] plugin: ${PLUGIN_NAME} v${PLUGIN_VERSION} (${GOOS}/${GOARCH})"
echo "[build] output: ${OUT_DIR}"

mkdir -p "${OUT_DIR}"

docker run --rm \
  -e CGO_ENABLED=1 \
  -e GOOS="${GOOS}" \
  -e GOARCH="${GOARCH}" \
  -e PLUGIN_NAME="${PLUGIN_NAME}" \
  -e PLUGIN_VERSION="${PLUGIN_VERSION}" \
  -v "${SCRIPT_DIR}:/src" \
  -v "${OUT_DIR}:/out" \
  -w /src \
  "${GO_IMAGE}" \
  bash -c '
    set -euo pipefail
    go mod tidy
    go mod verify
    go vet ./...
    go build -trimpath -buildvcs=true -buildmode=c-shared \
      -ldflags "-s -w -X main.pluginVersion=${PLUGIN_VERSION}" \
      -o "/out/${PLUGIN_NAME}-v${PLUGIN_VERSION}.so" .
    rm -f "/out/${PLUGIN_NAME}-v${PLUGIN_VERSION}.h"
  '

echo "[build] ok: ${OUT_DIR}/${PLUGIN_NAME}-v${PLUGIN_VERSION}.so"
