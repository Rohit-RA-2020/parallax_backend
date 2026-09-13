# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/parallax ./cmd/server

# Blender LTS for headless Director renders (matches host 5.2.x).
# Renders run headless in-container; the live MCP bridge addon is NOT vendored
# here (see scripts/setup-blender-mcp.sh on the host). Mount the addon at
# /opt/blender-mcp-addon when running the headless bridge inside containers.
FROM debian:bookworm-slim AS blender
ARG BLENDER_VERSION=5.2.1
ARG BLENDER_SHA256=""
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
        libgl1 libglx-mesa0 libgl1-mesa-dri libxi6 libxrender1 libxfixes1 \
        libxxf86vm1 libxkbcommon0 libsm6 libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL -o /tmp/blender.tar.xz \
        https://download.blender.org/release/Blender5.2/blender-${BLENDER_VERSION}-linux-x64.tar.xz \
    && curl -fsSL -o /tmp/blender.tar.xz.sha256 \
        https://download.blender.org/release/Blender5.2/blender-${BLENDER_VERSION}-linux-x64.tar.xz.sha256 || true \
    && if [ -n "${BLENDER_SHA256}" ]; then echo "${BLENDER_SHA256}  /tmp/blender.tar.xz" | sha256sum -c -; \
       elif [ -s /tmp/blender.tar.xz.sha256 ]; then (cd /tmp && sha256sum -c blender.tar.xz.sha256); \
       else echo "WARNING: no blender sha256 available; skipping checksum verify" >&2; fi \
    && mkdir -p /opt/blender \
    && tar -xJf /tmp/blender.tar.xz -C /opt/blender --strip-components=1 \
    && rm /tmp/blender.tar.xz /tmp/blender.tar.xz.sha256 \
    && /opt/blender/blender --background --python-expr "import bpy; print('blender', bpy.app.version_string)"

FROM python:3.11-slim-bookworm AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    PATH=/opt/blender:/opt/whisper/bin:$PATH \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PARALLAX_ADDR=:8080 \
    PARALLAX_WORKSPACE=/app/workspace \
    PARALLAX_DATA=/app/data \
    BLENDER_BIN=/opt/blender/blender \
    WHISPER_PYTHON=/opt/whisper/bin/python \
    WHISPER_SCRIPT=/app/scripts/transcribe.py \
    ELEVENLABS_TTS_VOICES_FILE=/app/assets/elevenlabs-voices.json

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl ffmpeg fontconfig fonts-dejavu-core fonts-noto-core \
        libgl1 libglx-mesa0 libgl1-mesa-dri libxi6 libxrender1 libxfixes1 \
        libxxf86vm1 libxkbcommon0 libsm6 libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/* \
    && python -m venv /opt/whisper \
    && /opt/whisper/bin/pip install --no-cache-dir --upgrade pip \
    && /opt/whisper/bin/pip install --no-cache-dir "faster-whisper>=1.1.0" "yt-dlp==2026.8.19" \
    && if [ "$(dpkg --print-architecture)" = "amd64" ]; then \
         /opt/whisper/bin/pip install --no-cache-dir \
           nvidia-cublas-cu12 nvidia-cuda-runtime-cu12 nvidia-cudnn-cu12; \
       fi

WORKDIR /app
COPY --from=builder /out/parallax /usr/local/bin/parallax
COPY --from=blender /opt/blender /opt/blender
COPY scripts/transcribe.py ./scripts/transcribe.py
COPY scripts/blender-headless-bridge.py ./scripts/blender-headless-bridge.py
COPY data/elevenlabs-voices.json ./assets/elevenlabs-voices.json
COPY provider-icons ./provider-icons

RUN groupadd --gid 10001 parallax \
    && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/parallax parallax \
    && mkdir -p /app/workspace /app/data \
    && chown -R parallax:parallax /app /home/parallax

USER parallax
EXPOSE 8080
VOLUME ["/app/workspace", "/app/data", "/app/media"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl --fail --silent http://127.0.0.1:8080/health/ready >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/parallax"]
