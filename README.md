# Parallax backend

Go service for the Parallax media agent. A user describes a video/audio/image task; a framework-free agent loop streams a plan, calls tools, runs **ffmpeg/ffprobe** in a workspace sandbox, and keeps going until the job is done.

The frontend is not wired yet. Talk to the API directly.

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
- Groq: `https://api.groq.com/openai/v1`
- OpenRouter: `https://openrouter.ai/api/v1`
- Ollama: `http://127.0.0.1:11434/v1`

## Run

```bash
cd parallax_backend
cp .env.example .env   # then put a key in LLM_API_KEY or XAI_API_KEY
go run ./cmd/server
```

Drop media into `./workspace`. The agent can only read and write that directory.

## Chat (SSE)

```bash
curl -N localhost:8080/v1/agent/chat \
  -H 'content-type: application/json' \
  -d '{"message":"strip audio from talk.mp4 and write talk_muted.mp4"}'
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

Pass `session_id` on the next request to continue the same conversation.

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
