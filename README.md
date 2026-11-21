# PhiRouter

A Go router connecting pi's JSONL RPC interface to one Telegram user. It forwards text and shared-directory attachments, streams text as separate bubbles, handles pi dialogs, and exposes authenticated injection and file-delivery endpoints.

## Quick start with Docker

Requirements: Docker Compose, a Telegram bot token from BotFather, your numeric Telegram user ID, and a pi-supported model account.

```sh
cp config.example.yaml config.yaml
cp .env.example .env
# Edit .env: set your token, user ID, and TWO different random bearer secrets.
docker compose build
# Open pi's interactive terminal, run /login, select your provider, then /exit.
docker compose run --rm --entrypoint pi router
docker compose up -d
docker compose logs -f router
```

Open a private chat with your bot and send a message. Telegram requires that you contact the bot before it can send you proactive messages. Only updates authored by the configured UID are accepted, including callback queries. That user may also interact from group chats if a route allows them; use an explicit `telegram:<chat_id>` route to constrain the destination.

The pi login, session pointer, extensions, and `mcp.json` survive in the `pi-agent` volume. Files live in the `media` volume. Both services run in one container as the non-root `node` user. Config is mounted read-only. The injection port is available to sibling containers as `http://router:8080`; it is not published to the host. To access it from the host, explicitly add a loopback-only port mapping (`127.0.0.1:8080:8080`). Do not publish it on a public interface. MCP remains bound to `127.0.0.1:8081` inside the container.

If using provider API keys instead of interactive login, put the provider's variable in `.env`; Compose passes its contents into the container. Local `-env` loading applies only to `ROUTER_` configuration variables, so export provider credentials in your shell when running without Docker.

On startup PhiRouter preserves unrelated MCP entries and updates the `phirouter` entry in `~/.pi/agent/mcp.json`. The token is stored as an environment reference, not as plaintext in that file. Agent processes receive their own return-route header through their environment. In pi, use its MCP tools to call `send_file` after writing the file to `/data/media/out/`. Run `pi mcp list` in the running container with `PHIROUTER_BACKEND=pi:default` to inspect the connection:

```sh
docker compose exec -e PHIROUTER_BACKEND=pi:default router pi mcp list
```

The container pins pi **1.6.0**. Its Node base is the Node 22 release line, satisfying pi's Node >=22.19 requirement. Go uses the 1.26 release line. Updating pi should include checking the RPC contract and running the tests.

## Local development

Install Go 1.26.10+, Node 22.19+, pi, and ffprobe:

```sh
npm install -g --ignore-scripts @earendil-works/pi-coding-agent@1.6.0
# Install ffmpeg with your operating system's package manager.
cp config.example.yaml config.yaml
cp .env.example .env
# Edit secrets, and set media.dir to an absolute writable directory.
make test
make vet
make build
./bin/router -config config.yaml -env .env
```

Use `pi` and `/login` once before starting. Backend command arrays are executed directly, without a shell. `-healthcheck http://127.0.0.1:8080/healthz` checks a running router without loading credentials. The health endpoint reports process liveness, including during pi recovery; `/status` reports backend readiness.

## Configuration

Precedence is process environment > `.env` > YAML > defaults. Unknown YAML fields and unknown `ROUTER_` environment paths fail startup. Dotenv supports assignments, comments on separate lines, optional `export`, and single/double quoted values; it does not perform shell expansion or interpolation.

Every field supports typed overrides with double underscores. Arrays/maps may be supplied as YAML/JSON, and existing array elements may be addressed by index:

```sh
export ROUTER_STREAM__SHOW_TOOL_CALLS=true
export ROUTER_STREAM__SEPARATORS='["\n", "<break />"]'
export ROUTER_MEDIA__TTL=48h
export ROUTER_BACKENDS__DEFAULT__COMMAND='["pi", "--mode", "rpc", "--provider", "anthropic"]'
export ROUTER_ROUTES__0__FROM='telegram:123456789'
```

Structured environment overrides are applied before more specific child overrides. Map keys used in individual environment paths are lowercase. YAML maps merge with default map entries; use a whole-map environment value to replace a map completely. Missing `config.yaml` and `.env` are allowed when environment variables provide all required settings.

Enable `timestamp.enabled` to prefix frontend messages inside the router. `timestamp.timezone` defaults to `UTC` and accepts IANA names such as `Europe/London` (including daylight saving changes). `timestamp.format` uses Go's reference time layout, defaulting to `"[2006-01-02 15:04:05 MST] "`; include any brackets, spaces, or newlines in the format itself. For example:

```yaml
timestamp:
  enabled: true
  timezone: Europe/London
  format: "[2006-01-02 15:04:05 MST] "
```

This produces messages such as `[2026-11-21 14:30:00 GMT] Hello`. Environment overrides are `ROUTER_TIMESTAMP__ENABLED`, `ROUTER_TIMESTAMP__TIMEZONE`, and `ROUTER_TIMESTAMP__FORMAT`. The timestamp records when the user sent the message as reported by the frontend (Telegram's message date), so backlog delivered after a restart keeps its original times; it falls back to router acceptance time when the frontend reports none. It is applied once, before queueing and inbound hooks; reconnects and `/retry` preserve it without adding another prefix. Media-only messages receive a leading text part. Router commands, dialog replies, and slash commands forwarded to the backend (for example pi skills or prompt templates) are not prefixed; API/plugin injections and outbound messages are not timestamped. The feature is disabled by default. Invalid timezones and empty formats fail startup when enabled.

The default restart queue holds 256 envelopes per backend and rejects overflow explicitly. It is memory-only. TTL cleanup runs on startup and every minute, recursively under `in/` and `out/`. Image thresholds and Telegram download limits use binary MB (MiB); attachment text sizes use decimal MB. No STT, TTS, document parsing, webhook, or outbound plugin subscription is performed.

Multiple pi backend instances can be declared and routed independently; add distinct source routes and backend names. A backend maintains one conversation/session. Exact routes take precedence over wildcards. Proactive traffic returns to that backend's last authorized source, or its configured exact route; a wildcard without prior inbound traffic defaults to the bound user's private chat. Avoid overlapping routes to the same source when expecting separate conversations. Additional frontend/backend adapter types require registration in `cmd/router` and config validation, with no changes to `core`.

## Commands and dialogs

- `/retry`: resubmit the most recent successfully accepted user envelope, including attachments.
- `/restart`: capture the current session and restart pi.
- `/model`: show available models; `/model provider/model-id` selects directly.
- `/new`: create a session, clear retry history after success, and display the session ID, provider, and model.
- `/abort`: clear queued work and abort the current generation.
- `/status`: display readiness, session ID, provider, model, activity, and queue length.

Unknown slash commands pass through pi's `prompt` command, even while busy, preserving extension-command support. Normal user messages request `steer` while busy; plugin messages request `follow_up`. The pi adapter implements those policies through `prompt.streamingBehavior` so a run ending during dispatch cannot strand a queued message. Plugin slash commands are rejected while busy, because pi executes extension commands immediately even with a streaming policy. Steering follows pi's turn/tool boundaries; it does not kill an in-progress tool. Only `agent_settled` makes a backend idle.

Select and confirm dialogs use inline buttons. Input/editor dialogs use ForceReply: reply to the requested message with your answer. The original message is marked with the selection. Dialog timeouts are owned by pi; expired callbacks are ignored. Model lists larger than a keyboard are split across messages. `notify` is displayed; cosmetic terminal UI events are logged only at debug level.

Text is sent as plain text, not Markdown, to preserve arbitrary model output without Telegram parsing failures. Separators are removed, including when split across RPC events. Empty bubbles are omitted; long bubbles are split conservatively at 4096 UTF-16 units. Completed text is flushed on message/run end and graceful shutdown. Tool events are hidden unless enabled. When enabled, only short start/end summaries are sent (tool name, outcome, and available duration); incremental updates, arguments, and results remain available to hooks but are not posted to Telegram. Telegram 429 responses honor `retry_after` with up to three retries; ambiguous network failures are logged rather than retried automatically, to avoid duplicate messages.

## Injection API

```sh
curl -X POST http://127.0.0.1:8080/v1/inject \
  -H "Authorization: Bearer $ROUTER_API__TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"route":"pi:default","parts":[{"type":"text","text":"Say good morning."}]}'
```

Successful acceptance returns `202`; authentication failure returns `401`, malformed or unsafe input `400`, and unavailable routes/queue capacity `503`. Requests are limited to 1 MiB. Supported parts are `text`, `image`, `audio`, and `file`; non-text parts require an existing absolute path inside the configured media directory. MIME and size are calculated from the file, not trusted from request metadata. No outgoing content is exposed by this API.

## MCP file delivery

The local endpoint `/mcp` implements stateless Streamable HTTP, with `initialize`, `ping`, `tools/list`, and `tools/call`. The only tool is:

```json
{"name":"send_file","arguments":{"path":"/data/media/out/report.pdf","caption":"Report"}}
```

The MCP token is independent from the injection token. Cross-origin requests are rejected. Paths outside the media root and symlinks escaping it are rejected using Go's `os.Root`; files are opened through the confined root. An agent instance's `X-PhiRouter-Route` header determines the return route and is inserted automatically by the registered MCP configuration.

Supported audio codecs in `.ogg`, `.opus`, `.mp3`, and `.m4a` files use `sendVoice` only when ffprobe confirms an audio-only duration strictly below the configured threshold (maximum 60 seconds). Unprobeable audio, longer audio, images, videos, and all other files use `sendDocument`. Voice detection failures fall back to document delivery. Captions are limited to Telegram's 1024 UTF-16 units. Media delivery does not wait for generation to finish.

## Plugins and extension points

`core.Hook` provides ordered inbound/outbound transformations (including drop), and read-only event observation. Errors are logged and the original message continues. Hooks are trusted in-process Go code and should return promptly. They must enqueue asynchronous triggers rather than synchronously call `Inject` from a hook, because the router serializes state transitions. Trigger goroutines receive the `core.Inject` handle and always use plugin priority.

Two optional built-ins demonstrate the interface: `prefix` adds configured text to inbound prompts, and `ticker` injects text at fixed intervals from startup. Neither is enabled by default. Configure them using the commented example. The ticker is an interval source, not a wall-clock calendar scheduler.

Backend adapters implement typed operations (`Submit`, `Status`, `Models`, `SetModel`, `NewSession`, `Abort`, `Respond`) with typed results. `DeliveryStart`, `DeliveryInterrupt`, and `DeliveryQueue` express scheduling intent; pi wire commands and JSON maps stay private to its adapter. `PartType` constants preserve the existing JSON strings, and `MediaStore` uses `io.Reader`/`io.ReadCloser`.

Dependency direction is enforced by a test: adapters, router, and plugins import `core`, never one another. Composition and implementation selection live in `cmd/router`. The only third-party Go dependency is `gopkg.in/yaml.v3`, for typed YAML parsing and strict known-field validation. Telegram, HTTP, JSON-RPC, subprocess management, dotenv parsing, and UUID generation use the standard library. `gopkg.in/check.v1` appears in `go.sum` as YAML's test dependency only.

## Reliability and verification

Pi exits trigger exponential restart backoff (1–60 seconds, reset after a minute of stable runtime). The active session pointer is captured on startup, agent start/settle, new session, and explicit restart, then atomically persisted beside pi's state. Failed session restoration is surfaced and retried rather than silently starting a different conversation. Messages received during recovery are replayed FIFO within user/plugin priority classes. Already accepted pi work is not replayed after a crash because acceptance does not prove whether tools ran; the user can `/retry`.

Interactive backend operations have a 20-second caller deadline. A timeout ends that call and removes its pending response waiter; it does not close pi's stdin, kill pi, or replay the operation. Each pi process has one serialized writer, so callers can time out even while stdin is blocked without interrupting a partially written JSONL record. A command whose write has begun may still execute after timeout; treat its outcome as uncertain. Late responses cannot satisfy a different request. Startup session restoration uses the process lifecycle context instead of a fixed RPC deadline, allowing slow extension dialogs; shutdown still cancels it.

SIGTERM stops polling and HTTP admission, flushes buffered bubbles, then closes pi stdin with a bounded wait before killing the child. Startup and graceful shutdown send a best-effort notice to the configured Telegram user (with a five-second timeout). Forced termination or power loss cannot send a shutdown notice. Errors use visibly labeled system messages when the frontend is available; delivery failures use structured stdout logs. No durable message queue or exactly-once delivery is promised.

`go test ./...` covers fake frontend/backend scheduling, stream boundaries, recovery and real subprocess framing, Telegram HTTP/dialog flows, auth, MCP delivery, path confinement, config layering, and package dependencies. The Telegram tests open local loopback sockets. `go test -race ./...` additionally needs a C compiler. Real Telegram/provider credentials are intentionally not required by the suite. See [DECISIONS.md](DECISIONS.md) for implementation choices and operational limits.

Protocol references: [pi RPC](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc.md), [commands](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc-commands.md), [extension UI](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc-extension-ui.md), [MCP configuration](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/mcp.md), [Telegram Bot API](https://core.telegram.org/bots/api), and [MCP Streamable HTTP](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports).

Container smoke checks (no real credentials or external network):

```sh
docker build -t phirouter:local .
docker run --rm --network none --entrypoint node \
  --mount type=bind,source="$PWD/scripts/smoke.mjs",target=/tmp/smoke.mjs,readonly \
  phirouter:local /tmp/smoke.mjs
```

The smoke script verifies real pi MCP discovery, bearer authentication, path rejection, liveness, and graceful shutdown. Audio tests exercise real ffprobe when ffmpeg is installed; they otherwise report a skip.

Telegram system notices and dialogs use a separate `System` header, a divider, and readable multiline details. Set `telegram.system_prefix` (default `𝑴𝒆𝒐𝒘☕︎`) or `ROUTER_TELEGRAM__SYSTEM_PREFIX` to customize the decoration; an empty string keeps the System label. Agent replies remain unchanged. Session fields unavailable from the backend are shown as `Unavailable`.
