#!/usr/bin/env bash
# One-command setup + start for the Parallax backend from this checkout.
#
# Usage:
#   ./scripts/setup.sh
#
# Does everything needed to start the server:
#   1. creates .env from .env.example when missing
#   2. generates POSTGRES_PASSWORD / QDRANT_API_KEY / MEDIA_COOKIE_SECRET when unset
#   3. builds the backend image (includes FFmpeg, faster-whisper, headless Blender)
#   4. starts PostgreSQL + Qdrant + backend (DB migrations run automatically)
#   5. waits for the API to become healthy
#
# Afterwards, fill in your LLM / Supabase / embedding keys in .env and re-run
# this script to apply them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

docker_cmd() {
  if docker info >/dev/null 2>&1; then printf 'docker\n'
  elif command -v sudo >/dev/null 2>&1 && sudo docker info >/dev/null 2>&1; then printf 'sudo docker\n'
  else return 1
  fi
}
DCMD="$(docker_cmd)" || die "Docker daemon is not reachable. Start Docker or run: sudo usermod -aG docker \$USER (then log back in)"

[[ -f compose.yaml ]] || die "compose.yaml is missing from $ROOT"

if [[ ! -f .env ]]; then
  log "Creating .env from .env.example"
  cp .env.example .env
fi

random_hex() { od -An -N48 -tx1 /dev/urandom | tr -d ' \n'; }
set_env() {
  local key="$1" value="$2"
  local escaped="${value//\\/\\\\}"
  escaped="${escaped//&/\\&}"
  escaped="${escaped//|/\\|}"
  if grep -q "^${key}=" .env; then sed -i "s|^${key}=.*|${key}=${escaped}|" .env
  else printf '%s=%s\n' "$key" "$value" >> .env
  fi
}

# Secrets: replace only when missing or still a placeholder.
if ! grep -q '^POSTGRES_PASSWORD=[0-9a-f]\{64,\}$' .env; then set_env POSTGRES_PASSWORD "$(random_hex)"; fi
if ! grep -q '^QDRANT_API_KEY=[0-9a-f]\{64,\}$' .env; then set_env QDRANT_API_KEY "$(random_hex)"; fi
if ! grep -q '^MEDIA_COOKIE_SECRET=[0-9a-f]\{64,\}$' .env; then set_env MEDIA_COOKIE_SECRET "$(random_hex)"; fi

COMPOSE_ARGS=(-f compose.yaml)
if [[ -f compose.nvidia.yaml ]] && command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1 \
  && $DCMD info --format '{{json .Runtimes}}' 2>/dev/null | grep -q '"nvidia"'; then
  log "NVIDIA runtime detected; enabling GPU support"
  COMPOSE_ARGS+=(-f compose.nvidia.yaml)
fi

log "Building and starting PostgreSQL, Qdrant, and the backend"
$DCMD compose "${COMPOSE_ARGS[@]}" up -d --build --remove-orphans

PORT="$(sed -n 's/^PARALLAX_PORT=//p' .env | tail -1)"
PORT="${PORT:-8080}"
log "Waiting for the API at http://localhost:${PORT}"
for _ in $(seq 1 60); do
  if curl --fail --silent "http://127.0.0.1:${PORT}/health/ready" >/dev/null 2>&1; then
    printf '\nParallax backend is up: http://localhost:%s\n' "$PORT"
    $DCMD compose "${COMPOSE_ARGS[@]}" ps
    exit 0
  fi
  sleep 5
done

die "Backend did not become healthy in time. Inspect with: $DCMD compose logs backend"
