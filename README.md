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
    │
    ▼
Any OpenAI-compatible /v1/chat/completions
    (xAI by default; swap base_url + api_key + model)
```

The agent is a plain `for` loop in `internal/agent`. The only LLM dependency is the `llm.ChatProvider` interface. Production uses `llm.CompatClient`, which speaks Chat Completions + SSE + function tools — the dialect almost every hosted model implements. Changing provider is a settings change, not a code change.

FFmpeg is never executed as a shell string. Commands arrive as structured tool arguments (`args: [...]`), get validated (binary, metacharacters, workspace paths), and run with `exec.CommandContext`.

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
| `GET` | `/v1/projects/{id}/media` | List uploaded and generated media |
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
requests. The agent only sees files inside that project's workspace. Director
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

## Tests

All tests live under `tests/`, one package per internal area.

```bash
go test ./...
go test ./tests/...
```
