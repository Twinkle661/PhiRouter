# PhiRouter

**A small Go router that connects agent harnesses to chat frontends.**

Most agent harnesses are CLI-native, and each one that wants a chat interface ends up shipping its own bot integration. PhiRouter separates those concerns: harnesses plug in as **backends**, chat platforms plug in as **frontends**, and a transport-independent core routes conversations between them. A backend should not need to know which frontend it is talking to, and the reverse.

The architecture is built for **many backends × many frontends**. The first release implements one pair:

| Side | Adapter | Transport | Status |
| --- | --- | --- | --- |
| Backend | [pi](https://github.com/earendil-works/pi) coding agent | `pi --mode rpc` (stdin/stdout JSONL) | ✅ implemented |
| Frontend | Telegram bot (single bound user) | Bot API long polling | ✅ implemented |
| Backend | Other harnesses (e.g. over HTTP or ACP) | adapter-private | planned |
| Frontend | Other chat platforms | adapter-private | planned |

## What it does

- **Routing**: declare frontend and backend instances in config and connect them with rules such as `telegram:* → pi:default`. Multiple backend instances can run side by side, each with its own conversation.
- **Text, images, voice and files in both directions**: attachments are passed to the agent by path in a shared media directory, never as inline blobs.
- **Streaming as chat bubbles**: model output is split into separate messages on configurable separators.
- **Interrupts and priorities**: user messages steer a busy agent; plugin messages queue behind it; `/abort` really stops.
- **Interactive dialogs**: the backend's select / confirm / input prompts become inline buttons and replies.
- **Proactive messages**: an authenticated HTTP injection API lets external processes (schedulers, reminder bots) start a turn.
- **Agent → user file delivery**: a built-in MCP server gives the agent a `send_file` tool.
- **Hooks**: in-process plugins can transform or drop inbound/outbound messages and observe events.
- **Recovery**: the backend process is supervised, its session is persisted and restored, and messages received during recovery are replayed.

## Architecture

```
            ┌──────────── frontends ────────────┐
            │  telegram   (future: …)           │
            └───────────────┬───────────────────┘
                            │ core.Envelope{Parts}
      HTTP inject API ──▶ ┌─┴───────────────────────┐ ◀── hooks / plugins
                          │  router                 │
                          │  routes · queues ·      │
                          │  priorities · bubbles   │
      MCP send_file  ───▶ └─┬───────────────────────┘
                            │ typed backend operations
            ┌───────────────┴───────────────────┐
            │  pi (stdio JSONL)  (future: …)    │
            └──────────── backends ─────────────┘
```

Design rules:

- **`internal/core` is the only shared vocabulary.** It defines `Envelope` / `Part` (text, image, audio, file), the `Frontend` and `Backend` interfaces, `Hook`, `MediaStore`, and scheduling intent (`DeliveryStart`, `DeliveryInterrupt`, `DeliveryQueue`).
- **Adapters depend on `core` and nothing else.** Adapters, router and plugins never import one another; a test (`internal/core/architecture_test.go`) enforces this.
- **Transport is a backend detail.** pi's JSONL wire commands and JSON maps stay private to its adapter. Backend adapters expose typed operations (`Submit`, `Status`, `Models`, `SetModel`, `NewSession`, `Abort`, `Respond`, `Restart`) with typed results.
- **Composition lives in `cmd/router`.** It loads config, chooses implementations and wires them together.
- **Minimal dependencies.** The only third-party Go module is `gopkg.in/yaml.v3`. Telegram, HTTP, JSON-RPC, subprocess management, dotenv parsing and UUIDs use the standard library.

```
cmd/router/                 composition root: config → adapters → router
internal/core/              contracts shared by everything
internal/router/            routing, scheduling queues, priorities, bubble splitting
internal/config/            env > .env > YAML > defaults
internal/media/             confined media store, TTL cleanup
internal/api/               injection API, /healthz, /status
internal/mcp/               MCP server exposing send_file
adapters/backend/pi/        pi RPC backend
adapters/frontend/telegram/ Telegram frontend
plugins/                    optional built-in hooks (prefix, ticker)
```

## Quick start (Docker)

Requirements: Docker Compose, a Telegram bot token from BotFather, your numeric Telegram user ID, and an account with a model provider supported by pi.

```sh
cp config.example.yaml config.yaml
cp .env.example .env
# Edit .env: set your bot token, user ID, and TWO different random bearer secrets
# (e.g. `openssl rand -hex 32`).
docker compose build
# Log pi in once: run /login, pick your provider, then /exit.
docker compose run --rm --entrypoint pi router
docker compose up -d
docker compose logs -f router
```

Open a private chat with your bot and send a message. Telegram requires you to contact the bot once before it can send you proactive messages.

Deployment notes:

- Router and pi run in **one container** as the non-root `node` user; the router spawns pi as a child process. Config is mounted read-only.
- The pi login, session pointer, extensions and `mcp.json` live in the `pi-agent` volume; files live in the `media` volume.
- The injection API is reachable by sibling containers at `http://router:8080` but is **not published to the host**. To reach it from the host, add a loopback-only mapping (`127.0.0.1:8080:8080`). Never publish it on a public interface. MCP stays bound to `127.0.0.1:8081` inside the container.
- To use provider API keys instead of interactive login, add the provider's variable (e.g. `ANTHROPIC_API_KEY`) to `.env`; Compose passes it to pi.
- The image pins **pi 1.6.0** on the Node 22 line (pi needs Node ≥ 22.19) and builds with Go 1.26. Upgrading pi means re-checking its RPC contract and running the tests.

## Local development

Install Go 1.26.10+, Node 22.19+, pi and ffmpeg (for `ffprobe`):

```sh
npm install -g --ignore-scripts @earendil-works/pi-coding-agent@1.6.0
cp config.example.yaml config.yaml
cp .env.example .env
# Edit secrets, and set media.dir to an absolute writable directory.
make test
make vet
make build
pi            # run /login once
./bin/router -config config.yaml -env .env
```

Backend `command` arrays are executed directly, without a shell. `./bin/router -healthcheck http://127.0.0.1:8080/healthz` probes a running router without loading credentials. Local `-env` loading only applies `ROUTER_*` variables, so export provider credentials in your shell when running outside Docker.

## Configuration

See [`config.example.yaml`](config.example.yaml) for every section. Precedence is **process environment > `.env` > YAML > defaults**. Unknown YAML fields and unknown `ROUTER_` environment paths fail startup, so typos are never silently ignored.

Every field can be overridden with a double-underscore path. Arrays and maps accept YAML/JSON values, and existing array elements can be addressed by index:

```sh
export ROUTER_STREAM__SHOW_TOOL_CALLS=true
export ROUTER_STREAM__SEPARATORS='["\n", "<break />"]'
export ROUTER_MEDIA__TTL=48h
export ROUTER_BACKENDS__DEFAULT__COMMAND='["pi", "--mode", "rpc", "--provider", "anthropic"]'
export ROUTER_ROUTES__0__FROM='telegram:123456789'
```

Parent paths apply before child paths. Map keys in environment paths are lowercase. YAML maps merge with default entries; pass a whole-map environment value to replace one completely. Dotenv supports assignments, comment lines, optional `export` and quoted values, but never shell expansion or interpolation. `config.yaml` and `.env` may be absent if the environment supplies all required settings.

### Routes and multiple backends

- Each backend instance keeps one conversation/session. Declare several pi backends with distinct names and route sources to them independently.
- Exact routes (`telegram:<chat_id>`) take precedence over wildcards (`telegram:*`). Avoid overlapping routes to the same source if you expect separate conversations.
- Proactive traffic returns to the backend's last authorized source, or its configured exact route. A wildcard with no prior inbound traffic defaults to the bound user's private chat.
- Only updates from the configured Telegram user ID are accepted, including callback queries. That user may also talk from group chats if a route allows it; use an explicit `telegram:<chat_id>` route to constrain the destination.

### Timestamps

With `timestamp.enabled`, the router prefixes user messages with the time the frontend reports they were sent (Telegram's message date), so backlog delivered after a restart keeps its original times.

```yaml
timestamp:
  enabled: true
  timezone: Europe/London            # IANA name; default UTC; DST aware
  format: "[2006-01-02 15:04:05 MST] " # Go reference layout, including separators
```

This yields `[2026-11-21 14:30:00 GMT] Hello`. The prefix is applied once, before queueing and inbound hooks, so `/retry` and reconnects never double it. Router commands, dialog replies and forwarded slash commands are not prefixed; API/plugin injections and outbound messages are not timestamped. Media-only messages get a leading text part. Invalid timezones or empty formats fail startup when enabled.

### Telegram presentation

System notices and dialogs carry a `System` header. `telegram.system_prefix` (default `𝑴𝒆𝒐𝒘☕︎`; `""` keeps just `System`) customizes the decoration, and `telegram.system_style` picks the layout:

- `full`: header, divider, multiline details.
- `compact`: a single `𝑴𝒆𝒐𝒘☕︎ · text` line.
- `auto` (default): compact for single-line notices such as tool summaries, full for multiline details.

Agent replies are never decorated. Session fields the backend cannot provide are shown as `Unavailable`.

## Using it

### Commands

| Command | Effect |
| --- | --- |
| `/retry` | Resubmit the most recent accepted user message, including attachments. |
| `/restart` | Capture the current session and restart the backend. |
| `/model` | List models; `/model provider/model-id` selects one directly. |
| `/new` | Start a new session and show its ID, provider and model. |
| `/abort` | Clear queued work and abort the current generation. |
| `/status` | Show readiness, session, model, activity and queue length. |

Unknown slash commands are forwarded to the backend, so pi extension commands, skills and prompt templates keep working.

### Scheduling

- While the agent is busy, user messages **steer** it (at pi's turn/tool boundaries; a running tool is not killed) and plugin messages **queue** as follow-ups.
- The pi adapter implements both through `prompt.streamingBehavior`, so a run finishing mid-dispatch cannot strand a message.
- Plugin slash commands are rejected while busy, because pi executes extension commands immediately.
- Output is flushed at message and run end, but the backend is considered idle only on `agent_settled`.

### Dialogs

Select and confirm dialogs become inline buttons; input and editor dialogs use ForceReply, so answer by replying to that message. The original message is updated with the selection. Timeouts belong to the backend; expired callbacks are ignored. Large model lists are split across several messages.

### Output formatting

Text is sent as plain text (no Markdown) so arbitrary model output never triggers Telegram parse errors. Separators are removed even when split across RPC events; empty bubbles are skipped; long bubbles are split at 4096 UTF-16 units. Tool calls are hidden by default. With `stream.show_tool_calls`, only one start and one end summary (name, outcome, duration) is posted per tool. Arguments and results stay available to hooks.

### Attachments

Incoming files are saved under `media.dir/in/` with random UUID names and passed to the agent as a text line (`prompts.attachment`, e.g. `[attachment: /data/media/in/<uuid>.pdf (application/pdf, 12.3 MB)]`). Audio and video are treated as ordinary files: no STT, TTS or document parsing. Files under `in/` and `out/` are removed after `media.ttl`. Image thresholds and download limits use MiB; displayed sizes use decimal MB.

## Integrations

### Injection API

External processes can start turns, for example a reminder scheduler or a proactive-messaging service:

```sh
curl -X POST http://127.0.0.1:8080/v1/inject \
  -H "Authorization: Bearer $ROUTER_API__TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"route":"pi:default","parts":[{"type":"text","text":"Say good morning."}]}'
```

| Status | Meaning |
| --- | --- |
| `202` | Accepted and queued |
| `400` | Malformed or unsafe input |
| `401` | Bad or missing token |
| `503` | Route unavailable or queue full |

Requests are capped at 1 MiB. Part types are `text`, `image`, `audio` and `file`; non-text parts must reference an existing absolute path inside the media directory, and MIME/size are computed from the file rather than trusted. The API is inbound-only: no outgoing content is exposed. `/healthz` reports router liveness (healthy even during backend recovery); `/status` reports backend readiness.

### MCP file delivery

The router serves a stateless Streamable HTTP MCP endpoint at `/mcp` (`initialize`, `ping`, `tools/list`, `tools/call`; protocol versions 2025-03-26, 2025-06-18 and 2025-11-25) with one tool:

```json
{"name":"send_file","arguments":{"path":"/data/media/out/report.pdf","caption":"Report"}}
```

- On startup the router atomically merges a reserved `phirouter` entry into `~/.pi/agent/mcp.json`, preserving your other servers. Tokens are stored as environment references, not plaintext.
- Each backend process gets `PHIROUTER_BACKEND` in its environment, which becomes the `X-PhiRouter-Route` header, so one endpoint serves many backends and files go back to the right chat.
- The MCP token is separate from the injection token. Cross-origin requests are rejected. Paths are opened through Go's `os.Root`, so traversal and escaping symlinks are refused.
- Audio in `.ogg`, `.opus`, `.mp3` or `.m4a` is sent as a voice message only when ffprobe confirms audio-only content shorter than `media.voice_max_seconds` (max 60). Everything else, images included, goes out as a document with original bytes. Captions are limited to 1024 UTF-16 units. Delivery does not wait for generation to finish.

Inspect the connection inside the container:

```sh
docker compose exec -e PHIROUTER_BACKEND=pi:default router pi mcp list
```

### Plugins

`core.Hook` provides ordered inbound/outbound transformations (including drop) and read-only event observation. Hooks are trusted in-process Go code: they must return promptly, and must trigger injections from their own goroutines rather than calling `Inject` synchronously, because router state transitions are serialized. Hook errors are logged and the original message continues. Trigger goroutines receive a `core.Inject` handle and always use plugin priority.

Two optional built-ins show the pattern, and neither is enabled by default:

```yaml
plugins:
  - type: prefix          # prepend text to inbound prompts
    prefix: "Reply briefly."
  - type: ticker          # inject on a fixed interval from startup (not a calendar)
    interval: 24h
    route: pi:default
    text: "Check whether the user needs a daily reminder."
```

## Adding an adapter

1. Implement `core.Backend` or `core.Frontend` in a new package under `adapters/`, importing only `internal/core`. Keep wire formats and transport inside the package.
2. Register its factory and config validation in `cmd/router` and `internal/config`.
3. Leave `internal/core` and `internal/router` unchanged. If a feature seems to need a core change, that is a contract discussion first.

Current composition-layer limits: this build requires exactly one frontend named `telegram`, and the composition root manages pi-specific state (`~/.pi/agent`, `mcp.json` registration). Both are confined to `cmd/router` and `internal/config` and will be generalized as adapters are added.

## Reliability and limits

- **Supervision**: pi exits trigger exponential restart backoff (1–60 s, reset after a minute of stable runtime).
- **Sessions**: the active session pointer is captured on startup, agent start/settle, `/new` and `/restart`, then atomically persisted per backend. A failed restore is surfaced and retried, never silently replaced with a new conversation. An explicit `session_file` overrides the pointer.
- **Replay**: messages received during recovery are replayed FIFO within user/plugin priority. Work pi already accepted is **not** replayed after a crash, because tools may already have run; use `/retry` deliberately.
- **Deadlines**: interactive backend operations have a 20 s caller deadline. A timeout only ends that wait. It does not kill pi or replay, and a command whose write had started may still execute. A single serialized writer per process keeps JSONL records intact; late responses can never satisfy another request.
- **Queues**: each backend's recovery queue holds 256 envelopes by default (`queue_limit`) and rejects overflow visibly. Queues are memory-only.
- **Telegram**: 429 responses honor `retry_after` up to three times; ambiguous network failures are logged rather than retried, to avoid duplicates.
- **Shutdown**: SIGTERM stops polling and HTTP admission, flushes buffered bubbles, then closes pi's stdin with a bounded wait before killing it. Startup and graceful shutdown send a best-effort notice to the bound user.
- **No durability guarantees**: there is no database, durable outbox or exactly-once delivery. A lost network response means an uncertain delivery result. Slow frontend calls apply backpressure to dispatch.
- **Non-goals for now**: multi-user Telegram, webhooks, config hot reload, outbound plugin subscriptions, web UI.

## Testing

```sh
make test   # go test ./...  (go test -race ./... additionally needs a C compiler)
make vet
```

The suite covers scheduling with fake frontends/backends, stream boundaries, recovery with a real JSONL child process, Telegram HTTP and dialog flows against a local fake server (opens loopback sockets), auth, MCP delivery, path confinement, config layering and the package dependency rule. No real Telegram or provider credentials are needed. Audio tests use real ffprobe when available and skip otherwise.

Container smoke test, offline and without credentials:

```sh
docker build -t phirouter:local .
docker run --rm --network none --entrypoint node \
  --mount type=bind,source="$PWD/scripts/smoke.mjs",target=/tmp/smoke.mjs,readonly \
  phirouter:local /tmp/smoke.mjs
```

It verifies real pi MCP discovery, bearer auth, path rejection, liveness and graceful shutdown.

## References

[pi RPC](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc.md) · [RPC commands](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc-commands.md) · [extension UI](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc-extension-ui.md) · [pi MCP configuration](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/mcp.md) · [Telegram Bot API](https://core.telegram.org/bots/api) · [MCP Streamable HTTP](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports)

## License

[MIT](LICENSE)
