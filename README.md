# PhiRouter

**A small Go router that connects agent harnesses to chat frontends.**

Most agent harnesses are CLI-native, and each one that wants a chat interface ends up shipping its own bot integration. PhiRouter separates those concerns: harnesses plug in as **backends**, chat platforms plug in as **frontends**, and a transport-independent core routes conversations between them. A backend should not need to know which frontend it is talking to, and the reverse.

The architecture is built for **many backends × many frontends**:

| Side | Adapter | Transport | Status |
| --- | --- | --- | --- |
| Backend | [pi](https://github.com/earendil-works/pi) coding agent | `pi --mode rpc` (stdin/stdout JSONL) | ✅ implemented |
| Backend | Any [ACP](https://agentclientprotocol.com) agent, e.g. `opencode acp` | JSON-RPC 2.0 over stdio | ✅ implemented |
| Frontend | Telegram bot (single bound user) | Bot API long polling | ✅ implemented |
| Backend | Native harness protocols (e.g. Codex app-server) | adapter-private | planned |
| Frontend | Other chat platforms (polling, WebSocket or webhook) | adapter-private | planned |

Backends differ in what they can do: pi steers a running turn and queues follow-ups itself, while ACP agents take one prompt at a time. Each backend declares `core.Capabilities`, and the router fills the gaps (see [Scheduling](#scheduling)).

## What it does

- **Routing**: declare frontend and backend instances in config and connect them with rules such as `tg:* → default`. Several frontend instances (even two bots of the same platform) and several backend instances can run side by side.
- **Text, images, voice and files in both directions**: attachments are passed to the agent by path in a shared media directory, never as inline blobs.
- **Streaming as chat bubbles**: model output is split into separate messages on configurable separators.
- **Interrupts and priorities**: user messages steer a busy agent where the backend can, otherwise wait or interrupt by policy; plugin messages queue behind them; `/abort` really stops.
- **Interactive dialogs**: the backend's select / confirm / input prompts and permission requests become inline buttons and replies.
- **Proactive messages**: an authenticated HTTP injection API lets external processes (schedulers, reminder bots) start a turn.
- **Agent → user delivery**: a built-in MCP server gives the agent a `send_file` tool and, optionally, `send_voice` (text-to-speech through any OpenAI-compatible speech endpoint).
- **Hooks**: in-process plugins can transform or drop inbound/outbound messages and observe events.
- **Webhook ingress**: frontends that receive platform events over HTTP get a dedicated listener that only starts when used.
- **Recovery**: the backend process is supervised, its session is persisted and restored, and messages received during recovery are replayed.

## Architecture

```
            ┌──────────── frontends ────────────┐
 webhooks ─▶│  telegram   (future: …)           │
 (ingress)  └───────────────┬───────────────────┘
                            │ core.Envelope{Parts}
      HTTP inject API ──▶ ┌─┴───────────────────────┐ ◀── hooks / plugins
                          │  router                 │
                          │  routes · queues ·      │
                          │  priorities · bubbles   │
      MCP send_file  ───▶ └─┬───────────────────────┘
                            │ typed backend operations
            ┌───────────────┴───────────────────┐
            │  pi (stdio JSONL) · acp (JSON-RPC)│
            └──────────── backends ─────────────┘
```

Design rules:

- **`internal/core` is the only shared vocabulary.** It defines `Envelope` / `Part` (text, image, audio, file), the `Frontend` and `Backend` interfaces, `Hook`, `MediaStore`, backend `Capabilities` and delivery modes (`DeliveryPrompt`, `DeliverySteer`, `DeliveryFollowUp`).
- **Backend events use ACP's vocabulary.** Adapters translate their wire protocol into typed `core.Event` kinds (message chunks, tool calls with ACP statuses, dialogs, turn ends with a stop reason). Records without router meaning pass through as `EventExtension` with the original record in `Raw`, so hooks still see everything.
- **Addresses are opaque.** A frontend reports and receives bare, adapter-defined addresses (a Telegram chat ID, a Discord channel, a WhatsApp JID…). The router qualifies them as `<instance>:<address>`; the config layer asks the adapter to validate them.
- **Adapters depend on `core` and nothing else.** Adapters, router and plugins never import one another; a test (`internal/core/architecture_test.go`) enforces this.
- **Transport is a backend detail.** Wire commands, session files and agent configuration (such as pi's `mcp.json`) stay private to each adapter. Backend adapters expose typed operations (`Prompt`, `Status`, `Models`, `SetModel`, `NewSession`, `Cancel`, `Respond`, `Restart`) with typed results.
- **Composition lives in `cmd/router`.** Registries of adapter types (`cmd/router/frontends.go`, `backends.go`, `tools.go`) build configured instances; config, core and router contain no platform or harness knowledge.
- **Minimal dependencies.** The only third-party Go module is `gopkg.in/yaml.v3`. Telegram, HTTP, JSON-RPC, subprocess management, dotenv parsing and UUIDs use the standard library.

```
cmd/router/                 composition root: config → adapters → router
internal/core/              contracts shared by everything
internal/router/            routing, scheduling queues, priorities, bubble splitting
internal/config/            env > .env > YAML > defaults
internal/media/             confined media store, TTL cleanup
internal/api/               injection API, /healthz, /status
internal/ingress/           webhook listener for frontends that need one
internal/mcp/               MCP protocol server and tool registry
internal/tools/             MCP tools (send_file, send_voice)
adapters/backend/pi/        pi RPC backend
adapters/backend/acp/       Agent Client Protocol backend
adapters/frontend/telegram/ Telegram frontend
adapters/tts/openaispeech/  OpenAI-compatible speech provider
plugins/                    optional built-in hooks (prefix, ticker)
```

## Quick start (Docker)

Requirements: Docker Compose, a Telegram bot token from BotFather, your numeric Telegram user ID, and an account with a model provider supported by pi.

```sh
mkdir router pi
cp config.example.yaml router/config.yaml
sudo chown -R 1000:1000 router pi   # the container runs as node (uid 1000)
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

- Router and pi run in **one container** as the non-root `node` user; the router spawns pi as a child process.
- Two kinds of host directories are bind-mounted. `./router` holds what the router persists: `config.yaml` and `state/<backend name>/` (pi's session pointer, an ACP session ID); `state.dir` is relative to the config file. Each backend's own home is mounted where that agent looks by default, e.g. `./pi` at `~/.pi/agent` for pi's login, sessions, extensions and `mcp.json`. To add Codex, install it in a derived image and uncomment the `./codex` mount; other agents work the same way.
- Media files are temporary (`media.ttl`) and stay inside the container.
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
export ROUTER_STREAM__SHOW_TOOL_CALLS__ENABLED=true
export ROUTER_STREAM__SEPARATORS='["\n", "<break />"]'
export ROUTER_MEDIA__TTL=48h
export ROUTER_BACKENDS__DEFAULT__OPTIONS__COMMAND='["pi", "--mode", "rpc", "--provider", "anthropic"]'
export ROUTER_ROUTES__0__FROM='tg:123456789'
export ROUTER_FRONTENDS__TG__OPTIONS__TOKEN='123456:bot-token'
```

Parent paths apply before child paths. Map keys in environment paths are lowercase. YAML maps merge with default entries; pass a whole-map environment value to replace one completely. Dotenv supports assignments, comment lines, optional `export` and quoted values, but never shell expansion or interpolation. `config.yaml` and `.env` may be absent if the environment supplies all required settings.

### Frontends

Each entry under `frontends` is an instance; its name (lowercase letters, digits, `-`, `_`) is used in routes and environment paths.

```yaml
frontends:
  tg:
    type: telegram
    owner: "123456789"        # bound user, in the adapter's address format
    options:                  # adapter-specific, decoded and validated by the adapter
      token: "123456:bot-token"
      system_prefix: "𝑴𝒆𝒐𝒘☕︎"
      system_style: auto
      voice_max_seconds: 60
    # ingress:                # webhook-based adapters only
    #   path: /hooks/tg

lifecycle_notify: [tg]        # owners that receive startup/shutdown notices
```

- `owner` is required. Adapters use it to authorize the user and as the default destination.
- `options` are strict like the rest of the config: unknown keys fail startup, and every option can be overridden with `ROUTER_FRONTENDS__<NAME>__OPTIONS__<KEY>`.
- A whole frontend can be declared from the environment (`ROUTER_FRONTENDS__TG__TYPE`, `…__OWNER`, `…__OPTIONS__TOKEN`).

#### Telegram options

| Option | Default | Meaning |
| --- | --- | --- |
| `token` | required | Bot token from BotFather. |
| `system_prefix` | `𝑴𝒆𝒐𝒘☕︎` | Decoration for system notices; `""` keeps just `System`. |
| `system_style` | `auto` | `full` (header, divider, details), `compact` (one `prefix · text` line), or `auto` (compact for single-line notices). |
| `voice_max_seconds` | `60` | Longest audio sent as a voice message (max 60). |

The owner is a numeric Telegram user ID. Only that user's updates, including callback queries, are accepted. They may also talk from group chats if a route allows it; use an explicit `tg:<chat_id>` route to constrain the destination. Agent replies are never decorated; session fields the backend cannot provide are shown as `Unavailable`.

### Webhook ingress

Frontends whose platform delivers events by HTTP declare `ingress.path`. The router then serves those paths, matched exactly, on a separate listener:

```yaml
ingress:
  listen: "0.0.0.0:8443"   # may be public, unlike the API and MCP listeners
```

The listener starts only if at least one frontend declares a path. Request bodies are capped at 1 MiB; signature verification is the adapter's job. Put TLS in front of it with your usual reverse proxy. A frontend type that does not support webhooks fails startup if given a path.

### Backends

Each entry under `backends` is an instance, named like frontends. Without a `backends` section the router runs one pi instance called `default`; declaring any backend replaces it.

```yaml
backends:
  default:
    type: pi
    options:
      command: ["pi", "--mode", "rpc"]
  coder:
    type: acp
    busy_policy: interrupt     # queue (default) or interrupt; see Scheduling
    options:
      command: ["opencode", "acp"]
      cwd: /workspace
```

- `type` picks the adapter; `options` belong to it and are strict like frontend options (`ROUTER_BACKENDS__<NAME>__OPTIONS__<KEY>`).
- `busy_policy` is a router setting: what a user message does while a backend that cannot steer is busy.
- Backend `command` arrays are executed directly, without a shell.

#### pi options

| Option | Default | Meaning |
| --- | --- | --- |
| `command` | `["pi", "--mode", "rpc"]` | pi in RPC mode. |
| `session_file` | empty | Pin a session file; empty resumes the last session. |
| `agent_dir` | `~/.pi/agent` | pi's agent directory; when set it is also passed as `PI_CODING_AGENT_DIR`. |
| `attachment` | `[attachment: {path} ({mime}, {size})]` | Text line describing each attached file. |
| `inline_image_max_mb` | `5` | Images up to this size are also sent inline (max 5, `0` disables). |

#### acp options

| Option | Default | Meaning |
| --- | --- | --- |
| `command` | required | The agent in ACP mode, e.g. `["opencode", "acp"]`. |
| `cwd` | required | Absolute working directory of the session. |
| `env` | `{}` | Extra environment variables for the agent. |
| `permission` | `ask` | `ask` shows permission requests as dialogs; `allow` / `reject` answer them with the agent's allow-once / reject-once option. |
| `inline_image_max_mb` | `5` | Images up to this size are sent inline if the agent accepts images; other files are sent as `file://` resource links. |

The router advertises no file system or terminal access, so agents use their own tools. The router's MCP endpoint is passed in `session/new` when the agent accepts HTTP MCP servers. The session ID is stored in `state.dir/<name>/session.json` and resumed with `session/load` when the agent supports it; the replayed history is not re-sent to the chat. Authenticate the agent beforehand (for example `opencode auth login`).

### Routes and multiple backends

- Route sources are `<frontend instance>:<address>` or `<frontend instance>:*`; destinations are backend instance names.
- Each backend instance keeps one conversation/session. Declare several backends, of the same or different types, and route sources to them independently.
- Exact routes take precedence over wildcards. Avoid overlapping routes to the same source if you expect separate conversations.
- Proactive traffic returns to the backend's last authorized source, or its configured exact route. A wildcard with no prior inbound traffic defaults to that frontend's owner.

### Timestamps

With `timestamp.enabled`, the router prefixes user messages with the time the frontend reports they were sent (Telegram's message date), so backlog delivered after a restart keeps its original times.

```yaml
timestamp:
  enabled: true
  timezone: Europe/London            # IANA name; default UTC; DST aware
  format: "[2006-01-02 15:04:05 MST] " # Go reference layout, including separators
```

This yields `[2026-11-21 14:30:00 GMT] Hello`. The prefix is applied once, before queueing and inbound hooks, so `/retry` and reconnects never double it. Router commands, dialog replies and forwarded slash commands are not prefixed; API/plugin injections and outbound messages are not timestamped. Media-only messages get a leading text part. Invalid timezones or empty formats fail startup when enabled.

## Using it

### Commands

| Command | Effect |
| --- | --- |
| `/retry` | Resubmit the most recent accepted user message, including attachments. |
| `/restart` | Capture the current session and restart the backend. |
| `/model` | List models; `/model provider/model-id` (or a bare model ID for backends without providers) selects one directly. |
| `/new` | Start a new session and show its ID, provider and model. |
| `/abort` | Clear queued work and abort the current generation. |
| `/status` | Show readiness, session, model, activity and queue length. |

Unknown slash commands are forwarded to the backend, so agent commands, skills and prompt templates keep working. `/model` and `/new` report "not supported" on backends without those capabilities.

### Scheduling

What happens to a message while the agent is busy depends on the backend's capabilities:

| Message | Backend can steer | Backend queues follow-ups | Neither (e.g. ACP) |
| --- | --- | --- | --- |
| User | steers the running turn | follows up | `busy_policy: queue` holds it until the turn ends; `interrupt` cancels the turn, then sends it |
| Plugin / API | follows up | follows up | held until the turn ends |

- Held messages go out one per turn, user messages first, and count toward `queue_limit`.
- pi does both: user messages steer at its turn/tool boundaries (a running tool is not killed) and plugin messages queue as follow-ups, through `prompt.streamingBehavior`, so a run finishing mid-dispatch cannot strand a message. Plugin slash commands are rejected while pi is busy, because pi executes extension commands immediately.
- Output is flushed at message and turn end; the backend is idle only once its turn ends (pi's `agent_settled`, ACP's `session/prompt` response).

### Dialogs

Select, confirm and permission dialogs become inline buttons; input and editor dialogs use ForceReply, so answer by replying to that message. A Cancel button is added unless the dialog already has a rejecting option. The original message is updated with the selection. Timeouts belong to the backend; expired callbacks are ignored. Large model lists are split across several messages.

### Output formatting

Text is sent as plain text (no Markdown) so arbitrary model output never triggers Telegram parse errors. Separators are removed even when split across RPC events; empty bubbles are skipped. The router does not split by length; each frontend enforces its platform limit (Telegram splits at 4096 UTF-16 units). Tool calls are hidden by default. With `stream.show_tool_calls.enabled`, one notice is posted when each tool starts and one more only if it fails (with its duration); successful completions are silent, since the agent's reply marks the end. With `detailed` (the default), start notices also show a one-line argument summary, such as `Tool started: bash · git status`: the command, pattern, path, URL or query of the tool's arguments, otherwise the compact JSON arguments, clipped to `max_detail_length` runes (default 120). Details travel through the chat platform, so set `detailed: false` if arguments may hold secrets. Results are never posted; full events stay available to hooks. The older `show_tool_calls: true` form still works.

### Attachments

Incoming files are saved under `media.dir/in/` with random UUID names and passed to the agent by path: pi gets a text line (its `attachment` option, e.g. `[attachment: /data/media/in/<uuid>.pdf (application/pdf, 12.3 MB)]`), ACP agents get a `file://` resource link. Incoming audio and video are treated as ordinary files: no STT or document parsing. Files under `in/` and `out/` are removed after `media.ttl`. Image thresholds and download limits use MiB; displayed sizes use decimal MB.

## Integrations

### Injection API

External processes can start turns, for example a reminder scheduler or a proactive-messaging service:

```sh
curl -X POST http://127.0.0.1:8080/v1/inject \
  -H "Authorization: Bearer $ROUTER_API__TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"route":"default","parts":[{"type":"text","text":"Say good morning."}]}'
```

| Status | Meaning |
| --- | --- |
| `202` | Accepted and queued |
| `400` | Malformed or unsafe input |
| `401` | Bad or missing token |
| `503` | Route unavailable or queue full |

Requests are capped at 1 MiB. Part types are `text`, `image`, `audio` and `file`; non-text parts must reference an existing absolute path inside the media directory, and MIME/size are computed from the file rather than trusted. The API is inbound-only: no outgoing content is exposed. `/healthz` reports router liveness (healthy even during backend recovery); `/status` reports backend readiness.

### MCP tools

The router serves a stateless Streamable HTTP MCP endpoint at `/mcp` (`initialize`, `ping`, `tools/list`, `tools/call`; protocol versions 2025-03-26, 2025-06-18 and 2025-11-25). `send_file` is always available; `send_voice` is opt-in.

- Each backend adapter registers the endpoint with its agent. pi: on startup the router atomically merges a reserved `phirouter` entry into the agent directory's `mcp.json`, preserving your other servers, with tokens stored as environment references, not plaintext. ACP: the endpoint and its headers are passed in `session/new` / `session/load`.
- Every request carries the backend instance name in the `X-PhiRouter-Route` header (for pi via `PHIROUTER_BACKEND`), so one endpoint serves many backends and files go back to the right chat.
- The MCP token is separate from the injection token. Cross-origin requests are rejected. Paths are opened through Go's `os.Root`, so traversal and escaping symlinks are refused.

#### `send_file`

```json
{"name":"send_file","arguments":{"path":"/data/media/out/report.pdf","caption":"Report"}}
```

- Audio in `.ogg`, `.opus`, `.mp3` or `.m4a` is sent as a voice message only when ffprobe confirms audio-only content shorter than the Telegram option `voice_max_seconds` (max 60). Everything else, images included, goes out as a document with original bytes. Captions are limited to 1024 UTF-16 units. Delivery does not wait for generation to finish.

#### `send_voice`

```json
{"name":"send_voice","arguments":{"text":"Good night.","style":"sleepy and tender"}}
```

The router synthesizes `text`, sends the audio through the same path as `send_file` (a voice message if short enough, otherwise a file) and deletes it once delivered; the agent only gets the result. With `style.enabled`, the tool also accepts an optional `style` describing how to say this message (emotion, mood, pace; not voice characteristics). It is appended to `style.base` and written to the request field named by the provider's `style_field`.

```yaml
mcp:
  tools:
    send_voice:
      enabled: true
      style:
        enabled: true
        base: ""
      provider:
        type: openai_speech
        options:
          base_url: https://openrouter.ai/api/v1
          model: openai/gpt-4o-mini-tts-2025-12-15
          voice: nova
          response_format: mp3
          style_field: instructions
```

Set the key with `ROUTER_MCP__TOOLS__SEND_VOICE__PROVIDER__OPTIONS__API_KEY`. `openai_speech` works with any `POST {base_url}/audio/speech` endpoint (OpenRouter, OpenAI, DeepInfra's `/v1/openai`, self-hosted servers):

| Option | Default | Meaning |
| --- | --- | --- |
| `base_url` | `https://openrouter.ai/api/v1` | API root; `/audio/speech` is appended. |
| `api_key` | empty | Bearer token; omitted when empty. |
| `model`, `voice` | required | Provider-specific identifiers. |
| `response_format` | `mp3` | `mp3` or `opus`, the formats Telegram plays as voice. |
| `speed` | `0` | Omitted when `0`, otherwise 0.25–4. |
| `style_field` | `instructions` | Dot path in the request body that receives style text, e.g. `provider.options.<slug>.<field>` for provider-specific fields. Only some models honour style; disable it otherwise. |
| `extra_body` | `{}` | Merged into every request; standard fields win. |
| `timeout` | `60s` | Whole-request timeout. |

Provider errors (bad key, unknown voice) are returned to the agent as tool errors. Other protocol families (for example ElevenLabs) can be added as new provider types in `cmd/router/tools.go`.

Inspect the connection inside the container:

```sh
docker compose exec -e PHIROUTER_BACKEND=default router pi mcp list
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
    route: default
    text: "Check whether the user needs a daily reminder."
```

## Adding a frontend

1. Create a package under `adapters/frontend/` that imports only `internal/core`, and implement `core.Frontend`:
   - `Run` reports inbound messages with a bare address in `Source`, and the platform message reference in `ReplyRef` if replies can be attached to it.
   - `Send` enforces platform limits (message length, media types). Outbound envelopes carry the latest inbound `ReplyRef` for the conversation. When platform rules refuse a delivery (a closed messaging window, no unsolicited messages), wrap `core.ErrSendNotAllowed`; the router reports it to hooks as an `EventDeliveryRefused` event and to the caller (for example the agent's `send_file`) instead of treating it as a transport failure.
   - `Typing` may be a no-op; `Dialog` should degrade (for example to numbered text) when the platform lacks buttons or reply prompts.
   - Implement `core.IngressFrontend` (`Ingress() http.Handler`) if the platform delivers events by webhook, and verify its signatures there.
2. Export an options struct with `yaml` tags and defaults (optionally with a `Validate() error` method), and an address validator.
3. Add one entry to `frontendTypes` in `cmd/router/frontends.go`.

`internal/core`, `internal/router` and `internal/config` need no changes.

## Adding a backend

1. Create a package under `adapters/backend/` that imports only `internal/core`, and implement `core.Backend`:
   - `Capabilities` declares what the router may ask: `Steer` and `FollowUp` for prompts while busy, `Models`, `NewSession`. The router holds prompts itself when neither delivery mode is available and refuses commands for missing features; runtime gaps return `core.ErrUnsupported`.
   - `Run` supervises the agent and emits events in order: `EventReady` when prompts are accepted, `EventTurnStarted` / `EventTurnEnded` around work, `EventMessageChunk` for reply text, `EventToolCall`, `EventDialog`, `EventNotice`, `EventError`, `EventRestarting`. Anything else goes out as `EventExtension` with the record in `Raw`.
   - `Prompt` returns once the prompt is accepted; it must not wait for the turn.
   - Register `core.ToolServer` (URL, token and route header) with the agent so it can call `send_file` and `send_voice`.
2. Export an options struct with `yaml` tags and defaults (optionally with a `Validate() error` method).
3. Add one entry to `backendTypes` in `cmd/router/backends.go`. It receives the instance name, a private state directory, the tool server and the media store.

## Reliability and limits

- **Supervision**: backend process exits trigger exponential restart backoff (1–60 s, reset after a minute of stable runtime).
- **Sessions**: pi's active session pointer is captured on startup, agent start/settle, `/new` and `/restart`, then atomically persisted to `state.dir/<name>/session`. A failed pi restore is surfaced and retried, never silently replaced with a new conversation; an explicit `session_file` overrides the pointer. ACP sessions are resumed with `session/load`; if the agent cannot load it, a new session starts and the failure is logged.
- **Replay**: messages received during recovery are replayed FIFO within user/plugin priority. Work a backend already accepted is **not** replayed after a crash, because tools may already have run; use `/retry` deliberately.
- **Deadlines**: interactive backend operations have a 20 s caller deadline. A timeout only ends that wait. It does not kill the backend or replay, and a command whose write had started may still execute. A single serialized writer per process keeps line-delimited records intact; late responses can never satisfy another request.
- **Queues**: each backend's recovery queue holds 256 envelopes by default (`queue_limit`) and rejects overflow visibly. Queues are memory-only.
- **Telegram**: 429 responses honor `retry_after` up to three times; ambiguous network failures are logged rather than retried, to avoid duplicates.
- **Shutdown**: SIGTERM stops polling and HTTP admission, flushes buffered bubbles, then closes each backend's stdin with a bounded wait before killing it. Startup and graceful shutdown send a best-effort notice to the owners listed in `lifecycle_notify`.
- **No durability guarantees**: there is no database, durable outbox or exactly-once delivery. A lost network response means an uncertain delivery result. Slow frontend calls apply backpressure to dispatch.
- **Platform rules**: some platforms restrict unsolicited messages (proactive turns, `send_file` outside a conversation). Refusals surface to hooks as `EventDeliveryRefused`; the router never retries them.
- **Non-goals for now**: multiple users per frontend instance, cross-platform identity, config hot reload, outbound plugin subscriptions, web UI.

## Testing

```sh
make test   # go test ./...  (go test -race ./... additionally needs a C compiler)
make vet
```

The suite covers scheduling with fake frontends/backends, stream boundaries, recovery with real JSONL and ACP child processes, capability-based scheduling, Telegram HTTP and dialog flows against a local fake server (opens loopback sockets), auth, MCP delivery, path confinement, config layering and adapter-typed options, multiple frontend instances, reply references, platform refusals, webhook dispatch, and the package dependency rule. No real Telegram or provider credentials are needed. Audio tests use real ffprobe when available and skip otherwise.

Container smoke test, offline and without credentials:

```sh
docker build -t phirouter:local .
docker run --rm --network none --entrypoint node \
  --mount type=bind,source="$PWD/scripts/smoke.mjs",target=/tmp/smoke.mjs,readonly \
  phirouter:local /tmp/smoke.mjs
```

It verifies real pi MCP discovery, bearer auth, path rejection, liveness and graceful shutdown.

## References

[pi RPC](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc.md) · [RPC commands](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc-commands.md) · [extension UI](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc-extension-ui.md) · [pi MCP configuration](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/mcp.md) · [Agent Client Protocol](https://agentclientprotocol.com/protocol/overview) · [Telegram Bot API](https://core.telegram.org/bots/api) · [MCP Streamable HTTP](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports)

## License

[MIT](LICENSE)
