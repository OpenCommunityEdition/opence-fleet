# AGENTS.md

Notes for coding agents (and people) changing this repository. `README.md`
is the user documentation and has the package map ("Contents"); the design
is `dedicated_server_plan/OPENCE_FLEET_PLAN.md` beside this checkout; this
file is what a change has to keep true.

## Commands

```sh
go build ./... && go vet ./... && go test -race ./...
gofmt -l .                                              # must print nothing
sh tools/generate.sh                                    # sqlc, templ, Tailwind: the output is committed
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...   # CI fails on any finding
sh -n install.sh agent.sh && shellcheck -s sh install.sh agent.sh
docker run --rm -v "$PWD:/repo" --workdir /repo rhysd/actionlint:1.7.12

# with Docker: the container runtime against fakehalo's image
docker build -f tools/fakehalo/Dockerfile -t fakehalo-server:test .
FLEET_TEST_DOCKER=1 go test -race ./tests/fleet ./internal/dockerapi
# fakehalo against the game's simulator; the pages in Chromium; the real game
OPENCE_SERVER_SIM=<game>/tools/server_sim.py go test ./tests/conformance
FLEET_BIN=… FAKEHALO_BIN=… node tests/browser/smoke.mjs
OPENCE_E2E_IMAGE=opence-server:local OPENCE_MAPS=<folder with maps/> go test ./tests/e2e
```

Tests touch nothing beyond loopback (the registry, the update check and the
masters are `httptest` servers or point nowhere), the database is a
temporary file, and the game is `tools/fakehalo` unless a test says it
needs the real one (`tests/e2e`).

## The game is the contract

- The server's API, RCON, events, settings, exit codes and `bans.txt` format
  are the game's (`docs/dedicated_server.md`, `tools/server_sim.py` and the
  server's sources in the game's repository). The fleet drives them; it
  never patches the game. A capability the fleet uses (`owner_web`,
  `ban_address`, `stop`, `event_addresses`, `identity`) is used only when the
  server's RCON hello names it, with a fallback for an older server.
- `tools/fakehalo` follows the simulator: `tests/conformance` checks the
  shapes. A change to the fake that the game does not do is a lie the tests
  will believe; change the game (and its simulator) first.
- `internal/spec/settings_table.go` is generated from the game's
  `port_config.c` (live vs restart settings); regenerate it when the game's
  settings change, and keep the golden files in `internal/spec/testdata`.
- `internal/master/testdata/vectors.json` is a byte-identical copy of the
  master's; its test checks that against upstream when the checkout is near.

## Invariants

- **Every mutation goes through `internal/core` with an actor** (`user:…`,
  `token:…`, `agent:…`, `schedule:…`, `auto`, `auto:update` for the game
  servers' automatic updates) and writes an audit row. The
  pages and the API are thin; a rule lives in core once.
- **No secret leaves its place**: web passwords, owner passwords, join and
  node tokens, API tokens, publisher keys' seeds and webhook URLs are never
  in the audit, the logs, a page for a role that may not see them, or
  `spec.json` on a node. Secrets at rest are sealed with `fleet.key`
  (`internal/crypt`, with an AAD naming what each is). Tests grep the audit
  and the log for every secret a fixture made: keep them passing.
- **The agent keeps the games running.** A control plane that is down,
  restarting or updating changes nothing on a node; an agent's self-update
  leaves its containers (and, in the process runtime, its children, by the
  pipe handoff) running; a server whose spec is not known yet is left alone.
  Only the unit's stop (or the spec) stops servers.
- **The spec is sent whole**, never as a diff; the agent converges to it.
  Live settings are applied through the server's API; anything else marks
  *restart needed* and waits for an operator or a job.
- **The exit codes' policy** (docs/agent_protocol.md) is the supervisor's
  `onExit`; its tests are the table. Change both together.
- **Roles**: viewer < moderator < admin < owner. A viewer never sees an
  address, a hardware id, an invite or a secret. Forms need the CSRF token
  and a same-origin `Origin`; plain HTTP is answered on loopback alone.
- **Nothing blocks on a node**: requests to agents have timeouts; a job
  reports *offline* for a node that is not connected rather than waiting.

## Style

- Go: the standard library first; the dependencies are the README's table,
  and a new one needs a reason there (and its licence, AGPL-compatible).
- Comments say why, in the code's voice (short, plain, British spelling
  where it shows); a parenthesis for the aside. Names are words, not
  abbreviations.
- Errors a person reads are sentences that say what to do.
- Pages: templ components in `internal/view/parts`, pages in
  `internal/view/pages`, tokens in `internal/view/style.css` (dark first,
  light by `prefers-color-scheme` or the theme cookie). No inline styles or
  scripts (the CSP forbids them); `static/app.js` holds the islands. In
  templ, a text line starting with `for ` or `if ` is read as a statement:
  write `{ "for ever" }`.
- SQL: queries in `internal/store/queries/*.sql` (sqlc); a schema change is
  a new numbered migration, never an edit of an old one.

## Releases

`git tag vX.Y.Z && git push origin vX.Y.Z` runs `release.yml`: tests, the
binaries for linux amd64, arm64 and armv7, the installers and deployment
files, `SHA256SUMS` signed with `RELEASE_SIGNING_KEY` (its public half is
`internal/update/key.go`'s `ReleaseKey`; `cmd/release-sign` makes and checks
signatures), the image on ghcr.io, and the GitHub release last. Only
`vX.Y.Z` builds update themselves. The agent is the same binary: a control
plane offers its agents its own version.

## The installers

`install.sh` (the control plane, and this machine as the first node) and
`agent.sh` (a node; served by the control plane at `/agent.sh` with
`@FLEET_URL@`, `@FLEET_VERSION@`, `@RELEASE_KEY@`, `@SELF_ARCH@` and
`@SELF_SHA256@` filled in) are POSIX sh, re-runnable, ask on `/dev/tty`, and
take every answer as an option and `--yes`. They verify what they download
(`SHA256SUMS`, and `agent.sh` its signature with OpenSSL 3 when there is
one). The systemd units in `deploy/` match what `agent.sh` writes.
