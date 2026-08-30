# Production deployment

Parallax uses managed Supabase Auth only. PostgreSQL is the source of truth for
users, projects, revisions, chats, jobs, and analysis metadata; the VM's
persistent `parallax_media` volume is the source of truth for durable media;
Qdrant contains rebuildable embeddings. The separate Docker temporary volume
contains only resumable upload chunks and FFmpeg workspaces.

## 1. Supabase Auth

Create a Supabase project and enable email/password sign-in. Require email
confirmation. Under Auth URL configuration, add the production application URL
and the reset callback used by the frontend; for local development add
`http://localhost:5173`. Customize the confirmation and password recovery email
templates as needed.

Use an asymmetric JWT signing key (RS256, ES256, or EdDSA). Parallax validates
tokens locally from `/auth/v1/.well-known/jwks.json` and requires issuer,
`aud=authenticated`, expiry, subject, and verified email claims. Do not provide a
Supabase service-role key to Parallax.

Set `SUPABASE_URL`, `SUPABASE_AUDIENCE`, `PARALLAX_ALLOWED_ORIGINS`, and a random
`MEDIA_COOKIE_SECRET` of at least 32 characters. In production set
`MEDIA_COOKIE_SECURE=true`. Build the frontend with `VITE_SUPABASE_URL` and
`VITE_SUPABASE_PUBLISHABLE_KEY`. Route `/v1` from the public frontend origin to the
backend so its HttpOnly media cookie works for native video, audio, and image
elements.

## 2. Durable VM media storage

Compose creates the persistent `parallax_media` Docker volume and mounts it at
`/app/media`. Objects are immutable and stored below
`projects/{project_id}/objects/{sha256-prefix}/{sha256}`; filenames and logical
paths exist only in PostgreSQL. For a non-Compose installation, set
`PARALLAX_MEDIA_STORAGE` to a dedicated persistent directory owned by the
backend user. Never point this setting at the temporary FFmpeg workspace.

## 3. VM bootstrap

On a Debian or Ubuntu VM:

```bash
PARALLAX_REPO=https://github.com/Rohit-RA-2020/parallax_backend.git \
  ./scripts/bootstrap-vm.sh
```

The script installs Docker when necessary, generates the PostgreSQL password,
Qdrant API key, and media-cookie secret without printing them, and starts
PostgreSQL 18.6 and Qdrant 1.15.4. It starts the backend once required Supabase
settings are present in `.env`.

Compose publishes the backend, PostgreSQL, and Qdrant on every host interface:

- Backend HTTP: `0.0.0.0:${PARALLAX_PORT:-8080}`
- PostgreSQL: `0.0.0.0:${POSTGRES_PORT:-5432}`
- Qdrant REST: `0.0.0.0:${QDRANT_HTTP_PORT:-6333}`
- Qdrant gRPC: `0.0.0.0:${QDRANT_GRPC_PORT:-6334}`

PostgreSQL requires its SCRAM-protected `parallax` password and Qdrant requires
the `QDRANT_API_KEY` header. The backend continues to use the private Compose
network names. Publishing a port is not a firewall policy: allow these database
ports only from trusted administration or application IPs. Do not expose them
unrestricted to the public Internet. PostgreSQL is not configured with TLS by
this Compose file, so remote access should traverse a private network, VPN, or
SSH tunnel.

After editing `.env`, apply it with `docker compose up -d --build backend`.
Check liveness at `/health/live` and dependency readiness at `/health/ready`.
Readiness fails when PostgreSQL, the persistent media directory, or Qdrant is
unavailable; liveness remains available while the process is running.

### NVIDIA GPU encoding

`FFMPEG_HWACCEL=auto` probes only GPUs already visible inside the backend
container; it does not make a host GPU available. The bootstrap script installs
and configures the NVIDIA Container Toolkit automatically when `nvidia-smi`
works on the host. For a manual deployment, install the toolkit, then start the
optional override:

```bash
docker compose -f compose.yaml -f compose.nvidia.yaml up -d --build backend
```

At startup the backend logs `ffmpeg gpu encode enabled` only after FFmpeg can
complete a hardware H.264 test encode. If it remains disabled, inspect the
container with `sudo docker exec <backend-container> nvidia-smi -L` and verify
that `ffmpeg -encoders` lists `h264_nvenc`. The backend requests
`compute,utility,video` NVIDIA driver capabilities; `video` is required for the
NVENC/NVDEC Video Codec SDK libraries.

The backend runs embedded numbered Goose migrations under a PostgreSQL advisory
lock before serving. Upload and FFmpeg bytes use `parallax_tmp`; they are not
authoritative and may be discarded after jobs finish or expire.
`PARALLAX_TMP_MAX_BYTES` bounds aggregate active upload reservations (100 GiB by
default); also size and monitor the VM filesystem for FFmpeg workspace headroom.

## Upgrades

Set `POSTGRES_IMAGE` and `QDRANT_IMAGE` in `.env` to reviewed pinned versions.
For a PostgreSQL minor update, take a VM snapshot, pull the image, and recreate
the service. A PostgreSQL major update requires the vendor-supported
`pg_upgrade` or dump/restore process—never point a new major image at an old data
directory. For Qdrant, follow its snapshot and compatibility instructions before
changing versions. Change `QDRANT_COLLECTION` when the embedding model or vector
dimension changes and rebuild embeddings from PostgreSQL analysis rows.

## Failure and recovery limits

Project deletion is immediate from the user’s perspective. A durable purge job
retries removal of Qdrant points and the project's durable media directory
before cascading the PostgreSQL rows. Media files are atomically renamed into
place before metadata becomes ready, and content-addressing makes retries safe.

There is intentionally no automated backup. Both `parallax_postgres` and
`parallax_media` are single points of data loss on the VM. Protect both volumes
with VM snapshots or external backups before production use. Media files alone
cannot reconstruct project timelines, ownership, chat history, or logical asset
names. Qdrant can be rebuilt from PostgreSQL, but PostgreSQL cannot be rebuilt
from Qdrant.
