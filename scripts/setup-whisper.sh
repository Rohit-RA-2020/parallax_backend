#!/usr/bin/env bash
# Create a local venv and install faster-whisper (no sudo).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
VENV="$ROOT/.venv"
PYTHON="${PYTHON:-python3}"

if ! "$PYTHON" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 9) else 1)'; then
  echo "Need Python 3.9+. This machine has: $($PYTHON --version 2>&1)" >&2
  exit 1
fi

if [ ! -x "$VENV/bin/python" ]; then
  "$PYTHON" -m venv --without-pip "$VENV"
fi
if [ ! -x "$VENV/bin/pip" ]; then
  curl -fsSL https://bootstrap.pypa.io/get-pip.py | "$VENV/bin/python"
fi
"$VENV/bin/python" -m pip install -U pip
"$VENV/bin/python" -m pip install -r "$ROOT/requirements-whisper.txt"
echo
echo "Installed. Point Parallax at:"
echo "  WHISPER_PYTHON=$VENV/bin/python"
echo "  WHISPER_MODEL=large-v3-turbo"
echo "  WHISPER_DEVICE=auto"
echo "  WHISPER_COMPUTE=int8"
