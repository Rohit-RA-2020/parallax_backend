# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/parallax ./cmd/server

FROM python:3.11-slim-bookworm AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    PATH=/opt/whisper/bin:$PATH \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PARALLAX_ADDR=:8080 \
    PARALLAX_WORKSPACE=/app/workspace \
    PARALLAX_DATA=/app/data \
    WHISPER_PYTHON=/opt/whisper/bin/python \
    WHISPER_SCRIPT=/app/scripts/transcribe.py \
    ELEVENLABS_TTS_VOICES_FILE=/app/assets/elevenlabs-voices.json

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl ffmpeg fontconfig fonts-dejavu-core fonts-noto-core \
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
COPY scripts/transcribe.py ./scripts/transcribe.py
COPY data/elevenlabs-voices.json ./assets/elevenlabs-voices.json

RUN groupadd --gid 10001 parallax \
    && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/parallax parallax \
    && mkdir -p /app/workspace /app/data \
    && chown -R parallax:parallax /app /home/parallax

USER parallax
EXPOSE 8080
VOLUME ["/app/workspace", "/app/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl --fail --silent http://127.0.0.1:8080/v1/settings >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/parallax"]
