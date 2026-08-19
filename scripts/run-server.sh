#!/usr/bin/env bash
# Start the Parallax backend from this checkout.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

export PATH="/usr/local/go/bin:/opt/ffmpeg-nvenc/bin:/usr/lib/jellyfin-ffmpeg:${PATH}"

if [[ ! -f .env ]]; then
  echo "Missing $ROOT/.env — copy .env.example and fill in API keys." >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "Go is not on PATH. Open a new shell or run: source /etc/profile.d/parallax-go.sh" >&2
  exit 1
fi

mkdir -p bin
go build -o bin/parallax ./cmd/server
exec ./bin/parallax
