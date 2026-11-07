# Architecture

How this client is put together. What it does is in `README.md`, and the
protocol it implements is `docs/actors.md` in cohesion (`../cohesion`). It
mirrors `../actor-agentsdk` piece for piece; where Codex differs,
this says so.

## Shape

One process (`actor-codex run`) is one paired Cohesion client. It
offers one actor per configured directory, and runs one `Session` per
actor session. Each session is one Codex thread in its actor's directory,
driven through a `codex app-server` process of its own:

```
cohesiond ⇄ cohesion.Client ⇄ Host ─┬─ Session 12 ─ codex app-server ─ thread 01a1…
                                    ├─ Session 13 ─ codex app-server ─ thread 01a2…
                                    └─ Usage (rate limits, shared by every actor)
```

An actor session's process (a *stretch*) exits after ten idle minutes (`Linger`), and
the next message starts a new one that resumes the thread. `sessions.json`
keeps the session → thread mapping so a restart resumes too.

## Packages

| Path | Role |
| --- | --- |
| `main.go` | The CLI: `setup`, `add`, `remove`, `list`, `run`. `run` builds `Usage`, `Host` and the `Client`, and re-offers when `config.json` changes. |
| `internal/cohesion` | The Cohesion protocol, ported from cohesion-ts: wire types (`types.go`), pairing, local identity and pinned TLS (`server.go`), the `/connect` WebSocket (`websocket.go`), one `Session`, and the reconnecting `Client`, whose report queue holds updates while offline and resends them in order, checking the transcript before resending a message. |
| `internal/codex` | `codex app-server` over stdio: JSONL JSON-RPC without the `jsonrpc` field, requests matched by id, notifications and server requests handed to a `Handler`, and `initialize` (experimental API on, streamed deltas opted out). |
| `internal/actor/host.go` | `Host.OnCall` answers cohesiond's four calls; it opens or picks up the `Session` for each (from `sessions.json`, or from Cohesion's transcript) and resyncs running sessions on reconnect. |
| `internal/actor/session.go` | `Session`: one thread. Everything it does runs on its own event loop (`queue`), one event at a time: calls from the host, Codex's notifications and requests, timers. `open` starts a stretch; `notify` turns items and turns into reports; `request` turns approvals into requests and waits on the user (`waiting`). |
| `internal/actor/translate.go` | Pure functions between Codex and Cohesion's transcript kinds: `ToolUse`, `Progress`, `DisplayName`, `UnwrapShell`, `Request` (approvals, questions, MCP forms), `Response` (the user's response back to Codex), `Describe`. |
| `internal/actor/offers.go` | The modes (`Modes`: approval policy, sandbox, collaboration mode) and models (`AvailableModels`, `ActorModels`). |
| `internal/actor/usage.go` | Context usage per session (`ContextOf`) and `Usage`: the account's rate limits, pulled and taken from updates, said for every actor. |
| `internal/actor/images.go` | Image limits and conversion (`Sendable`, `ImageFile`, `McpImages` in session.go). |
| `internal/actor/config.go`, `store.go` | `config.json` (server, key, actors) and `sessions.json` (`Record` per session), owner-only, written whole then renamed. |

## Flows

- **In**: cohesiond call → `Host.OnCall` → `Session.Say` / `Session.Hear` (pushed
  onto the session's loop) → `turn/start`, or `turn/steer` mid-turn.
- **Out**: Codex notification → `Session.notify` → `translate` → `Session.report`
  → `cohesion.Client` (queued in order; held while offline).
- **Requests**: Codex server request → `Session.request` → `translate.Request`
  reported as a message, and the session reports `needsAttention` → the user's
  response arrives through `Hear` → `translate.Response` → `Respond`.
  Plans are the exception: Codex doesn't ask, so a `plan` item in a Plan
  mode turn is shown as a request when the turn completes, and the answer
  starts the next turn.
- **Status**: `working` while a turn runs, `needsAttention` while a request
  waits, `idle` at each completed turn with no subagent running
  (`background`, from `subAgentActivity` and `collabAgentToolCall` items;
  otherwise `held` working until Codex's next turn ends, or `Resume`
  passes with none), and `failed` only when Codex itself fails (a failed
  turn, or its process exiting). A stopped turn is `idle` without a
  report, as cohesiond already set it. Never report what cohesiond sets
  itself; the rules are in `docs/actors.md`, "Statuses".
- **Resync**: `Session.Resync` reads a locked snapshot (status, progress,
  title), never the loop, which may be blocked on a report waiting for the
  very session that is resyncing.

## Dependencies

- `github.com/coder/websocket` for the `/connect` WebSocket; everything else
  is the standard library. TLS checks cohesiond's key against the paired
  fingerprint before anything is sent, as cohesion-ts does.
- `codex` itself, run per stretch. The protocol moves fast:
  `codex app-server generate-json-schema --out DIR` (or `generate-ts
  --experimental`) gives the exact schema for the Codex installed.
- A protocol change starts in cohesion, then cohesion-ts, then here
  (`internal/cohesion/types.go` mirrors cohesion-ts's `frames.ts` and
  `generated.ts`).

## Where to look

- A tool shown wrong, or a wrong category: `translate.go`, `translate_test.go`.
- A status, a tool end, a missing message: `Session.notify` / `completed` /
  `turnCompleted` in `session.go`, and `session_test.go`. It runs the test binary
  as a scripted fake `codex app-server`.
- An approval, question or plan flow: `Session.request` / `hear` / `askPlan`,
  `translate.Request` / `Response`.
- Resume and restart behaviour: `Host.hear` / `open`, `Session.open`, `store.go`.
- Reconnects, ordering and resends: `internal/cohesion/client.go`, and
  `client_test.go`, which runs a fake cohesiond on a Unix socket.
- Usage numbers: `usage.go`, `usage_test.go`.
