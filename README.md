# Parallax backend

Go service for the Parallax media agent. A user describes a video/audio/image task; a framework-free agent loop streams a plan, calls tools, runs **ffmpeg/ffprobe** in a workspace sandbox, and keeps going until the job is done.

The React frontend is wired to this service for projects, uploads, project media,
and streamed Director sessions.

## Design

```
User message
    │
    ▼
HTTP  POST /v1/agent/chat   (SSE)
    │
    ▼
Agent loop  (no framework)
    observe → think (stream tokens) → act (tools) → observe …
    │
    ├── list_workspace / inspect_file / probe_media
    └── run_ffmpeg  →  argv parse → sandbox validate → exec.Command (no shell)
    └── search_web   →  Exa Search API (links + highlights/full page text)
    └── generate_image →  Gemini image generation (still lands in the project bin)
    └── search_transcript / get_transcript / add_captions  →  Whisper index + timed captions
    │
    ▼
Any OpenAI-compatible /v1/chat/completions
    (xAI by default; swap base_url + api_key + model)
```

The agent is a plain `for` loop in `internal/agent`. The only LLM dependency is the `llm.ChatProvider` interface. Production uses `llm.CompatClient`, which speaks Chat Completions + SSE + function tools — the dialect almost every hosted model implements. Changing provider is a settings change, not a code change.

FFmpeg is never executed as a shell string. Commands arrive as structured tool arguments (`args: [...]`), get validated (binary, metacharacters, workspace paths), and run with `exec.CommandContext`.

At startup the server probes ffmpeg plus the host GPUs and, when a backend
actually encodes a test frame, rewrites software video codecs (`libx264`,
`libx265`, `libvpx-vp9`, SVT/AOM AV1) to that encoder for exports, caption
burns, and `run_ffmpeg`. NVIDIA NVENC is preferred when present; then
VideoToolbox, Intel QSV, then VAAPI. Filter-heavy graphs stay in system
memory and only the encode steps onto the GPU (NVENC accepts those frames
directly). NVIDIA preview proxies use NVDEC, `scale_cuda`, and NVENC so video
frames stay on the GPU; unsupported sources fall back to GPU encode-only and
then CPU. A failed GPU encode retries on CPU. Set `FFMPEG_HWACCEL=off` to
disable, or pin `cuda` / `vaapi` / `qsv` / `videotoolbox`. `FFMPEG_HWDEVICE`
selects a GPU index or `/dev/dri/renderD*` node.

## Configure the LLM

Models are declared in `.env`. The editor can only select among those entries.

```bash
LLM_MODELS=grok,openai

LLM_GROK_LABEL=Grok
LLM_GROK_BASE_URL=https://api.x.ai/v1
LLM_GROK_MODEL=grok-4.6
LLM_GROK_API_KEY=xai-…

LLM_OPENAI_LABEL=OpenAI
LLM_OPENAI_BASE_URL=https://api.openai.com/v1
LLM_OPENAI_MODEL=gpt-4.1
LLM_OPENAI_API_KEY=sk-…
```

To enable Director web search, set `EXA_API_KEY` in the backend environment.
The server keeps the key private and exposes a `search_web` function to
Director. It uses Exa's `/search` endpoint, defaults to compact highlights,
and supports full page text with `content_mode: "text"`. `EXA_BASE_URL` is
optional and defaults to `https://api.exa.ai`.

To let Director generate stills into the project bin, set `GEMINI_API_KEY`
(or `GOOGLE_API_KEY`). The server keeps the key private and exposes a
`generate_image` function. It calls Gemini's Interactions image API
(`gemini-3.1-flash-image` by default). A text prompt creates a new still.
Passing `source` (or `images`) sends an existing uploaded or generated
file with the edit instructions — Gemini's image-to-image path — and
replaces that bin item in place so the timeline updates. Use
`apply_to: "none"` to keep a separate variant. Optional overrides:

| Field | Env | Default |
|-------|-----|---------|
| API key | `GEMINI_API_KEY` (or `GOOGLE_API_KEY`) | _(empty)_ |
| Base URL | `GEMINI_API_BASE` | `https://generativelanguage.googleapis.com/v1beta` |
| Model | `GEMINI_IMAGE_MODEL` | `gemini-3.1-flash-image` |

To let Director generate voiceovers and sound effects, set `ELEVENLABS_API_KEY`.
Music generation uses Gemini Lyria 3, so set `GEMINI_API_KEY` as well. The
integrations are implemented in Go and use the provider REST APIs directly.

```bash
GEMINI_API_KEY=your-gemini-key
GEMINI_MUSIC_MODEL=lyria-3-pro-preview
GEMINI_MUSIC_OUTPUT_FORMAT=mp3
ELEVENLABS_API_KEY=xi-…
ELEVENLABS_BASE_URL=https://api.elevenlabs.io
ELEVENLABS_TTS_MODEL=eleven_v3
ELEVENLABS_SFX_MODEL=eleven_text_to_sound_v2
ELEVENLABS_TTS_VOICES_FILE=./data/elevenlabs-voices.json
```

The voice catalog is a JSON array containing `id`, `name`, `description`,
`languages`, and `characteristics`. Director calls `list_tts_voices` before
`generate_voiceover`, then can generate-only or place the resulting audio on
`A1`/`A2` in the same timeline revision. Generated voiceovers use ElevenLabs'
character timing response for transcript search; music and sound effects are
indexed from their prompts, lyrics, and generation metadata.

Gemini music prompts should specify the genre, instruments, BPM, key, mood,
structure, and intended duration. Use `lyria-3-clip-preview` for a fixed
30-second clip and `lyria-3-pro-preview` for a longer song. Use `[Intro]`,
`[Verse]`, `[Chorus]`, `[Bridge]`, and `[Outro]` tags or timestamps when the
arrangement matters; add `instrumental only, no vocals` when the music sits
under dialogue.

If `LLM_MODELS` is unset, the original single-model vars still work:

| Field      | Env            | Default                 |
|------------|----------------|-------------------------|
| `base_url` | `LLM_BASE_URL` | `https://api.x.ai/v1`   |
| `api_key`  | `LLM_API_KEY` (or `XAI_API_KEY`) | _(empty)_ |
| `model`    | `LLM_MODEL`    | `grok-4.6`              |

`GET /v1/settings` lists the env-defined models (keys are never returned).
`PUT /v1/settings` with `{"active_id":"openai"}` switches the active one.
`POST /v1/agent/chat` accepts optional `profile_id` for that turn.
It also accepts optional `thinking_effort` (`low`, `medium`, or `high`;
defaults to `medium`) and forwards it as the provider's `reasoning_effort`.

Examples of other providers (same three fields):

- OpenAI: `https://api.openai.com/v1` + `gpt-4.1`
- Gemini: `https://generativelanguage.googleapis.com/v1beta/openai` + a Gemini model
- Groq: `https://api.groq.com/openai/v1`
- OpenRouter: `https://openrouter.ai/api/v1`
- Ollama: `http://127.0.0.1:11434/v1`

Gemini thinking-model tool calls include
`extra_content.google.thought_signature`. Parallax preserves that field and
returns it unchanged during sequential and parallel tool-calling steps, as
required by Gemini's OpenAI-compatible API.

## Transcripts and search

On upload, files with audio are transcribed by a long-lived **faster-whisper**
worker (`large-v3-turbo`, CUDA int8 when a GPU is available). One file uses the
GPU at a time. Word-level original language is stored at
`.parallax/transcripts/<sha256>.json`. Unchanged soundtracks reuse that
transcript. A Qdrant outage keeps the transcript and marks search indexing as
failed instead of throwing the whole job away.

Non-English segments are translated to English by the **active chat LLM**.
English segment text (plus neighboring segments) is embedded through a
**separate** OpenAI-compatible embeddings endpoint and upserted into **local
Qdrant**, one collection per project. Stills take the same path: a vision
caption is written on upload, generate, or in-place edit, then embedded as
`kind: "image"` points so they do not mix with speech. Stills caption in a
small Go worker pool (default 6) so a multi-file upload does not wait in a
single line; speech and video stay serial because of the GPU. Videos are split on
visual cuts (with interval samples inside long takes); each shot is captioned
and stored as `kind: "video_scene"` with start/end times. Overlapping English
transcript text is attached when speech exists. Director must query in
English and may filter by file path.

```bash
# from parallax_backend/
./scripts/setup-whisper.sh

WHISPER_PYTHON=./scripts/.venv/bin/python
WHISPER_MODEL=large-v3-turbo
WHISPER_DEVICE=auto
WHISPER_COMPUTE=int8

EMBEDDING_BASE_URL=https://api.openai.com/v1
EMBEDDING_API_KEY=sk-…
EMBEDDING_MODEL=text-embedding-3-small

QDRANT_URL=http://127.0.0.1:6333
```

## Run

```bash
cd parallax_backend
cp .env.example .env   # then put a key in LLM_API_KEY or XAI_API_KEY
go run ./cmd/server
```

Create projects from the frontend and upload media there. Each project gets an
isolated directory under `./workspace/projects/<project-id>`; Director tools are
scoped to that directory for the whole session.

## Project and media API

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/v1/projects` | List persistent projects |
| `POST` | `/v1/projects` | Create a project with `{"name":"…"}` |
| `GET` | `/v1/projects/{id}` | Get a project and its media |
| `DELETE` | `/v1/projects/{id}` | Permanently delete a project, its media, chats, transcripts, and embeddings |
| `GET` | `/v1/projects/{id}/media` | List uploaded and generated media |
| `GET` | `/v1/projects/{id}/media/search?q=` | Semantic search over stills, video shots, and speech |
| `POST` | `/v1/projects/{id}/media` | Upload one or more multipart `files` |
| `POST` | `/v1/projects/{id}/export` | Render a downloadable file (`mp4`, `mov`, `webm`, `gif`, `mp3`) |
| `GET` | `/v1/projects/{id}/files/{path...}` | Stream a project file with range support |
| `DELETE` | `/v1/projects/{id}/files/{path...}` | Remove a media file from the project |
| `GET` | `/v1/projects/{id}/chats` | List persisted Director chats |
| `POST` | `/v1/projects/{id}/chats` | Start a new chat |
| `GET` | `/v1/projects/{id}/chats/{chatId}` | Load a chat and its messages |
| `PATCH` | `/v1/projects/{id}/chats/{chatId}` | Rename a chat |
| `DELETE` | `/v1/projects/{id}/chats/{chatId}` | Delete a chat |
| `GET` | `/v1/projects/{id}/timeline` | Load the persisted sequence (clips, in-points, playhead) |
| `PUT` | `/v1/projects/{id}/timeline` | Atomically save the sequence as a frame-accurate document |
| `GET` | `/v1/projects/{id}/history` | List immutable revisions, branches, and checkpoints |
| `POST` | `/v1/projects/{id}/history/undo` | Move to the parent revision |
| `POST` | `/v1/projects/{id}/history/redo` | Move to a redo candidate |
| `POST` | `/v1/projects/{id}/history/restore` | Restore any revision without deleting alternate futures |
| `POST` | `/v1/projects/{id}/checkpoints` | Name the current or selected revision |

Include `project_id` and `session_id` (the chat id) in `/v1/agent/chat`
requests. Optional `images` is an array of `{name, mime, data}` stills
(base64 or data URLs). They are saved under `.parallax/chat-media/`, shown
in the chat, and sent to the model as Chat Completions `image_url` parts so
vision-capable models can see them. The agent only sees workspace files
through tools; chat attachments are vision context, not bin items. Director
timeline and media changes are staged for the request and commit as one
revision. Timeline-representable edits remain non-destructive. FFmpeg
fallbacks keep one logical bin item while content-addressed objects preserve
the previous bytes for undo. Pass `apply_to: "none"` only when the user wants
a separate generated asset or export.

## Chat (SSE)

```bash
curl -N localhost:8080/v1/agent/chat \
  -H 'content-type: application/json' \
  -d '{"project_id":"PROJECT_ID","message":"strip audio from media/talk.mp4 and write talk_muted.mp4"}'
```

Events:

| Event         | Payload |
|---------------|---------|
| `session`     | `{session_id}` |
| `step`        | `{iteration, phase: think\|act}` |
| `text`        | `{delta}` streamed tokens |
| `tool_call`   | `{id, name, arguments}` |
| `tool_result` | `{id, name, ok, output, error}` |
| `done`        | `{reason, iterations}` |
| `error`       | `{message}` |

Pass `session_id` on the next request to continue the same conversation. Project
chats are written under `.parallax/chats/` and survive server restarts. The
sequence is stored as `.parallax/timeline.json` with integer frame times at the
project fps, source in-points, and media paths (not playback URLs).

## Tools

| Tool             | Purpose |
|------------------|---------|
| `list_workspace` | Inventory media in the sandbox |
| `inspect_file`   | Size / mtime |
| `probe_media`    | `ffprobe` JSON |
| `run_ffmpeg`     | One validated ffmpeg/ffprobe command |
| `get_timeline` | Inspect stable timeline IDs and editable properties |
| `place_media` | Put a file on the timeline (V1 picture + linked A1 audio) |
| `edit_timeline` | Stage validated effects, keyframes, cuts, and transitions |
| `get_project_history` | Inspect revisions, alternate futures, and checkpoints |
| `undo_project_change` / `redo_project_change` | Stage persistent history navigation |
| `restore_project_revision` | Restore a selected revision |
| `create_project_checkpoint` | Name the state committed by the current request |
| `search_web` | Search the web through Exa for links, metadata, and page content |
| `generate_image` | Generate a still with Gemini, or edit an existing uploaded/generated still by sending it back with a prompt |
| `search_images` | English semantic search over this project's described stills (optional path filter) |
| `get_image_caption` | Read the stored English description for one still |
| `search_scenes` | English semantic search over this project's video shots (optional path filter) |
| `get_video_scenes` | Read stored shot times and descriptions for one video |
| `search_transcript` | English semantic search over this project's speech (optional path filter) |
| `get_transcript` | Read the timed original + English transcript for one file |
| `add_captions` | Place a visible C1 caption track from the stored transcript (or burn into the picture) |

## Tests

All tests live under `tests/`, one package per internal area.

```bash
go test ./...
go test ./tests/...
```
