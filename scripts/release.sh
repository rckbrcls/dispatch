#!/bin/sh
# Compila para darwin/linux (arm64/amd64) em dist/ e gera checksums.txt.
# Uso: scripts/release.sh [versão]   (publicar: gh release create <versão> dist/*)
set -eu
cd "$(dirname "$0")/.."
rm -rf dist && mkdir dist
for os in darwin linux; do
  for arch in arm64 amd64; do
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" \
      -o "dist/dispatch_${os}_${arch}" ./cmd/dispatch
  done
done
(cd dist && shasum -a 256 dispatch_* > checksums.txt)
cat dist/checksums.txt
