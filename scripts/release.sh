#!/bin/sh
# Compila para darwin/linux (arm64/amd64) em dist/ e gera checksums.txt.
# Uso: scripts/release.sh <versão>   (publicar: gh release create <versão> dist/*)
# A versão é embutida no binário (dispatch version / dispatch update).
set -eu
cd "$(dirname "$0")/.."
VERSION="${1:-$(git describe --tags --always --dirty)}"
rm -rf dist && mkdir dist
for os in darwin linux; do
  for arch in arm64 amd64; do
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o "dist/dispatch_${os}_${arch}" ./cmd/dispatch
  done
done
(cd dist && shasum -a 256 dispatch_* > checksums.txt)
cat dist/checksums.txt
