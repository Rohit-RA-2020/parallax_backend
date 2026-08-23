#!/usr/bin/env bash
# Install Docker when needed, fetch Parallax, build the backend image, and
# provision its private Qdrant dependency.
#
# Usage (Ubuntu/Debian):
#   curl -fsSL https://raw.githubusercontent.com/Rohit-RA-2020/parallax_backend/main/scripts/bootstrap-vm.sh | bash
#
# Optional environment:
#   PARALLAX_DIR=/path/to/checkout
#   PARALLAX_REPO=https://github.com/Rohit-RA-2020/parallax_backend.git
#   PARALLAX_REF=main
#   PARALLAX_IMAGE=parallax-backend:latest
#   QDRANT_IMAGE=qdrant/qdrant:latest
set -euo pipefail

PARALLAX_REPO="${PARALLAX_REPO:-https://github.com/Rohit-RA-2020/parallax_backend.git}"
PARALLAX_REF="${PARALLAX_REF:-main}"
PARALLAX_DIR="${PARALLAX_DIR:-${HOME}/parallax_backend}"
PARALLAX_IMAGE="${PARALLAX_IMAGE:-parallax-backend:latest}"
QDRANT_IMAGE="${QDRANT_IMAGE:-qdrant/qdrant:latest}"
NETWORK_NAME="parallax"
QDRANT_NAME="parallax-qdrant"

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

as_root() {
  if [[ ${EUID} -eq 0 ]]; then
    "$@"
  elif have sudo; then
    sudo "$@"
  else
    die "sudo is required to install Docker"
  fi
}

docker_cmd() {
  if docker info >/dev/null 2>&1; then
    printf 'docker\n'
  elif [[ ${EUID} -ne 0 ]] && have sudo && sudo docker info >/dev/null 2>&1; then
    printf 'sudo docker\n'
  else
    return 1
  fi
}

install_docker() {
  [[ -f /etc/os-release ]] || die "this installer requires a Linux system with /etc/os-release"
  # shellcheck disable=SC1091
  . /etc/os-release
  case "${ID:-}" in
    ubuntu|debian) ;;
    *) die "automatic Docker installation supports Ubuntu and Debian only" ;;
  esac

  log "Installing Docker"
  as_root apt-get update -y
  as_root apt-get install -y --no-install-recommends ca-certificates curl git
  local installer
  installer="$(mktemp)"
  curl -fsSL https://get.docker.com -o "$installer"
  as_root sh "$installer"
  rm -f "$installer"
  as_root systemctl enable --now docker >/dev/null 2>&1 || true
  if [[ ${EUID} -ne 0 ]]; then
    as_root usermod -aG docker "$(id -un)" || true
  fi
}

if ! have git || ! have curl; then
  log "Installing bootstrap tools"
  as_root apt-get update -y
  as_root apt-get install -y --no-install-recommends ca-certificates curl git
fi

if ! have docker || ! docker_cmd >/dev/null; then
  install_docker
fi
DCMD="$(docker_cmd)" || die "Docker is installed but its daemon is not reachable"

log "Fetching Parallax backend"
if [[ -d "$PARALLAX_DIR/.git" ]]; then
  if [[ -n "$(git -C "$PARALLAX_DIR" status --porcelain)" ]]; then
    die "$PARALLAX_DIR has local changes; commit or move them before running the bootstrap again"
  fi
  git -C "$PARALLAX_DIR" fetch --depth 1 origin "$PARALLAX_REF"
  git -C "$PARALLAX_DIR" checkout -q "$PARALLAX_REF"
  git -C "$PARALLAX_DIR" merge --ff-only FETCH_HEAD
elif [[ -e "$PARALLAX_DIR" && -n "$(ls -A "$PARALLAX_DIR" 2>/dev/null || true)" ]]; then
  die "$PARALLAX_DIR already exists and is not an empty Parallax checkout"
else
  mkdir -p "$(dirname "$PARALLAX_DIR")"
  git clone --depth 1 --branch "$PARALLAX_REF" "$PARALLAX_REPO" "$PARALLAX_DIR"
fi

[[ -f "$PARALLAX_DIR/Dockerfile" ]] || die "Dockerfile is missing from $PARALLAX_DIR"
[[ -f "$PARALLAX_DIR/data/elevenlabs-voices.json" ]] || \
  die "bundled voice catalog is missing from $PARALLAX_DIR/data"

log "Building $PARALLAX_IMAGE"
$DCMD build --pull --tag "$PARALLAX_IMAGE" "$PARALLAX_DIR"

log "Provisioning persistent backend services"
$DCMD network inspect "$NETWORK_NAME" >/dev/null 2>&1 || $DCMD network create "$NETWORK_NAME" >/dev/null
$DCMD volume inspect parallax_workspace >/dev/null 2>&1 || $DCMD volume create parallax_workspace >/dev/null
$DCMD volume inspect parallax_data >/dev/null 2>&1 || $DCMD volume create parallax_data >/dev/null
$DCMD volume inspect parallax_qdrant >/dev/null 2>&1 || $DCMD volume create parallax_qdrant >/dev/null

if $DCMD container inspect "$QDRANT_NAME" >/dev/null 2>&1; then
  $DCMD network connect "$NETWORK_NAME" "$QDRANT_NAME" >/dev/null 2>&1 || true
  $DCMD start "$QDRANT_NAME" >/dev/null
else
  $DCMD run -d \
    --name "$QDRANT_NAME" \
    --restart unless-stopped \
    --network "$NETWORK_NAME" \
    --volume parallax_qdrant:/qdrant/storage \
    "$QDRANT_IMAGE" >/dev/null
fi

if [[ ! -f "$PARALLAX_DIR/.env" ]]; then
  cp "$PARALLAX_DIR/.env.example" "$PARALLAX_DIR/.env"
fi

GPU_FLAG=""
if have nvidia-smi && $DCMD info --format '{{json .Runtimes}}' 2>/dev/null | grep -q 'nvidia'; then
  GPU_FLAG=" --gpus all"
fi

cat <<EOF

Parallax backend image is ready.

1. Add your API keys to:
   ${PARALLAX_DIR}/.env

2. Start the backend in detached mode:

   ${DCMD} run -d --name parallax-backend --restart unless-stopped${GPU_FLAG} \\
     --network ${NETWORK_NAME} -p 8080:8080 --env-file '${PARALLAX_DIR}/.env' \\
     -e PARALLAX_WORKSPACE=/app/workspace -e PARALLAX_DATA=/app/data \\
     -e WHISPER_PYTHON=/opt/whisper/bin/python -e WHISPER_SCRIPT=/app/scripts/transcribe.py \\
     -e QDRANT_URL=http://${QDRANT_NAME}:6333 \\
     -v parallax_workspace:/app/workspace -v parallax_data:/app/data \\
     ${PARALLAX_IMAGE}

The API will be available at http://localhost:8080.
EOF
