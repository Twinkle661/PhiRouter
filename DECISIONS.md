# Implementation decisions

Date: 2026-11-20. These are implementation judgments made where the PRD left details open, or where the upstream protocol requires a more precise interpretation.

| Area | Decision and reason | Consequence |
| --- | --- | --- |
| pi compatibility | Pin the runtime to `@earendil-works/pi-coding-agent@1.6.0` and implement its documented RPC commands. | Upgrades require a protocol review; the package was verified against the npm registry and upstream docs. |
| Idle detection | Flush at `message_end` and `agent_end`, but become idle only on `agent_settled`. | pi can continue queued work or retries after `agent_end`; new messages still steer/follow up correctly. |
| Busy/idle race | Translate core `DeliveryInterrupt` and `DeliveryQueue` policies to pi `prompt` with `streamingBehavior: steer/followUp`. Inspection of pi 1.6.0 showed standalone queue commands do not start idle runs. | Preserves the required scheduling policy while preventing messages from becoming stranded when a run finishes concurrently. Busy plugin slash commands are rejected, because pi executes extension commands immediately under `prompt`, bypassing follow-up priority. |
| Slash commands | Unknown user slash commands use `prompt` with `streamingBehavior: steer` when busy. | pi rejects extension commands in the standalone `steer` command. This preserves extension behavior. |
| Abort semantics | Clear pi's queue before `abort`, and clear router recovery queues. | `/abort` really stops scheduled continuation instead of immediately starting the next follow-up. |
| Retry semantics | Retry the last successfully accepted user envelope, excluding router commands and plugin injections. | Attachments are retained by path; an expired attachment produces a visible error. It creates another turn rather than rewriting session history. |
| Session persistence | Store a small atomic session-pointer file under `~/.pi/agent`, per backend. | Recovery also works after restarting the router. An explicit `session_file` overrides the pointer on startup. |
| Restore failure | Retry and surface the error; never silently discard the session pointer. | A missing/corrupt session must be repaired or its pointer removed by the operator. |
| In-flight crash | Do not replay prompts already acknowledged by pi. | Replaying could repeat tool side effects. Only router-owned recovery queues are replayed automatically. |
| Queue capacity | Bound each recovery queue at 256 messages by default; bound input/event channels. | Exhaustion produces a visible rejection / HTTP 503 instead of unbounded memory growth. Queues are not durable. |
| Return routing | Each backend tracks the last authorized source; configured exact sources seed it; wildcard routes initially use the bound user's private chat. | Scheduled messages work before any prompt, provided the user has first contacted the bot. |
| Multiple backends | Use one shared MCP entry with a per-child `PHIROUTER_BACKEND` environment value expanded into `X-PhiRouter-Route`. | Multiple pi instances can return files through one loopback endpoint without importing adapters into core. |
| Adapter contracts | Core exposes typed backend methods and results, envelopes, dialogs, and scheduling policies (`DeliveryStart`, `DeliveryInterrupt`, `DeliveryQueue`). The earlier string/map `Command` contract was removed after review. | Wire commands, response decoding, and transport stay inside the adapter; no magic `envelope` key crosses core. Factories and supported type validation change at composition time. |
| RPC deadlines (review fix) | A command timeout only ends the caller's wait and removes its pending response entry. A single per-process writer serializes JSONL and allows callers waiting for stdin to time out independently. No context callback closes stdin or kills pi. | An already-started write may finish and the operation may execute after timeout; no automatic replay. Unsent expired requests are skipped. Process termination belongs to explicit restart, shutdown, or supervision of actual transport/initialization failures. |
| Startup restore deadline (review fix) | Session restoration uses the lifecycle context, not a fixed 30-second command deadline. | Slow `switch_session`/extension dialogs no longer cause automatic process kills due solely to elapsed time. Shutdown still cancels restoration. |
| Tool presentation (review fix) | The pi adapter normalizes tool start/end metadata; router formats tool name, success/failure, and duration. It never posts incremental tool JSON. | At most a start and an end summary per normal tool execution. Hooks retain all raw events; arguments/results do not fill the chat. |
| Media contracts (review cleanup) | Use `io.Reader` and `io.ReadCloser`; define typed `PartText`, `PartImage`, `PartAudio`, and `PartFile` constants throughout the implementation and tests. | Standard Go interfaces and compile-time type distinctions without changing HTTP/JSON representations. |
| Go dependency | Use only `yaml.v3`; implement the small Bot API surface with `net/http`. | Fewer runtime dependencies and direct control over limits, streaming uploads, and tests. |
| Configuration | Typed environment overrides support whole collections and indexed nested values; unknown paths fail. Parent paths apply before children. | Every field can be overridden; typos are not silently ignored. Map keys in environment paths are lowercase. |
| Dotenv | Implement assignments and quoted values, without shell evaluation/interpolation. | No command execution from config. Provider credentials in a local dotenv must be exported separately; Compose exports them. |
| Media containment | Require absolute regular-file paths and open via `os.Root`; do not trust request MIME/size. | Traversal and outward symlinks are rejected, including open-time symlink races. The shared filesystem remains accessible to the trusted pi process itself. |
| UUIDs and sizes | Generate random RFC 4122 version-4 names; retain only a short basename extension. Limits use binary MiB; display sizes use decimal MB. | Original filenames cannot choose destination paths. |
| Cleanup | Recursively clean files under `in/` and `out/` on startup and every minute using file mtime. | Files are eligible at the TTL and normally removed within one extra minute. A file edited later receives a fresh lifetime. |
| Incoming audio/video | Preserve them as ordinary file attachments and do no content interpretation. | The agent receives path/MIME/size only, as requested by the PRD. |
| Outgoing images | Use `sendDocument` for images too. | This follows the PRD's explicit “all other files use sendDocument” rule and preserves original bytes. |
| Voice probing | Use ffprobe against an already-open confined file descriptor; accept only audio-only supported codecs below the duration limit. | Failed/missing probe or unknown duration falls back to document, never guesses voice suitability. Linux is the deployment target. |
| Text rendering | Send plain text; count UTF-16 units conservatively when enforcing Telegram limits. | No Markdown escaping failures; some non-BMP text splits earlier than a code-point interpretation. |
| Button payloads | Store opaque random callback IDs mapped to full answers; split large menus across messages. | Long model names/options do not exceed Telegram's 64-byte callback limit. Replay is ignored after selection. State is memory-only. |
| Dialog input | Consume replies to the ForceReply message, not arbitrary next messages from unrelated chats. | User text outside the requested reply can still steer the agent. Timed-out dialogs belong to pi. |
| MCP transport | Stateless Streamable HTTP with JSON responses; no server-initiated SSE stream (GET returns 405). | Implements the transport option needed by `send_file` without unnecessary session machinery. Supports protocol negotiation for 2025-03-26, 2025-06-18, and 2025-11-25. |
| MCP registration | Atomically merge/update only the reserved `phirouter` server entry in pi's existing config. | Existing user entries remain intact. Tokens are environment references. The router owns its reserved entry. |
| API exposure | Default injection listener is loopback; Compose binds inside the container but does not publish a host port. | Local operation is private by default; sibling plugin containers can access the Compose network. |
| Health | `/healthz` measures router process liveness, not pi readiness or provider connectivity. | Expected pi recovery does not make Docker restart a healthy supervisor. `/status` exposes backend state. |
| Telegram retries | Retry explicit 429 responses up to three times after `retry_after`; do not retry ambiguous sends. | Avoids knowingly dropping rate-limited sends while reducing duplicate delivery on uncertain network errors. Polling retries exponentially. |
| Hooks | Run ordered, synchronous transformations; isolate error-return mutations by copying envelopes; event observers receive copies. | Trusted hooks must return promptly and inject from separate goroutines. Hook failures only log and do not stop message flow. |
| Built-in examples | Provide optional `prefix` and interval `ticker` plugins; enable none by default. | Demonstrates both transformation and trigger extension points without adding default prompt text. |
| Shutdown | Stop producers and HTTP requests, flush bubbles, then stop pi with a deadline. | Normal shutdown preserves completed buffered text. Unsent work is not durable across a forced kill. |
| Base images | Use Go 1.26 and Node 22 official release-line tags; pin pi exactly. | Security patches to base images are picked up on rebuild; builds are not bit-for-bit reproducible without digest-pinning. |

## Known operational limits

- Telegram and model-provider end-to-end behavior requires real credentials and a pi login. Automated tests use a local fake Telegram server and an actual fake JSONL child process.
- Router transitions are serialized; outbound HTTP calls and RPC acknowledgements apply backpressure. A slow frontend can delay new dispatches; file messages bypass generation completion but do not bypass already executing network calls.
- A lost network response can mean an uncertain delivery result. There is no durable outbox, database, or exactly-once guarantee in this scope.
- Session pointers cannot undo already-run agent tools. `/retry` should be used with awareness of possible earlier tool side effects.
- The currently registered frontend type is Telegram and backend type is pi. Configuring a new adapter requires implementing the core contracts and adding its factory/validation; the core package itself stays unchanged.

## Validation performed

- Standard Go test suite and `go vet` passed.
- The full test suite passed with Go's race detector in the official Go build container.
- The multi-stage Docker image built successfully with pinned pi 1.6.0.
- The runtime smoke check connected the real pi MCP client to PhiRouter and discovered `send_file`, with external networking disabled.
- Real ffmpeg/ffprobe checks verified short OGG/Opus, MP3, and M4A voice selection, long audio/document fallback, and WAV/document fallback.
- A real Telegram account and model-provider generation have not been exercised; these require user credentials.

## Review regression coverage

- A deliberately delayed `new_session` times out while the same child PID remains alive; a subsequent status call receives its own result rather than the late response.
- Blocked stdin and another caller waiting for the writer both honor deadlines; releasing the pipe allows later commands to succeed and never transmits the cancelled waiting command.
- One hundred tool progress events produce no user bubbles; enabled start/end events produce two concise summaries, and the disabled flag produces none.
- Typed model operations, dialog responses, abort queue clearing, scheduling priorities, and session restoration retain protocol coverage.
