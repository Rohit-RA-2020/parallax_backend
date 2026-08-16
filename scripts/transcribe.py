#!/usr/bin/env python3
"""faster-whisper sidecar. Prints one JSON object to stdout."""

from __future__ import annotations

import argparse
import glob
import json
import os
import sys


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


def run_model(model, wav):
    return model.transcribe(
        wav,
        language=None,
        word_timestamps=True,
        vad_filter=True,
        beam_size=5,
    )


def transcribe(wav: str, name: str, device: str, compute: str):
    device = (device or "auto").lower()
    compute = compute or "int8"
    if device != "auto":
        return run_model(load_model(name, device, compute), wav), device

    try:
        return run_model(load_model(name, "cuda", compute), wav), "cuda"
    except Exception as exc:
        print(f"cuda unavailable ({exc}); using cpu", file=sys.stderr)
        return run_model(load_model(name, "cpu", "int8"), wav), "cpu"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("wav")
    parser.add_argument("--model", default="large-v3-turbo")
    parser.add_argument("--device", default="auto")
    parser.add_argument("--compute", default="int8")
    args = parser.parse_args()
    _add_nvidia_lib_path()

    try:
        (segments, info), used = transcribe(args.wav, args.model, args.device, args.compute)
    except Exception as exc:
        print(json.dumps({"ok": False, "error": str(exc)}), file=sys.stderr)
        return 1

    out_segments = []
    words = []
    for i, seg in enumerate(segments):
        text = (seg.text or "").strip()
        if not text:
            continue
        out_segments.append(
            {
                "id": f"seg-{i:04d}",
                "start": float(seg.start or 0),
                "end": float(seg.end or 0),
                "text": text,
            }
        )
        for word in seg.words or []:
            token = (word.word or "").strip()
            if not token:
                continue
            words.append(
                {
                    "start": float(word.start or 0),
                    "end": float(word.end or 0),
                    "text": token,
                }
            )

    json.dump(
        {
            "ok": True,
            "language": (info.language or "").lower(),
            "model": args.model,
            "device": used,
            "segments": out_segments,
            "words": words,
        },
        sys.stdout,
        ensure_ascii=False,
    )
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
