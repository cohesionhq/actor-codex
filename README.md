# actor-codex

Offers [Codex](https://developers.openai.com/codex) to Cohesion as actors,
one per directory, and runs the sessions Cohesion hands them through
[`codex app-server`](https://developers.openai.com/codex/app-server). Each
session is one Codex thread in its directory: what the user says in Cohesion
goes in as the thread's input, and Codex's words, tool uses, questions,
approval requests and plans come back as the session's transcript, with its
status and progress. See `docs/actors.md` in cohesion for the protocol,
which this client speaks itself (`internal/cohesion`, a Go port of
[cohesion-ts](../cohesion-ts)).

It is the Codex counterpart of
[actor-agentsdk](../actor-agentsdk), and works the same way
wherever Codex allows. It runs as the user who runs it, with that user's
Codex sign-in and configuration (`~/.codex/config.toml`, `AGENTS.md`), and
needs `codex` on the `PATH` (or `$COHESION_ACTOR_CODEX_BINARY`).

## Set up

```sh
go build -o actor-codex .
# On the machine running cohesiond (its socket, or the default one):
./actor-codex setup --local /tmp/cohesion.sock --name "My laptop"
# Or from another device, with a code from `cohesion pair`:
./actor-codex setup <setup-code> --name "My laptop"

./actor-codex add ~/src/website  # offered as "website"
./actor-codex add ~/src/api-server --as api
./actor-codex run
```

The configuration lives in `~/.config/actor-codex` (or `--config <dir>`,
or `$COHESION_ACTOR_CODEX_CONFIG`): `config.json` holds how to reach cohesiond,
the client's key, and the directories offered, readable only by its owner;
`sessions.json` maps each actor session to its Codex thread. `add` and
`remove` take effect on a running client within a second.

## What each actor offers

- **Modes**, each a Codex approval policy and sandbox:
  - Auto (`auto`, the default): `on-request` in the `workspace-write`
    sandbox. It edits and runs commands in its directory, and asks before
    going outside it or online.
  - Read only (`readOnly`): `on-request` in the `read-only` sandbox.
  - Plan (`plan`): Codex's plan collaboration mode, read-only.
  - Full access (`fullAccess`): `never` in `danger-full-access`. It runs
    everything without asking.
- **Models**, as Codex lists them for the account at startup (`model/list`,
  hidden ones left out, Codex's default first), each with its reasoning
  efforts.

## How a session runs

- **Start**: a new Codex thread (`thread/start`) with the session's mode,
  model and the note below, kept in `sessions.json` once Codex names it.
- **Messages**: each starts a turn (`turn/start`) at the session's effort.
  One sent mid-turn joins it (`turn/steer`). A thread's process exits after
  ten minutes with nothing to do, and the next message resumes the thread
  (`thread/resume`), as does the first one after a restart. A session this
  client has no record of starts a new thread from Cohesion's transcript.
  One whose thread Codex can no longer resume carries on in a new thread,
  with a note in the transcript.
- **Requests**: Codex's approvals become `permission` requests. A command
  approval is a `command`, a file change approval a `fileChange` with a
  unified diff of each file, and a network or permissions approval a `tool`.
  `requestUserInput` becomes a `question`, and an MCP server's form
  (elicitation) a `question` of its fields. The session needs attention until
  the user responds:
  - Allow is `accept` and Allow for session is `acceptForSession`. Codex
    keeps a session approval for the thread, never in a config file.
  - Deny is `decline`; a note given with it follows as the user's words.
  - Abort is `cancel`, which ends the turn.
  - Anything the user says instead declines every open request, and Codex
    hears the words.
- **Plans**: in Plan mode, the plan Codex writes is shown as a `plan`
  request when its turn ends. Approved, the session moves to Auto and Codex
  implements it in the same session. Not approved, Codex hears why and plans
  again.
- **Instructions**: Codex's own, plus a note (`internal/actor/instructions.go`,
  sent as developer instructions) that Cohesion renders `mermaid` code
  blocks as diagrams, to use where one helps explain a flow, architecture,
  sequence or state.
- **Images**: the user's are saved owner-only under the system's temp
  directory (`actor-codex/<session>/`) and go in with what they said as
  `localImage` input. The ones Codex shows come back under their tool use,
  reported once it completes: an image it viewed (`imageView`), one it drew
  (`imageGeneration`) or an MCP tool's (a screenshot, say). One cohesiond
  would refuse (over 10, over 5 MB encoded, not PNG, JPEG, GIF or WebP) is
  left out, with a note in the transcript.
- **Tool uses**: each of Codex's items is reported as it starts and ended
  (`end`) once it completes, failed or not, so Cohesion times it. An
  MCP call, a web search or an image is reported once it completes,
  ended as it is shown. Each is named and classified by what it does:

  | Codex item | Tool | Category |
  | --- | --- | --- |
  | Command that only reads files | `Read` | `read` |
  | Command that only lists files | `List` | `read` |
  | Command that only searches | `Search` | `search` |
  | Any other command | `Shell` | `command` |
  | File change | `Edit` | `edit` |
  | Web search or page | `WebSearch` | `web` |
  | Image viewed | `ViewImage` | `read` |
  | Subagent call | `Agent` | `agent` |
  | MCP or dynamic tool | e.g. "Github: search issues" | `other` |
  | To-do list update | `UpdatePlan` | `other` |

  A command shows without the `bash -lc '…'` Codex runs it in.
- **Mode**: the user can switch it at any time. The next turn takes it: one
  under way keeps the sandbox it started with, since Codex sets a thread's
  sandbox as it loads it. A switch between modes with different sandboxes
  reloads the thread (`thread/resume`) before the next turn.
- **Status**: working while Codex is at it, idle at the end of each turn,
  failed when Codex itself fails (a failed turn, or its process exiting),
  not when, say, tests it ran fail. A turn isn't over while subagents Codex
  started are running in their own threads. The session stays working, its
  progress "Waiting on a background agent", until Codex takes their results
  up in a turn and ends it, or ten seconds after the last is done should no
  turn follow. Stop interrupts the turn and the subagents' turns.
- **Title**: a session Cohesion titled with its prompt's first line takes
  Codex's name for the thread as a `title` report once Codex has one, and
  again whenever it changes: from `thread/name/updated`, and read after each
  turn. A session started with a title keeps it, and the thread is named after
  it (`thread/name/set`). A session picked up from a thread no longer available
  keeps its title.
- **Away from cohesiond**: the thread keeps working. Its updates are held
  and sent in order once it reconnects, after reporting each running session's
  status; a message that may have been stored before the session dropped
  is checked against the transcript rather than sent twice.

## Usage

Each session tells cohesiond how full its context window is, and each actor
how much of its plan it has used, whenever either changes:

- **Context**: a session's own, as a `context` report, from each
  `thread/tokenUsage/updated`: the last request's tokens out of the model's
  context window.
- **Plan**: the ChatGPT account's Codex rate limits (`account/rateLimits/read`,
  and each `account/rateLimits/updated` during a turn), shared by every
  actor. Each window is labelled by its length ("5-hour limit", "Weekly
  limit"), and a model's own limit carries its name. They name the account
  as their `pool` (`codex:` and its email, within its workspace when it has
  one), so cohesiond shows one row for the account across directories and
  devices. They are read at startup, every five minutes, as each thread
  starts and after each turn. An API-key, Bedrock or GCP sign-in has none,
  so no plan is said.

What Codex can't say is left out, and a cohesiond that refuses usage (an
older one) is ignored: usage never fails a session.

## Checks

```sh
go vet ./...
go test -race ./...
```

CI (`.github/workflows/test.yml`) runs these and `gofmt` on every push to
`master`. `internal/codex`'s tests run the JSON-RPC client against the test
binary standing in for `codex app-server`.

`TestLiveCodex` runs one turn on a real Codex, skipped unless
`COHESION_ACTOR_CODEX_LIVE` names the binary:

```sh
COHESION_ACTOR_CODEX_LIVE=$(which codex) go test -run TestLiveCodex -v ./internal/actor/
```

## Licence

Copyright (C) 2026 Jackson Zamorano.

Licensed under the GNU Affero General Public License, version 3 only
(`AGPL-3.0-only`). See [`LICENSE`](LICENSE).
