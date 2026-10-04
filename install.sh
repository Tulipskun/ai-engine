#!/usr/bin/env bash
set -euo pipefail

REPO="Tulipskun/ai-engine"
PREFIX="${AI_ENGINE_PREFIX:-}"

case "$(uname -s)" in
  Linux) ;;
  *) echo "ai-engine installer: Linux only" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "ai-engine installer: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [[ -z "$PREFIX" ]]; then
  if [[ -d /usr/local/bin && -w /usr/local/bin ]]; then PREFIX="/usr/local/bin"; else PREFIX="$HOME/.local/bin"; fi
fi

mkdir -p "$PREFIX"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

download() {
  curl -fL --retry 3 --retry-delay 1 --connect-timeout 10 "$1" -o "$2"
}

echo "Installing ai-engine ($ARCH) to $PREFIX..."
download "https://github.com/$REPO/releases/download/latest/ai-engine-linux-$ARCH" "$TMP_DIR/ai-engine"
chmod 755 "$TMP_DIR/ai-engine"
mv "$TMP_DIR/ai-engine" "$PREFIX/ai-engine"

if ! command -v cloudflared >/dev/null 2>&1; then
  echo "Installing cloudflared..."
  download "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$ARCH" "$TMP_DIR/cloudflared"
  chmod 755 "$TMP_DIR/cloudflared"
  mv "$TMP_DIR/cloudflared" "$PREFIX/cloudflared"
fi

echo
echo "Installed:"
echo "  $PREFIX/ai-engine"
echo "  $PREFIX/cloudflared"
echo
echo "Run with:"
echo "  export CF_TOKEN='YOUR_CLOUDFLARE_API_TOKEN'"
echo "  ai-engine"
if [[ ":$PATH:" != *":$PREFIX:"* ]]; then
  echo
  echo "Add to PATH if needed:"
  echo "  export PATH="$PREFIX:$PATH""
fi
