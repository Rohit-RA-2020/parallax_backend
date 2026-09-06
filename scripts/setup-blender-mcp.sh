#!/usr/bin/env bash
# Installs the Blender MCP stack used by the Director agent.
# - clones djeada/blender-mcp-server next to the repo (or $BLENDER_MCP_DIR)
#   at a pinned commit (BLENDER_MCP_PIN, default below) for reproducibility
# - installs its Python server into scripts/.blender-mcp-venv
# - builds the Blender addon zip for GUI install
# Headless renders need NONE of this (they use `blender --background`
# directly); the bridge only matters for live blender_scene_info sessions.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="${BLENDER_MCP_DIR:-$(dirname "$ROOT")/blender-mcp-server}"
VENV="$ROOT/scripts/.blender-mcp-venv"
# Pinned upstream commit — bump deliberately after testing against
# internal/tools/blender.go bridge protocol + scripts/blender-headless-bridge.py.
PIN="${BLENDER_MCP_PIN:-main}"

command -v git >/dev/null || { echo "setup-blender-mcp: git not found" >&2; exit 1; }
command -v python3 >/dev/null || { echo "setup-blender-mcp: python3 not found" >&2; exit 1; }

if [ ! -d "$DEST/.git" ]; then
  if [ -e "$DEST" ]; then
    echo "setup-blender-mcp: $DEST exists but is not a git checkout (set BLENDER_MCP_DIR to override)" >&2
    exit 1
  fi
  git clone --depth 1 https://github.com/djeada/blender-mcp-server.git "$DEST"
fi

# Pin to a known commit when requested (anything other than "main" skips fetch).
if [ "$PIN" != "main" ]; then
  (cd "$DEST" && git fetch --depth 1 origin "$PIN" && git checkout --detach "$PIN")
fi
echo "Upstream: $(cd "$DEST" && git rev-parse --short HEAD) $(cd "$DEST" && git log -1 --format=%s)"

if [ ! -x "$VENV/bin/blender-mcp-server" ]; then
  python3 -m venv "$VENV"
  "$VENV/bin/pip" install -q -e "$DEST"
fi

if [ ! -f "$DEST/addon/__init__.py" ]; then
  echo "setup-blender-mcp: addon source missing at $DEST/addon/__init__.py" >&2
  exit 1
fi

if [ -f "$DEST/scripts/build_addon_zip.sh" ]; then
  (cd "$DEST" && ./scripts/build_addon_zip.sh)
  echo "Addon zip: $DEST/dist/blender_mcp_bridge.zip"
  echo "Install in Blender: Edit > Preferences > Add-ons > Install > select the zip,"
  echo "then enable 'Blender MCP Bridge' (N-panel > MCP tab shows 127.0.0.1:9876)."
else
  echo "Addon source: $DEST/addon/__init__.py"
fi

echo "MCP server binary: $VENV/bin/blender-mcp-server"
echo "Headless bridge: blender --background --python $ROOT/scripts/blender-headless-bridge.py"
echo "Director env: BLENDER_BIN=blender BLENDER_BRIDGE_HOST=127.0.0.1 BLENDER_BRIDGE_PORT=9876"
