#!/usr/bin/env python3
"""faster-whisper sidecar. CLI prints one JSON object; serve keeps the model loaded."""

from __future__ import annotations

import argparse
import glob
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def _add_nvidia_lib_path() -> None:
    root = os.path.join(sys.prefix, "lib", f"python{sys.version_info.major}.{sys.version_info.minor}", "site-packages", "nvidia")
    libs = [p for p in glob.glob(os.path.join(root, "*", "lib")) if os.path.isdir(p)]
    if not libs:
        return
    current = os.environ.get("LD_LIBRARY_PATH", "")
    os.environ["LD_LIBRARY_PATH"] = ":".join(libs + ([current] if current else []))
    try:
        import ctypes
        for name in ("libcudart.so.12", "libcublas.so.12", "libcudnn.so.9"):
            matches = glob.glob(os.path.join(root, "*", "lib", name))
            if matches:
                ctypes.CDLL(matches[0], mode=ctypes.RTLD_GLOBAL)
    except OSError:
        pass


def load_model(name: str, device: str, compute: str):
    from faster_whisper import WhisperModel

    return WhisperModel(name, device=device, compute_type=compute)


def pick_model(name: str, device: str, compute: str):
    device = (device or "auto").lower()
    compute = compute or "int8"
    if device != "auto":
        return load_model(name, device, compute), device
    try:
        return load_model(name, "cuda", compute), "cuda"
    except Exception as exc:
        print(f"cuda unavailable ({exc}); using cpu", file=sys.stderr, flush=True)
        return load_model(name, "cpu", "int8"), "cpu"


def collect(model, wav, on_progress=None, on_segment=None, on_meta=None):
    segments, info = model.transcribe(
        wav,
        language=None,
        word_timestamps=True,
        vad_filter=True,
        beam_size=5,
    )
    duration = float(getattr(info, "duration", 0) or 0)
    language = (info.language or "").lower()
    if on_meta:
        on_meta(language, duration)
    out_segments = []
    words = []
    for i, seg in enumerate(segments):
        text = (seg.text or "").strip()
        if not text:
            continue
        end = float(seg.end or 0)
        item = {
            "id": f"seg-{i:04d}",
            "start": float(seg.start or 0),
            "end": end,
            "text": text,
        }
        out_segments.append(item)
        seg_words = []
        for word in seg.words or []:
            token = (word.word or "").strip()
            if not token:
                continue
            w = {
                "start": float(word.start or 0),
                "end": float(word.end or 0),
                "text": token,
            }
            words.append(w)
            seg_words.append(w)
        if on_segment:
            on_segment(item, seg_words)
        if on_progress:
            on_progress(end, duration)
    return {
        "ok": True,
        "language": language,
        "duration": duration,
        "segments": out_segments,
        "words": words,
    }


def run_cli(args) -> int:
    try:
        model, used = pick_model(args.model, args.device, args.compute)
        payload = collect(model, args.wav)
        payload["model"] = args.model
        payload["device"] = used
    except Exception as exc:
        print(json.dumps({"ok": False, "error": str(exc)}), file=sys.stderr)
        return 1
    json.dump(payload, sys.stdout, ensure_ascii=False)
    sys.stdout.write("\n")
    return 0


def run_server(args) -> int:
    model, used = pick_model(args.model, args.device, args.compute)
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, fmt, *rest):
            return

        def _json(self, code, payload):
            body = json.dumps(payload).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path.split("?", 1)[0] == "/health":
                self._json(200, {"ok": True, "device": used, "model": args.model})
                return
            self._json(404, {"ok": False, "error": "not found"})

        def do_POST(self):
            if self.path.split("?", 1)[0] != "/transcribe":
                self._json(404, {"ok": False, "error": "not found"})
                return
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b"{}"
            try:
                req = json.loads(raw.decode("utf-8") or "{}")
            except json.JSONDecodeError:
                self._json(400, {"ok": False, "error": "invalid json"})
                return
            wav = (req.get("wav") or "").strip()
            if not wav or not os.path.isfile(wav):
                self._json(400, {"ok": False, "error": "wav not found"})
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.end_headers()

            def emit(obj):
                self.wfile.write((json.dumps(obj, ensure_ascii=False) + "\n").encode("utf-8"))
                self.wfile.flush()

            try:
                with lock:
                    payload = collect(
                        model,
                        wav,
                        on_meta=lambda language, duration: emit(
                            {"type": "meta", "language": language, "duration": duration, "ok": True}
                        ),
                        on_segment=lambda item, words: emit(
                            {
                                "type": "segment",
                                "ok": True,
                                "id": item.get("id"),
                                "start": item.get("start"),
                                "end": item.get("end"),
                                "text": item.get("text"),
                                "words": words,
                            }
                        ),
                        on_progress=lambda at, duration: emit({"type": "progress", "at": at, "duration": duration}),
                    )
                payload["type"] = "result"
                payload["model"] = args.model
                payload["device"] = used
                emit(payload)
            except Exception as exc:
                emit({"type": "result", "ok": False, "error": str(exc)})

    server = ThreadingHTTPServer(("127.0.0.1", int(args.port or 0)), Handler)
    print(json.dumps({"ok": True, "port": server.server_address[1], "device": used, "model": args.model}), flush=True)
    server.serve_forever()
    return 0


def main() -> int:
    _add_nvidia_lib_path()
    parser = argparse.ArgumentParser()
    parser.add_argument("wav", nargs="?", help="audio file, or 'serve' to keep the model loaded")
    parser.add_argument("--model", default="large-v3-turbo")
    parser.add_argument("--device", default="auto")
    parser.add_argument("--compute", default="int8")
    parser.add_argument("--port", type=int, default=0)
    args = parser.parse_args()
    if args.wav == "serve":
        return run_server(args)
    if not args.wav:
        parser.error("wav path required")
    return run_cli(args)


if __name__ == "__main__":
    raise SystemExit(main())
