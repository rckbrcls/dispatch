#!/bin/sh
# Instala o dispatch: baixa o binário do GitHub Releases, confere o SHA-256
# e instala em $DISPATCH_INSTALL_DIR (padrão ~/.local/bin).
#   curl -fsSL <url>/install.sh | sh
#   curl -fsSL <url>/install.sh | DISPATCH_VERSION=v0.1.0 sh
set -eu

REPO="${DISPATCH_REPO:-rckbrcls/dispatch}"
VERSION="${DISPATCH_VERSION:-latest}"
DEST="${DISPATCH_INSTALL_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$os" in darwin|linux) ;; *) echo "dispatch: sistema não suportado: $os" >&2; exit 1 ;; esac
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "dispatch: arquitetura não suportada: $arch" >&2; exit 1 ;;
esac

if [ -n "${DISPATCH_BASE_URL:-}" ]; then
  base="$DISPATCH_BASE_URL"
elif [ "$VERSION" = latest ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi
name="dispatch_${os}_${arch}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "baixando $name ($VERSION)"
curl -fsSL "$base/$name" -o "$tmp/$name"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

want=$(grep " $name\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || { echo "dispatch: checksum de $name não encontrado" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$name" | awk '{print $1}')
else got=$(shasum -a 256 "$tmp/$name" | awk '{print $1}'); fi
[ "$got" = "$want" ] || { echo "dispatch: checksum não confere" >&2; exit 1; }

mkdir -p "$DEST"
install -m 0755 "$tmp/$name" "$DEST/dispatch"
echo "instalado em $DEST/dispatch"
case ":$PATH:" in *":$DEST:"*) ;; *) echo "adicione $DEST ao PATH" ;; esac
echo "próximo passo: dispatch skill install"
