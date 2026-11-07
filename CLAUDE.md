# CLAUDE.md

`actor-codex`: a Cohesion actor client, in Go, that runs Codex through
`codex app-server`. Read `README.md` first, then `ARCHITECTURE.md`, then
`docs/actors.md` in cohesion (`../cohesion`), which this implements. It
mirrors `../actor-agentsdk`: a behaviour change there likely belongs
here too, and the other way round.

- `main.go`: the CLI. `internal/cohesion`: the Cohesion protocol (a port of
  cohesion-ts; wire fields snake_case, enum values camelCase, as the daemon
  sends them). `internal/codex`: the app-server's JSON-RPC.
  `internal/actor`: the host, one `Session` per actor session, translation,
  offers and usage.
- An actor session's state belongs to its event loop: change it only from functions
  run on the loop (`t.events.push`), never from another goroutine. What a
  reconnect reads goes through the locked `snap`.
- Follow `docs/actors.md` for every status rule: report `idle` at the end of
  each turn, `failed` only when Codex itself failed, and nothing cohesiond
  already sets itself.
- Never write approvals to Codex's config on the user's behalf;
  allow-for-session is `acceptForSession`, which lasts the thread.
- Codex's protocol drifts between versions: check field names against
  `codex app-server generate-json-schema` for the installed Codex, and
  tolerate unknown fields and methods.
- Tests (`go test ./...`) cover the translation rules and flows a
  regression would quietly break. `gofmt -l .` must be empty, and
  `go vet ./...` and `go test -race ./...` must pass before a change is done.

## Commits

- Commits must NOT include a `Co-Authored-By` (co-author) trailer.
