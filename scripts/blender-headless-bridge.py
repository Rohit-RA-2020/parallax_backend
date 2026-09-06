"""Persistent headless Blender running the MCP bridge.

Usage:
    blender --background --python scripts/blender-headless-bridge.py -- [port] [host]

Keeps Blender alive with the djeada/blender-mcp-server addon TCP bridge
listening (default 127.0.0.1:9876) so the Director's blender_scene_info and
blender_run_script tools have a live session. Renders (blender_render) do
NOT need this — they spawn their own `blender --background` jobs.

The addon source is expected at ../blender-mcp-server/addon (clone via
scripts/setup-blender-mcp.sh) or BLENDER_MCP_ADDON env.
Set BLENDER_MCP_PIN (commit hash) via setup script for reproducibility.
"""
import argparse
import os
import sys
import threading
import time

# Blender puts args after `--` into sys.argv. When run outside Blender
# (plain python for --help), sys.argv is normal argparse args.
_argv = sys.argv[1:]
if "--" in _argv:
    _argv = _argv[_argv.index("--") + 1:]

parser = argparse.ArgumentParser(description="Headless Blender MCP bridge")
parser.add_argument("port", nargs="?", type=int, default=int(os.environ.get("BLENDER_BRIDGE_PORT", "9876")),
                    help="TCP port for the MCP bridge (default 9876)")
parser.add_argument("host", nargs="?", default=os.environ.get("BLENDER_BRIDGE_HOST", "127.0.0.1"),
                    help="Bind host (default 127.0.0.1; keep loopback)")
args = parser.parse_args(_argv)

PORT = args.port
HOST = args.host
if not (1 <= PORT <= 65535):
    print(f"blender-headless-bridge: invalid port {PORT}", file=sys.stderr)
    raise SystemExit(2)
if HOST not in ("127.0.0.1", "localhost", "::1"):
    print(f"blender-headless-bridge: refusing non-loopback host {HOST!r} "
          "(bridge has no auth; bind loopback only)", file=sys.stderr)
    raise SystemExit(2)

CANDIDATES = [
    os.environ.get("BLENDER_MCP_ADDON", ""),
    os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                 "..", "blender-mcp-server", "addon"),
    "/tmp/blender-mcp-server/addon",
]

addon_dir = next((c for c in CANDIDATES if c and os.path.isfile(os.path.join(c, "__init__.py"))), "")
if not addon_dir:
    print("blender-headless-bridge: addon not found; run scripts/setup-blender-mcp.sh first",
          file=sys.stderr)
    raise SystemExit(1)

sys.path.insert(0, os.path.dirname(addon_dir))
try:
    import addon as bridge  # noqa: E402  (addon/__init__.py)
except ImportError as exc:
    print(f"blender-headless-bridge: cannot import addon at {addon_dir}: {exc}",
          file=sys.stderr)
    raise SystemExit(1)

for attr in ("BlenderMCPServer",):
    if not hasattr(bridge, attr):
        print(f"blender-headless-bridge: addon at {addon_dir} has no {attr} "
              "(version mismatch? re-run scripts/setup-blender-mcp.sh)",
              file=sys.stderr)
        raise SystemExit(1)

# Upstream addon exposes PORT / HOST globals in some revisions; set when present.
if hasattr(bridge, "PORT"):
    bridge.PORT = PORT
if hasattr(bridge, "HOST"):
    bridge.HOST = HOST

server = bridge.BlenderMCPServer()
try:
    try:
        server.start()
    except TypeError:
        # Some revisions take (host, port).
        server.start(HOST, PORT)
except OSError as exc:
    print(f"blender-headless-bridge: cannot listen on {HOST}:{PORT}: {exc}",
          file=sys.stderr)
    raise SystemExit(1)

print(f"blender-headless-bridge: listening on {HOST}:{PORT}", flush=True)

# Park forever without binding extra ports. threading.Event().wait() sleeps
# until SIGTERM/SIGINT; Blender's bpy timers + bridge thread keep running.
stop = threading.Event()
try:
    while not stop.wait(timeout=3600):
        pass
except KeyboardInterrupt:
    pass
finally:
    try:
        server.stop()
    except Exception as exc:  # noqa: BLE001 - best effort shutdown
        print(f"blender-headless-bridge: stop failed: {exc}", file=sys.stderr)
