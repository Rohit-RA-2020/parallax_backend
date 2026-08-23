#!/usr/bin/env bash
# Bootstrap a GPU VM for the Parallax backend.
#
# Installs host tools, Docker, Qdrant (localhost only), Go, Whisper,
# and a driver-compatible NVENC-capable ffmpeg when the system build lacks it.
# Clones this repo if needed, writes a starter .env, and prints what you still
# have to fill in. Older NVIDIA drivers use the compatible FFmpeg release
# branch; unsupported drivers fall back to CPU encoding without failing setup.
#
# Usage (Ubuntu 22.04 / 24.04):
#   curl -fsSL https://raw.githubusercontent.com/Rohit-RA-2020/parallax_backend/main/scripts/bootstrap-vm.sh | bash
#   ./scripts/bootstrap-vm.sh
#   ./scripts/bootstrap-vm.sh --check
#
# Optional env:
#   PARALLAX_DIR      clone/install path (default: $HOME/parallax_backend)
#   PARALLAX_REPO     git URL (default: origin of this project)
#   PARALLAX_REF      branch or tag to clone (default: main)
#   GO_VERSION        Go version without the "go" prefix (default: latest stable)
#   QDRANT_IMAGE      default qdrant/qdrant:latest
#   SKIP_WHISPER=1    skip the faster-whisper venv (large download)
#   SKIP_QDRANT=1     skip the Qdrant container
#   SKIP_DOCKER=1     assume Docker is already usable
#   SKIP_FFMPEG_NVENC=1
set -euo pipefail

REPO_DEFAULT="https://github.com/Rohit-RA-2020/parallax_backend.git"
QDRANT_IMAGE="${QDRANT_IMAGE:-qdrant/qdrant:latest}"
PARALLAX_REPO="${PARALLAX_REPO:-$REPO_DEFAULT}"
PARALLAX_REF="${PARALLAX_REF:-main}"
PARALLAX_DIR="${PARALLAX_DIR:-$HOME/parallax_backend}"
FFMPEG_OPT_DIR="/opt/ffmpeg-nvenc"

CHECK_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --check) CHECK_ONLY=1 ;;
    -h|--help)
      sed -n '2,28p' "$0"
      exit 0
      ;;
    *)
      echo "Unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a
export PATH="/usr/local/go/bin:${FFMPEG_OPT_DIR}/bin:${PATH}"

if [[ -t 1 ]]; then
  C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'; C_RED=$'\033[31m'
  C_GRN=$'\033[32m'; C_YEL=$'\033[33m'; C_RST=$'\033[0m'
else
  C_BOLD=""; C_DIM=""; C_RED=""; C_GRN=""; C_YEL=""; C_RST=""
fi

log()  { printf '%s==>%s %s\n' "$C_BOLD" "$C_RST" "$*"; }
ok()   { printf '    %sOK%s  %s\n' "$C_GRN" "$C_RST" "$*"; }
warn() { printf '    %s!!%s  %s\n' "$C_YEL" "$C_RST" "$*"; }
fail() { printf '    %sXX%s  %s\n' "$C_RED" "$C_RST" "$*"; }
die()  { fail "$*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

as_root() {
  if [[ ${EUID} -eq 0 ]]; then
    "$@"
  else
    sudo -n "$@" 2>/dev/null || sudo "$@"
  fi
}

require_sudo() {
  if [[ ${EUID} -eq 0 ]]; then
    return
  fi
  if ! sudo -n true 2>/dev/null; then
    log "Need sudo for package installs"
  fi
  sudo -v || die "sudo is required"
}

os_id()      { . /etc/os-release; printf '%s' "${ID:-}"; }
os_codename(){ . /etc/os-release; printf '%s' "${VERSION_CODENAME:-}"; }

detect_repo_dir() {
  local src="${BASH_SOURCE[0]:-}"
  case "$src" in
    ""|/dev/fd/*|/proc/self/fd/*) src="" ;;
  esac
  if [[ -n "$src" && -f "$src" ]]; then
    local here
    here="$(cd "$(dirname "$src")" && pwd)"
    if [[ -f "$here/../cmd/server/main.go" && -f "$here/setup-whisper.sh" ]]; then
      (cd "$here/.." && pwd)
      return
    fi
  fi
  if [[ -f "$PWD/cmd/server/main.go" && -f "$PWD/scripts/setup-whisper.sh" ]]; then
    pwd
    return
  fi
  if [[ -f "$PARALLAX_DIR/cmd/server/main.go" ]]; then
    (cd "$PARALLAX_DIR" && pwd)
    return
  fi
  return 1
}

docker_bin() {
  if have docker && docker info >/dev/null 2>&1; then
    echo docker
    return
  fi
  if [[ ${EUID} -ne 0 ]] && sudo -n docker info >/dev/null 2>&1; then
    echo "sudo docker"
    return
  fi
  if [[ ${EUID} -ne 0 ]] && sudo docker info >/dev/null 2>&1; then
    echo "sudo docker"
    return
  fi
  return 1
}

go_new_enough() {
  have go || return 1
  local current
  current="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  [[ -n "$current" && "$(printf '%s\n%s\n' 1.25.8 "$current" | sort -V | head -1)" == "1.25.8" ]]
}

nvidia_driver_version() {
  have nvidia-smi || return 1
  local raw
  raw="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader,nounits 2>/dev/null)" || return 1
  printf '%s\n' "$raw" | sed -n '1p' | tr -d '[:space:]'
}

nvidia_driver_major() {
  local version="${1:-}"
  version="${version%%.*}"
  [[ "$version" =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "$version"
}

nvidia_driver_uses_server_packages() {
  dpkg -l 2>/dev/null \
    | awk '$1 == "ii" && $2 ~ /^(nvidia-(driver|headless|utils)|libnvidia-encode)-/ {print $2}' \
    | grep -- '-server' >/dev/null
}

nvidia_encode_runtime_present() {
  ldconfig -p 2>/dev/null | grep 'libnvidia-encode.so.1' >/dev/null
}

ensure_nvidia_encode_runtime() {
  local version major package
  version="$(nvidia_driver_version || true)"
  major="$(nvidia_driver_major "$version" || true)"
  [[ -n "$major" ]] || return 0
  nvidia_encode_runtime_present && return 0

  package="libnvidia-encode-${major}"
  if nvidia_driver_uses_server_packages; then
    package+="-server"
  fi
  if ! apt-cache show "$package" >/dev/null 2>&1; then
    warn "NVIDIA driver ${version} is present, but ${package} is unavailable"
    return 0
  fi
  log "Installing NVIDIA NVENC runtime ${package}"
  as_root apt-get install -y --no-install-recommends "$package"
  as_root ldconfig || true
}

ffmpeg_build_for_driver() {
  local driver_major="${1:-}"
  local arch="${2:-}"
  [[ "$driver_major" =~ ^[0-9]+$ ]] || return 1

  local suffix
  case "$arch" in
    x86_64) suffix="linux64" ;;
    aarch64|arm64) suffix="linuxarm64" ;;
    *) return 1 ;;
  esac

  if (( driver_major >= 610 )); then
    printf 'ffmpeg-master-latest-%s-gpl.tar.xz\n' "$suffix"
  elif (( driver_major >= 570 )); then
    printf 'ffmpeg-n8.1-latest-%s-gpl-8.1.tar.xz\n' "$suffix"
  else
    return 1
  fi
}

ffmpeg_has_nvenc() {
  local bin="${1:-ffmpeg}"
  if [[ "$bin" != */* ]]; then
    have "$bin" || return 1
  elif [[ ! -x "$bin" ]]; then
    return 1
  fi
  "$bin" -hide_banner -encoders 2>/dev/null | grep 'h264_nvenc' >/dev/null || return 1
  "$bin" -hide_banner -loglevel error \
    -f lavfi -i color=c=black:s=1280x720:d=1 \
    -frames:v 1 -c:v h264_nvenc -f null - >/dev/null 2>&1
}

upsert_env() {
  local file="$1" key="$2" value="$3"
  python3 - "$file" "$key" "$value" <<'PY'
import sys
from pathlib import Path
path, key, value = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
text = path.read_text() if path.exists() else ""
lines = text.splitlines()
found = False
out = []
for line in lines:
    raw = line.lstrip()
    body = raw[1:].lstrip() if raw.startswith("#") else raw
    if body.startswith(key + "="):
        out.append(f"{key}={value}")
        found = True
    else:
        out.append(line)
if not found:
    if out and out[-1] != "":
        out.append("")
    out.append(f"{key}={value}")
path.write_text("\n".join(out) + "\n")
PY
}

empty_env_keys() {
  local file="$1"
  python3 - "$file" <<'PY'
import sys
from pathlib import Path
path = Path(sys.argv[1])
if not path.exists():
    raise SystemExit(0)
keys = (
    "LLM_GROK_API_KEY",
    "LLM_OPENAI_API_KEY",
    "LLM_API_KEY",
    "XAI_API_KEY",
    "GEMINI_API_KEY",
    "ELEVENLABS_API_KEY",
    "EXA_API_KEY",
    "EMBEDDING_API_KEY",
)
for line in path.read_text().splitlines():
    s = line.strip()
    if not s or s.startswith("#") or "=" not in s:
        continue
    k, _, v = s.partition("=")
    if k in keys and not v.strip().strip('"').strip("'"):
        print(k)
PY
}

# ---------------------------------------------------------------------------
# Checks only
# ---------------------------------------------------------------------------
print_checks() {
  local repo="${1:-}"
  local issues=0
  log "Preflight checks"

  if have nvidia-smi && nvidia-smi >/dev/null 2>&1; then
    ok "NVIDIA driver: $(nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader | head -1)"
  else
    warn "nvidia-smi is not available — GPU acceleration will be disabled"
  fi

  local ff="ffmpeg"
  if [[ -x "${FFMPEG_OPT_DIR}/bin/ffmpeg" ]]; then
    ff="${FFMPEG_OPT_DIR}/bin/ffmpeg"
  elif [[ -x /usr/lib/jellyfin-ffmpeg/ffmpeg ]]; then
    ff=/usr/lib/jellyfin-ffmpeg/ffmpeg
  fi
  if ffmpeg_has_nvenc "$ff"; then
    ok "ffmpeg NVENC via $ff ($($ff -version 2>/dev/null | head -1))"
  elif [[ -x "$ff" ]] || have ffmpeg; then
    warn "ffmpeg NVENC is unavailable in $ff — exports will use CPU"
  else
    fail "ffmpeg is not installed"
    issues=$((issues + 1))
  fi

  if go_new_enough; then
    ok "$(go version)"
  else
    fail "Go 1.25.8+ is required"
    issues=$((issues + 1))
  fi

  if have python3 && python3 -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 9) else 1)'; then
    ok "$(python3 --version)"
  else
    fail "Python 3.9+ is required"
    issues=$((issues + 1))
  fi

  if docker_bin >/dev/null; then
    ok "Docker daemon reachable via $(docker_bin)"
  else
    fail "Docker daemon is not reachable"
    issues=$((issues + 1))
  fi

  if curl -fsS --max-time 3 http://127.0.0.1:6333/readyz >/dev/null 2>&1; then
    ok "Qdrant ready on 127.0.0.1:6333"
  else
    fail "Qdrant is not answering on 127.0.0.1:6333"
    issues=$((issues + 1))
  fi

  if [[ -n "$repo" && -x "$repo/scripts/.venv/bin/python" ]]; then
    if "$repo/scripts/.venv/bin/python" -c 'import faster_whisper' 2>/dev/null; then
      ok "faster-whisper venv is importable"
    else
      fail "Whisper venv exists but faster_whisper failed to import"
      issues=$((issues + 1))
    fi
  else
    warn "Whisper venv not installed yet (scripts/.venv)"
  fi

  if [[ -n "$repo" && -f "$repo/.env" ]]; then
    ok ".env exists at $repo/.env"
    local missing
    missing="$(empty_env_keys "$repo/.env" || true)"
    if [[ -n "$missing" ]]; then
      warn "Empty API keys (fill before Director / embeddings / generation work):"
      while IFS= read -r k; do
        [[ -n "$k" ]] && printf '        %s\n' "$k"
      done <<<"$missing"
    fi
  elif [[ -n "$repo" ]]; then
    warn "No .env yet — copy .env.example and add keys"
  fi

  if have ufw && as_root ufw status 2>/dev/null | grep -qi 'Status: active'; then
    warn "ufw is active. Allow 8080 from your laptop IP only, e.g."
    printf '        sudo ufw allow from YOUR_IP to any port 8080 proto tcp\n'
  fi

  return "$issues"
}

if [[ $CHECK_ONLY -eq 1 ]]; then
  REPO_DIR="$(detect_repo_dir || true)"
  if print_checks "${REPO_DIR:-}"; then
    exit 0
  fi
  exit 1
fi

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------
log "Parallax GPU VM bootstrap"
printf '    %s\n' "repo    ${PARALLAX_REPO} (${PARALLAX_REF})"
printf '    %s\n' "install ${PARALLAX_DIR}"

[[ -f /etc/os-release ]] || die "This script expects Linux with /etc/os-release"
case "$(os_id)" in
  ubuntu|debian) ok "OS $(os_id) $(os_codename)" ;;
  *) die "Only Ubuntu/Debian are supported (found $(os_id))" ;;
esac

require_sudo

log "Installing apt packages"
as_root apt-get update -y
as_root apt-get install -y --no-install-recommends \
  ca-certificates curl git gnupg lsb-release \
  python3 python3-venv python3-dev \
  build-essential pkg-config \
  ffmpeg jq unzip xz-utils \
  apt-transport-https software-properties-common
ok "base packages"

ensure_nvidia_encode_runtime

if ! go_new_enough; then
  log "Installing Go"
  arch="$(uname -m)"
  case "$arch" in
    x86_64) goarch=amd64 ;;
    aarch64|arm64) goarch=arm64 ;;
    *) die "unsupported architecture: $arch" ;;
  esac
  if [[ -z "${GO_VERSION:-}" ]]; then
    GO_VERSION="$(curl -fsSL https://go.dev/VERSION?m=text | head -1 | sed 's/^go//')" || true
  fi
  GO_VERSION="${GO_VERSION:-1.25.8}"
  tmp="$(mktemp -d)"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" -o "$tmp/go.tgz"
  as_root rm -rf /usr/local/go
  as_root tar -C /usr/local -xzf "$tmp/go.tgz"
  rm -rf "$tmp"
  as_root tee /etc/profile.d/parallax-go.sh >/dev/null <<'EOF'
export PATH="/usr/local/go/bin:$PATH"
EOF
  export PATH="/usr/local/go/bin:$PATH"
  go_new_enough || die "Go install did not produce 1.25.8+"
  ok "$(go version)"
else
  ok "$(go version) already present"
fi

if [[ "${SKIP_DOCKER:-0}" != "1" ]]; then
  if have docker && { docker info >/dev/null 2>&1 || as_root docker info >/dev/null 2>&1; }; then
    ok "Docker already installed"
  else
    log "Installing Docker"
    tmp_docker="$(mktemp)"
    curl -fsSL https://get.docker.com -o "$tmp_docker"
    as_root sh "$tmp_docker"
    rm -f "$tmp_docker"
  fi
  as_root systemctl enable --now docker >/dev/null 2>&1 || true
  if [[ ${EUID} -ne 0 ]]; then
    if ! id -nG "$USER" | grep -qw docker; then
      as_root usermod -aG docker "$USER"
      warn "Added $USER to the docker group. New shells will not need sudo; this script will keep using sudo docker."
    fi
  fi
  docker_bin >/dev/null || die "Docker installed but the daemon is not reachable"
  ok "Docker ready"
fi

if [[ "${SKIP_QDRANT:-0}" != "1" ]]; then
  log "Starting Qdrant on 127.0.0.1:6333"
  dcmd="$(docker_bin)" || die "Docker is required for Qdrant"
  if $dcmd inspect qdrant >/dev/null 2>&1; then
    $dcmd start qdrant >/dev/null
    ok "existing qdrant container started"
  else
    $dcmd pull "$QDRANT_IMAGE"
    $dcmd run -d --name qdrant --restart unless-stopped \
      -p 127.0.0.1:6333:6333 \
      -v qdrant_storage:/qdrant/storage \
      "$QDRANT_IMAGE" >/dev/null
    ok "qdrant container created"
  fi
  ready=0
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 2 http://127.0.0.1:6333/readyz >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  [[ $ready -eq 1 ]] || die "Qdrant did not become ready on 127.0.0.1:6333"
  ok "Qdrant is ready"
fi

log "FFmpeg NVENC"
FFMPEG_BIN_VALUE="ffmpeg"
FFPROBE_BIN_VALUE="ffprobe"
if [[ "${SKIP_FFMPEG_NVENC:-0}" != "1" ]]; then
  if ffmpeg_has_nvenc ffmpeg; then
    ok "system ffmpeg already has h264_nvenc"
  elif ffmpeg_has_nvenc /usr/lib/jellyfin-ffmpeg/ffmpeg; then
    FFMPEG_BIN_VALUE=/usr/lib/jellyfin-ffmpeg/ffmpeg
    FFPROBE_BIN_VALUE=/usr/lib/jellyfin-ffmpeg/ffprobe
    ok "using jellyfin-ffmpeg"
  elif [[ -x "${FFMPEG_OPT_DIR}/bin/ffmpeg" ]] && ffmpeg_has_nvenc "${FFMPEG_OPT_DIR}/bin/ffmpeg"; then
    FFMPEG_BIN_VALUE="${FFMPEG_OPT_DIR}/bin/ffmpeg"
    FFPROBE_BIN_VALUE="${FFMPEG_OPT_DIR}/bin/ffprobe"
    ok "using ${FFMPEG_OPT_DIR}"
  else
    log "Selecting a static ffmpeg build compatible with the NVIDIA driver"
    arch="$(uname -m)"
    driver_version="$(nvidia_driver_version || true)"
    driver_major="$(nvidia_driver_major "$driver_version" || true)"
    ffbuild="$(ffmpeg_build_for_driver "$driver_major" "$arch" || true)"
    if [[ -z "$ffbuild" ]]; then
      if [[ -n "$driver_version" ]]; then
        warn "NVIDIA driver ${driver_version} is older than the supported static NVENC builds"
      else
        warn "NVIDIA driver not detected; keeping the system ffmpeg and using CPU fallback"
      fi
    else
      ok "using ${ffbuild} for NVIDIA driver ${driver_version}"
    fi
    if [[ -n "$ffbuild" ]]; then
      tmp="$(mktemp -d)"
      curl -fL "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/${ffbuild}" -o "$tmp/ffmpeg.tar.xz"
      tar -C "$tmp" -xJf "$tmp/ffmpeg.tar.xz"
      extracted="$(find "$tmp" -maxdepth 1 -type d -name 'ffmpeg-*' | head -1)"
      [[ -n "$extracted" ]] || die "failed to unpack ffmpeg"
      as_root rm -rf "$FFMPEG_OPT_DIR"
      as_root mkdir -p /opt
      as_root mv "$extracted" "$FFMPEG_OPT_DIR"
      rm -rf "$tmp"
      FFMPEG_BIN_VALUE="${FFMPEG_OPT_DIR}/bin/ffmpeg"
      FFPROBE_BIN_VALUE="${FFMPEG_OPT_DIR}/bin/ffprobe"
      if ffmpeg_has_nvenc "$FFMPEG_BIN_VALUE"; then
        ok "installed NVENC ffmpeg at $FFMPEG_BIN_VALUE"
      else
        warn "installed $FFMPEG_BIN_VALUE but the NVENC runtime test failed; using CPU fallback"
        FFMPEG_BIN_VALUE="ffmpeg"
        FFPROBE_BIN_VALUE="ffprobe"
      fi
    fi
  fi
fi

log "Fetching Parallax backend"
if REPO_DIR="$(detect_repo_dir)"; then
  ok "using existing checkout $REPO_DIR"
  if [[ -d "$REPO_DIR/.git" ]]; then
    if [[ -z "$(git -C "$REPO_DIR" status --porcelain)" ]]; then
      git -C "$REPO_DIR" fetch --quiet origin || warn "git fetch failed"
      git -C "$REPO_DIR" pull --ff-only --quiet || warn "git pull --ff-only skipped"
    else
      warn "checkout has local changes; not pulling"
    fi
  fi
else
  if [[ -e "$PARALLAX_DIR" && ! -d "$PARALLAX_DIR" ]]; then
    die "$PARALLAX_DIR exists and is not a directory"
  fi
  if [[ -d "$PARALLAX_DIR" && -n "$(ls -A "$PARALLAX_DIR" 2>/dev/null || true)" ]]; then
    die "$PARALLAX_DIR exists but is not a Parallax backend checkout"
  fi
  mkdir -p "$(dirname "$PARALLAX_DIR")"
  git clone --branch "$PARALLAX_REF" --depth 1 "$PARALLAX_REPO" "$PARALLAX_DIR"
  REPO_DIR="$(cd "$PARALLAX_DIR" && pwd)"
  ok "cloned $REPO_DIR"
fi

if [[ "${SKIP_WHISPER:-0}" != "1" ]]; then
  log "Installing faster-whisper (this can take several minutes)"
  PYTHON="${PYTHON:-python3}" "$REPO_DIR/scripts/setup-whisper.sh"
  ok "whisper venv at $REPO_DIR/scripts/.venv"
else
  warn "SKIP_WHISPER=1 — transcription will stay disabled"
fi

log "Preparing .env"
ENV_FILE="$REPO_DIR/.env"
if [[ ! -f "$ENV_FILE" ]]; then
  cp "$REPO_DIR/.env.example" "$ENV_FILE"
  ok "created $ENV_FILE from .env.example"
else
  ok "keeping existing $ENV_FILE"
fi

upsert_env "$ENV_FILE" PARALLAX_ADDR ":8080"
upsert_env "$ENV_FILE" PARALLAX_WORKSPACE "./workspace"
upsert_env "$ENV_FILE" QDRANT_URL "http://127.0.0.1:6333"
upsert_env "$ENV_FILE" WHISPER_PYTHON "./scripts/.venv/bin/python"
upsert_env "$ENV_FILE" WHISPER_SCRIPT "./scripts/transcribe.py"
upsert_env "$ENV_FILE" WHISPER_MODEL "large-v3-turbo"
upsert_env "$ENV_FILE" WHISPER_COMPUTE "int8"
upsert_env "$ENV_FILE" FFMPEG_BIN "$FFMPEG_BIN_VALUE"
upsert_env "$ENV_FILE" FFPROBE_BIN "$FFPROBE_BIN_VALUE"
if have nvidia-smi && nvidia-smi >/dev/null 2>&1 && ffmpeg_has_nvenc "$FFMPEG_BIN_VALUE"; then
  upsert_env "$ENV_FILE" WHISPER_DEVICE "cuda"
  upsert_env "$ENV_FILE" FFMPEG_HWACCEL "cuda"
else
  upsert_env "$ENV_FILE" WHISPER_DEVICE "auto"
  upsert_env "$ENV_FILE" FFMPEG_HWACCEL "auto"
  if have nvidia-smi && nvidia-smi >/dev/null 2>&1; then
    warn "NVENC is unavailable; left Whisper/ffmpeg on auto"
  else
    warn "No working nvidia-smi; left Whisper/ffmpeg on auto"
  fi
fi

log "Building server"
mkdir -p "$REPO_DIR/bin"
( cd "$REPO_DIR" && go build -o bin/parallax ./cmd/server )
ok "built $REPO_DIR/bin/parallax"

chmod +x "$REPO_DIR/scripts/run-server.sh" 2>/dev/null || true

echo
print_checks "$REPO_DIR" || true

echo
log "Next steps"
cat <<EOF

    1. Fill API keys in:
         ${ENV_FILE}

       At minimum set one of LLM_*_API_KEY / LLM_API_KEY so Director can chat.
       GEMINI_API_KEY, ELEVENLABS_API_KEY, EXA_API_KEY, EMBEDDING_* are optional
       but needed for generation, web search, and bin search.

    2. Start the server:
         ${REPO_DIR}/scripts/run-server.sh

       A good start log includes:
         ffmpeg gpu encode enabled  backend=cuda
         parallax listening         addr=:8080

    3. On your laptop, point the frontend at this VM:
         VITE_API_URL=http://<vm-ip>:8080
       or keep localhost and tunnel:
         ssh -L 8080:127.0.0.1:8080 USER@<vm-ip>

    4. Open only port 8080 to your IP. Leave Qdrant on 127.0.0.1.

    Re-run checks later:
         ${REPO_DIR}/scripts/bootstrap-vm.sh --check

EOF
