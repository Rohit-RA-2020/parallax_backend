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
    │
    ▼
Any OpenAI-compatible /v1/chat/completions
    (xAI by default; swap base_url + api_key + model)
```

The agent is a plain `for` loop in `internal/agent`. The only LLM dependency is the `llm.ChatProvider` interface. Production uses `llm.CompatClient`, which speaks Chat Completions + SSE + function tools — the dialect almost every hosted model implements. Changing provider is a settings change, not a code change.

FFmpeg is never executed as a shell string. Commands arrive as structured tool arguments (`args: [...]`), get validated (binary, metacharacters, workspace paths), and run with `exec.CommandContext`.

## Configure the LLM

Three fields. That is the whole provider contract.

| Field      | Env            | Default                 |
|------------|----------------|-------------------------|
| `base_url` | `LLM_BASE_URL` | `https://api.x.ai/v1`   |
| `api_key`  | `LLM_API_KEY` (or `XAI_API_KEY`) | _(empty)_ |
| `model`    | `LLM_MODEL`    | `grok-4.6`              |

Copy `.env.example` to `.env` or set the variables. Settings can also be changed at runtime:

```bash
curl -X PUT localhost:8080/v1/settings \
  -H 'content-type: application/json' \
  -d '{"base_url":"https://api.x.ai/v1","api_key":"xai-…","model":"grok-4.6"}'
```

GET `/v1/settings` returns a **masked** key. Sending the masked value back on PUT keeps the existing secret.

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

Include `project_id` and `session_id` (the chat id) in `/v1/agent/chat`
requests. The agent only sees files inside that project's workspace. Edits of
an existing clip replace that file in place; the bin does not collect a new
copy on every process. Pass `apply_to: "none"` only when the user wants a
separate export.

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
chats are written under `.parallax/chats/` and survive server restarts.

## Tools

| Tool             | Purpose |
|------------------|---------|
| `list_workspace` | Inventory media in the sandbox |
| `inspect_file`   | Size / mtime |
| `probe_media`    | `ffprobe` JSON |
| `run_ffmpeg`     | One validated ffmpeg/ffprobe command |

## Tests

```bash
go test ./...
```
