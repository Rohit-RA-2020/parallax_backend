#!/usr/bin/env bash
# Install Docker, fetch Parallax, and start PostgreSQL, Qdrant, and the backend.
set -euo pipefail

PARALLAX_REPO="${PARALLAX_REPO:-https://github.com/Rohit-RA-2020/parallax_backend.git}"
PARALLAX_REF="${PARALLAX_REF:-main}"
PARALLAX_DIR="${PARALLAX_DIR:-${HOME}/parallax_backend}"
PARALLAX_IMAGE="${PARALLAX_IMAGE:-parallax-backend:latest}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:18.6-bookworm}"
QDRANT_IMAGE="${QDRANT_IMAGE:-qdrant/qdrant:v1.15.4}"
# auto installs the NVIDIA Container Toolkit when a working NVIDIA driver is
# present. Use off to force CPU-only deployment or required to fail when GPU
# support cannot be enabled.
PARALLAX_GPU="${PARALLAX_GPU:-auto}"
NVIDIA_ENCODE_INSTALLED=false

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

as_root() {
  if [[ ${EUID} -eq 0 ]]; then "$@"
  elif have sudo; then sudo "$@"
  else die "sudo is required to install Docker"
  fi
}

docker_cmd() {
  if docker info >/dev/null 2>&1; then printf 'docker\n'
  elif [[ ${EUID} -ne 0 ]] && have sudo && sudo docker info >/dev/null 2>&1; then printf 'sudo docker\n'
  else return 1
  fi
}

install_docker() {
  [[ -f /etc/os-release ]] || die "this installer requires Linux with /etc/os-release"
  # shellcheck disable=SC1091
  . /etc/os-release
  case "${ID:-}" in ubuntu|debian) ;; *) die "automatic Docker installation supports Ubuntu and Debian only" ;; esac
  log "Installing Docker"
  as_root apt-get update -y
  as_root apt-get install -y --no-install-recommends ca-certificates curl git
  local installer
  installer="$(mktemp)"
  curl -fsSL https://get.docker.com -o "$installer"
  as_root sh "$installer"
  rm -f "$installer"
  as_root systemctl enable --now docker >/dev/null 2>&1 || true
  if [[ ${EUID} -ne 0 ]]; then as_root usermod -aG docker "$(id -un)" || true; fi
}

docker_has_nvidia_runtime() {
  $DCMD info --format '{{json .Runtimes}}' 2>/dev/null | grep -q '"nvidia"'
}

nvidia_encode_library_present() {
  # NVENC is a separate host-driver library. nvidia-smi and CUDA compute can
  # work without it, but FFmpeg cannot use h264_nvenc/hevc_nvenc in that case.
  ldconfig -p 2>/dev/null | grep -q 'libnvidia-encode\.so\.1'
}

nvidia_encode_package() {
  # Ubuntu/Debian split the video libraries from nvidia-utils. Derive the
  # matching package name from the installed nvidia-smi owner, e.g.
  # nvidia-utils-580-server -> libnvidia-encode-580-server.
  have dpkg-query || return 1
  local smi owner
  smi="$(readlink -f "$(command -v nvidia-smi)")" || return 1
  owner="$(dpkg-query -S "$smi" 2>/dev/null | head -n1 | cut -d: -f1)"
  if [[ "$owner" =~ ^nvidia-utils-([0-9]+)(-server)?$ ]]; then
    printf 'libnvidia-encode-%s%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]:-}"
    return 0
  fi
  return 1
}

ensure_nvidia_encode_library() {
  if nvidia_encode_library_present; then
    return 0
  fi

  local package
  if ! package="$(nvidia_encode_package)"; then
    log "NVENC library is missing; could not determine its package from nvidia-smi"
    log "CUDA transcription can still work, but FFmpeg GPU encoding will remain disabled"
    return 1
  fi

  log "Installing NVIDIA video driver library ($package)"
  as_root apt-get update
  as_root apt-get install -y --no-install-recommends "$package"
  if ! nvidia_encode_library_present; then
    log "NVENC library is still unavailable after installing $package"
    return 1
  fi
  NVIDIA_ENCODE_INSTALLED=true
  return 0
}

install_nvidia_container_toolkit() {
  log "Installing NVIDIA Container Toolkit"
  as_root apt-get update
  as_root apt-get install -y --no-install-recommends ca-certificates curl gnupg2

  local keyring='/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg'
  local source_list='/etc/apt/sources.list.d/nvidia-container-toolkit.list'
  local key_tmp list_tmp
  key_tmp="$(mktemp)"
  list_tmp="$(mktemp)"
  trap 'rm -f "$key_tmp" "$list_tmp"' RETURN
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor > "$key_tmp"
  curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list |
    sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' > "$list_tmp"
  as_root install -m 0644 "$key_tmp" "$keyring"
  as_root install -m 0644 "$list_tmp" "$source_list"
  rm -f "$key_tmp" "$list_tmp"
  trap - RETURN

  as_root apt-get update
  as_root apt-get install -y nvidia-container-toolkit
}

configure_nvidia_gpu() {
  case "$PARALLAX_GPU" in
    auto|required|off) ;;
    *) die "PARALLAX_GPU must be auto, required, or off" ;;
  esac
  if [[ "$PARALLAX_GPU" == 'off' ]]; then
    log "GPU setup disabled (PARALLAX_GPU=off)"
    return
  fi
  if ! have nvidia-smi || ! nvidia-smi -L >/dev/null 2>&1; then
    if [[ "$PARALLAX_GPU" == 'required' ]]; then
      die "PARALLAX_GPU=required, but no working NVIDIA driver was found"
    fi
    log "No working NVIDIA driver found; using CPU-only media processing"
    return
  fi
  local restart_docker=false
  if ensure_nvidia_encode_library; then
    # Refresh Docker after adding a driver library so new GPU containers see
    # the complete compute and video driver set.
    if [[ "$NVIDIA_ENCODE_INSTALLED" == true ]]; then restart_docker=true; fi
  elif [[ "$PARALLAX_GPU" == 'required' ]]; then
    die "PARALLAX_GPU=required, but the NVIDIA NVENC library could not be enabled"
  fi
  if docker_has_nvidia_runtime; then
    log "Docker NVIDIA runtime is already configured"
  else
    if ! have nvidia-ctk; then
      install_nvidia_container_toolkit
    fi
    log "Configuring Docker for NVIDIA GPU access"
    as_root nvidia-ctk runtime configure --runtime=docker
    restart_docker=true
  fi
  if [[ "$restart_docker" == true ]]; then
    as_root systemctl restart docker
  fi
  if ! docker_has_nvidia_runtime; then
    die "Docker did not register the NVIDIA runtime after configuration"
  fi
}

if ! have git || ! have curl; then
  as_root apt-get update -y
  as_root apt-get install -y --no-install-recommends ca-certificates curl git
fi
if ! have docker || ! docker_cmd >/dev/null; then install_docker; fi
DCMD="$(docker_cmd)" || die "Docker is installed but its daemon is not reachable"
configure_nvidia_gpu

log "Fetching Parallax backend"
if [[ -d "$PARALLAX_DIR/.git" ]]; then
  if [[ -n "$(git -C "$PARALLAX_DIR" status --porcelain)" ]]; then die "$PARALLAX_DIR has local changes; commit or move them before updating"; fi
  git -C "$PARALLAX_DIR" fetch --depth 1 origin "$PARALLAX_REF"
  git -C "$PARALLAX_DIR" checkout -q "$PARALLAX_REF"
  git -C "$PARALLAX_DIR" merge --ff-only FETCH_HEAD
elif [[ -e "$PARALLAX_DIR" && -n "$(ls -A "$PARALLAX_DIR" 2>/dev/null || true)" ]]; then
  die "$PARALLAX_DIR already exists and is not an empty Parallax checkout"
else
  mkdir -p "$(dirname "$PARALLAX_DIR")"
  git clone --depth 1 --branch "$PARALLAX_REF" "$PARALLAX_REPO" "$PARALLAX_DIR"
fi

[[ -f "$PARALLAX_DIR/compose.yaml" ]] || die "compose.yaml is missing from $PARALLAX_DIR"
if [[ ! -f "$PARALLAX_DIR/.env" ]]; then cp "$PARALLAX_DIR/.env.example" "$PARALLAX_DIR/.env"; fi

random_hex() { od -An -N48 -tx1 /dev/urandom | tr -d ' \n'; }
set_env() {
  local key="$1" value="$2"
  local escaped="${value//\\/\\\\}"
  escaped="${escaped//&/\\&}"
  escaped="${escaped//|/\\|}"
  if grep -q "^${key}=" "$PARALLAX_DIR/.env"; then sed -i "s|^${key}=.*|${key}=${escaped}|" "$PARALLAX_DIR/.env"
  else printf '%s=%s\n' "$key" "$value" >> "$PARALLAX_DIR/.env"
  fi
}

if ! grep -q '^POSTGRES_PASSWORD=[0-9a-f]\{64,\}$' "$PARALLAX_DIR/.env"; then set_env POSTGRES_PASSWORD "$(random_hex)"; fi
if ! grep -q '^QDRANT_API_KEY=[0-9a-f]\{64,\}$' "$PARALLAX_DIR/.env"; then set_env QDRANT_API_KEY "$(random_hex)"; fi
if ! grep -q '^MEDIA_COOKIE_SECRET=[0-9a-f]\{64,\}$' "$PARALLAX_DIR/.env"; then set_env MEDIA_COOKIE_SECRET "$(random_hex)"; fi
set_env POSTGRES_IMAGE "$POSTGRES_IMAGE"
set_env QDRANT_IMAGE "$QDRANT_IMAGE"
set_env PARALLAX_IMAGE "$PARALLAX_IMAGE"
for key in SUPABASE_URL SUPABASE_JWKS_URL SUPABASE_ISSUER SUPABASE_AUDIENCE PARALLAX_ALLOWED_ORIGINS POSTGRES_PORT QDRANT_HTTP_PORT QDRANT_GRPC_PORT PARALLAX_PORT; do
  if [[ -n "${!key:-}" ]]; then set_env "$key" "${!key}"; fi
done

COMPOSE_ARGS=(-f compose.yaml)
if have nvidia-smi && nvidia-smi -L >/dev/null 2>&1 && docker_has_nvidia_runtime; then
  printf '%s\n' \
    'services:' \
    '  backend:' \
    '    deploy:' \
    '      resources:' \
    '        reservations:' \
    '          devices:' \
    '            - driver: nvidia' \
    '              count: all' \
    '              capabilities: [gpu]' > "$PARALLAX_DIR/compose.gpu.yaml"
  COMPOSE_ARGS+=(-f compose.gpu.yaml)
fi

log "Starting PostgreSQL and Qdrant"
(cd "$PARALLAX_DIR" && $DCMD compose "${COMPOSE_ARGS[@]}" up -d postgres qdrant --remove-orphans)

required_ready=true
for key in SUPABASE_URL; do
  value="$(sed -n "s/^${key}=//p" "$PARALLAX_DIR/.env" | tail -1)"
  if [[ -z "$value" || "$value" == *YOUR_* ]]; then required_ready=false; fi
done
if $required_ready; then
  log "Building and starting the Parallax backend"
  (cd "$PARALLAX_DIR" && $DCMD compose "${COMPOSE_ARGS[@]}" up -d --build backend)
else
  log "Backend credentials are incomplete; PostgreSQL and Qdrant are ready"
fi

cat <<EOF

PostgreSQL and Qdrant are running.

Add the required Supabase, LLM, and embedding credentials to:
  ${PARALLAX_DIR}/.env

Then apply changes with:
  cd '${PARALLAX_DIR}' && ${DCMD} compose ${COMPOSE_ARGS[*]} up -d --build

When credentials are complete, the API will be available at http://localhost:8080.
PostgreSQL listens on 0.0.0.0:${POSTGRES_PORT:-5432}; Qdrant REST listens on
0.0.0.0:${QDRANT_HTTP_PORT:-6333} and gRPC on 0.0.0.0:${QDRANT_GRPC_PORT:-6334}.
Both require the randomly generated credentials stored in ${PARALLAX_DIR}/.env.
Restrict these ports to trusted source IPs with the VM firewall; neither service
should be exposed unrestricted to the public Internet.

No automated backup is configured. Protect both the parallax_postgres and
parallax_media volumes with VM snapshots or an external backup process before
storing production data.
EOF
