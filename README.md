# opence-fleet

`opence-fleet` runs many [OpenCE](https://github.com/OpenCommunityEdition)
dedicated servers (`halo --server`), on many machines, from one page. A
community's operators make servers from **profiles** (a rotation, settings,
gametypes, a release), see every game live, moderate players across the
fleet (a ban on one is a ban on all), and update or restart servers in
rolling waves that wait for each game's end.

It is one binary with two jobs:

- **`opence-fleet serve`**, the control plane: the pages, the API, SQLite,
  and the hub the agents connect to. It runs anywhere (it is a few tens of
  megabytes of memory), usually in Docker behind Caddy.
- **`opence-fleet agent`**, one per machine that hosts servers (a *node*):
  it connects out to the control plane over a WebSocket (no inbound port),
  receives its desired state, and runs each server as a container of the
  game's server image (or, without Docker, as a child process of the
  release's zip). It keeps them running when the control plane is down,
  and when it updates itself.

The servers' own HTTP API and RCON (the game's `docs/dedicated_server.md`)
are what the fleet drives: it never patches the game's files or memory.
The plan it implements is `dedicated_server_plan/OPENCE_FLEET_PLAN.md`.

One static Go binary (no CGO), one SQLite file. Licence: AGPL-3.0-or-later
(`LICENSE`): the fleet is a network service, so its users are owed its source.

## Installing

On a Linux machine with a domain whose DNS record points at it:

```sh
curl -fsSL https://github.com/OpenCommunityEdition/opence-fleet/releases/latest/download/install.sh | sh
```

It asks how HTTPS reaches the fleet (Caddy in Docker gets the certificate,
or a reverse proxy of your own forwards to `127.0.0.1:8070`), for the
fleet's domain, and whether this machine also runs servers. It installs
Docker if it is missing (it asks first), sets the control plane up in
`/opt/opence-fleet`, starts it, and prints a **setup link**: open it to make
the fleet's owner. When the machine runs servers, it is enrolled as the
fleet's first node in the same run (the agent on the host under systemd, so
that the games run as containers beside the control plane's, never inside
it). Every question is also an option (`--domain`, `--proxy`, `--port`,
`--servers`, `--node`, `--dir`, `--yes`; `install.sh --help`). Run it
again to repair or switch an install: the data and the enrolment are kept.

### Adding a node

On the **Nodes** page, *Add a node* makes a join token (good for an hour,
once) and the one-liner to run on the machine:

```sh
curl -fsSL https://fleet.example.org/agent.sh | sudo sh -s -- --token <join token>
```

`agent.sh` is served by the control plane itself, with its URL and the
release key filled in. It installs Docker with consent (`--runtime process`
installs the 32-bit C runtime instead), makes the `opence-fleet` user and
`/var/lib/opence-fleet`, installs the agent (checked against the release's
signed `SHA256SUMS`), enrols the node (the node token goes to
`/etc/opence-fleet/agent.env`, mode 0600) and starts
`opence-fleet-agent.service`. Then open the node's tunnel ports to the
internet (UDP, 2302-2399 by default: `ufw allow 2302:2399/udp`). Docker
publishes ports with its own iptables rules, which bypass `ufw`'s input
chain; a server's web port is bound to the node's loopback regardless.

A node that runs servers is amd64 (the game is 32-bit x86). The port
ranges, the public address, the limits per container (600 MB and one core
by default) and the loopback base of the process runtime are node settings
on the node's page.

### Without Docker

`deploy/opence-fleet.service` runs the control plane under systemd
(`DynamicUser`, its state in `/var/lib/opence-fleet/control`, settings in
`/etc/opence-fleet/fleet.env`), behind a proxy of your own; its header says
how. Such a machine runs servers through an agent with the process runtime
(`agent.sh --runtime process`).

## Running a fleet

The pages, by what they are for:

| Page | What it does |
| --- | --- |
| Dashboard | the fleet's tiles (servers up, players, nodes, alerts), the live feed, say to every server |
| Servers | every server with its state, game and players, live; filters; bulk actions (start, stop, restart, drain, update, say, set a rotation, apply a profile, groups) as jobs |
| A server | overview, players (kick, ban, tell, mute), rotation, gametypes, the console (the owner's, or the RCON's), settings (live, or *restart needed*), files (`debug.txt`, `stderr.txt`, `bans.txt`), history |
| Players | everyone playing across the fleet, searchable; ban on sight |
| Nodes | machines, their health and versions, *Add a node*, their settings, forget |
| Profiles | rotations, settings, gametypes, release channel, password policy, publisher key: a change reaches every server of the profile live (or marks it *restart needed*) |
| Library | maps, custom maps and gametype files the nodes sync (uploads are chunked and resumable, checked by SHA-256) |
| Releases | the server image's builds (`latest` resolved from the registry), local builds, and the release zips of the process runtime |
| Masters | the master servers, the fleet's publisher keys and what each master says of them |
| Bans | fleet bans (by address, hardware id or name, with an expiry), imported or applied everywhere |
| Jobs, Schedules | bulk actions and their waves; cron schedules of them (in the fleet's time zone) |
| Alerts | crashes, crash loops, nodes offline, probes failing, slow frames, a disk filling; Discord and generic webhooks |
| Audit, Team, Settings | who did what; users, roles (viewer, moderator, admin, owner), invites and API tokens; the fleet's settings |

Everything a page does is also `/api/v1` ([docs/api.md](docs/api.md)), with
an API token from the Team page, and the command line:

```sh
export FLEET_URL=https://fleet.example.org FLEET_TOKEN=ocf_...
opence-fleet servers
opence-fleet restart btb-eu-1 --drain
opence-fleet update --group eu --after game
opence-fleet say --group eu "restart at 04:00"
opence-fleet export > fleet.json
```

### Options

Every option is an environment variable and a flag; the flag wins. A
setting the Settings page also has (the name, the time zone, retention) is
the page's unless the variable is set, when the page shows it as pinned.

`opence-fleet serve`:

| Variable | Flag | Default | What |
| --- | --- | --- | --- |
| `FLEET_NAME` | `--name` | — | the fleet's name, shown on its pages (else set on the Settings page) |
| `FLEET_PUBLIC_URL` | `--public-url` | — | the fleet's public HTTPS origin (https://fleet.example.org), for the agents' one-liner and links |
| `FLEET_LISTEN` | `--listen` | `127.0.0.1:8070` | the HTTP listener's address |
| `FLEET_DB` | `--db` | — | the SQLite database file (default: fleet.db in FLEET_DATA) |
| `FLEET_DATA` | `--data` | `fleet-data` | the folder of the fleet's files: the library's blobs, the releases' zips, fleet.key, bin/ |
| `FLEET_SECRET_KEY` | `--secret-key` | — | the key that encrypts secrets at rest (made if missing; default: fleet.key in FLEET_DATA) |
| `FLEET_TRUSTED_PROXIES` | `--trusted-proxies` | — | reverse proxies (addresses or CIDRs, commas) whose X-Forwarded-* are believed |
| `FLEET_TLS_CERT` | `--tls-cert` | — | a certificate file, to serve HTTPS directly |
| `FLEET_TLS_KEY` | `--tls-key` | — | the certificate's key file |
| `FLEET_MASTERS_FILE` | `--masters-file` | — | a masters.txt naming the master servers the Masters page asks (default: the official master) |
| `FLEET_IMAGE` | `--image` | `ghcr.io/opencommunityedition/opence-server` | the dedicated server's image (or a mirror of it) |
| `FLEET_RELEASE_REPOSITORY` | `--release-repository` | `OpenCommunityEdition/OpenCE` | the game's GitHub repository, whose releases' halo-linux-release.zip the process runtime runs |
| `FLEET_RELEASE_CHECK_INTERVAL` | `--release-check-interval` | `6h` | how often latest is resolved |
| `FLEET_EVENTS_DAYS` | `--events-days` | `30` | days the servers' events and games are kept |
| `FLEET_SAMPLES_DAYS` | `--samples-days` | `7` | days the players' samples (the sparklines) are kept |
| `FLEET_LOG_ADDRESSES` | `--log-addresses` | `true` | the servers log and show players' addresses and hardware ids to admins (HALO_SERVER_LOG_ADDRESSES), which fleet bans need |
| `FLEET_PUBLIC_STATUS` | `--public-status` | `false` | serve a public, read-only /status page of the servers |
| `FLEET_METRICS` | `--metrics` | `true` | serve /metrics (Prometheus) without a token |
| `FLEET_LOG_LEVEL` | `--log-level` | `info` | debug, info, warn or error |
| `FLEET_UPDATES` | `--updates` | `notify` | notify (tell the owners of a new release; they update with a button), auto (install it at once) or off |
| `FLEET_UPDATE_URL` | `--update-url` | `https://api.github.com/repos/OpenCommunityEdition/opence-fleet/releases/latest` | the fleet's latest release, on GitHub's API (a fork's) |
| `FLEET_UPDATE_KEY` | `--update-key` | — | the release key's public key, 64 hex digits (a fork's; default the project's) |
| `FLEET_TIMEZONE` | `--timezone` | — | the time zone of the schedules and pages (an IANA name; default the machine's) |
| `FLEET_CA_FILE` | `--ca-file` | — | extra certificate authorities (PEM) for the masters, the registry and webhooks |
| `FLEET_SESSION_HOURS` | `--session-hours` | `12` | how long a sign-in lasts |

`opence-fleet agent` (`/etc/opence-fleet/agent.env`, written by the
enrolment):

| Variable | Flag | Default | What |
| --- | --- | --- | --- |
| `FLEET_AGENT_URL` | `--url` | — | the control plane's URL (https://fleet.example.org) |
| `FLEET_AGENT_TOKEN` | `--node-token` | — | the node token (made by the enrolment) |
| `FLEET_AGENT_TOKEN_FILE` | `--node-token-file` | — | a file holding the node token |
| `FLEET_AGENT_DIR` | `--dir` | `/var/lib/opence-fleet` | the node folder: data/, instances/, releases/, the state file |
| `FLEET_AGENT_NAME` | `--name` | — | the node's name (default: the host name) |
| `FLEET_AGENT_RUNTIME` | `--runtime` | `docker` | docker (each server a container of the server image) or process (child processes of the release) |
| `FLEET_AGENT_DOCKER_HOST` | `--docker-host` | `unix:///var/run/docker.sock` | the Docker Engine API's socket |
| `FLEET_AGENT_DATA_ROOT` | `--data-root` | — | an existing game data folder (maps/, custom_maps/, gametypes/) instead of the synced library |
| `FLEET_AGENT_LOG_LEVEL` | `--log-level` | `info` | debug, info, warn or error |
| `FLEET_AGENT_CA_FILE` | `--ca-file` | — | extra certificate authorities (PEM) for the control plane and the registry |

The command line's client:

| Variable | Flag | Default | What |
| --- | --- | --- | --- |
| `FLEET_URL` | `--url` | `http://127.0.0.1:8070` | the control plane's URL |
| `FLEET_TOKEN` | `--token` | — | an API token (made on the Team page) |

### Backups

`fleet-data` (the Docker volume, or `FLEET_DATA`) holds `fleet.db` and
`fleet.key`, which encrypts the secrets in the database (the servers' web
passwords, the publisher keys, the webhooks): back both up together. The
library's blobs and the releases' zips are there too, and can be fetched
again. `opence-fleet export --keys` writes the fleet's configuration as
JSON (the profiles, groups, bans, schedules and settings, and with
`--keys` the publisher keys' seeds); `opence-fleet import` reads it into a
new fleet, whose servers are then made from the profiles.

A node keeps nothing that cannot be made again: its state file, the synced
library and each server's folder (`instances/<slug>`: its `config.toml`,
logs and saves). A forgotten server's folder is kept aside for 7 days.

### Updating

The fleet checks its releases every `FLEET_RELEASE_CHECK_INTERVAL` and, with
`FLEET_UPDATES=notify`, shows the owners a button; `auto` installs at once.
An update is a signed release (`SHA256SUMS` signed with the release key);
the control plane restarts into it with its listener handed over, and
offers the same version to every agent, which updates itself without
stopping its servers (containers are Docker's; the process runtime hands its
children's pipes to the new agent). In Docker, `docker compose pull && docker
compose up -d` works too.

The game's servers are updated from the Releases page or `opence-fleet
update`: a rolling job, a wave at a time, each server drained (after its
game, when it is empty, or now) and started on the new build, with a
rollback when one does not come back up.

### What is kept, and for how long

The servers' events and games: `FLEET_EVENTS_DAYS` (30). Player-count
samples (the sparklines): `FLEET_SAMPLES_DAYS` (7). The audit: for good.
Players' addresses and hardware ids are shown to admins and owners alone,
and kept with the events for fleet bans; `FLEET_LOG_ADDRESSES=false`
keeps them out of the servers' logs and the fleet.

## Contents

| Path | What |
| --- | --- |
| `cmd/opence-fleet` | the binary: serve, agent, the control plane's commands, the CLI client |
| `cmd/release-sign` | signs a release's `SHA256SUMS` (the master's) |
| `internal/config` | every option: variable, flag, help |
| `internal/store` | SQLite: migrations, queries (sqlc, generated into `db/`), backups, retention |
| `internal/core` | the rules: nodes, instances, profiles, allocation, jobs, bans, schedules, alerts, audit |
| `internal/spec` | the spec shared by both sides; rendering `config.toml`, `init.txt`, gametypes, the environment (golden tests) |
| `internal/hub`, `internal/agentproto` | the agents' WebSocket and its messages ([docs/agent_protocol.md](docs/agent_protocol.md)) |
| `internal/live` | the live state the pages stream |
| `internal/agent` | the agent: enrolment, reconcile, supervisor (the exit codes' policy), sync, self-update |
| `internal/agent/runtime/{container,process}` | the two runtimes |
| `internal/dockerapi` | a thin Docker Engine API client over the socket |
| `internal/serverapi` | the game server's HTTP API and RCON client |
| `internal/master` | the master protocol's client side (keys, signed `GET /v1/me`), tested against the master's vectors |
| `internal/library`, `internal/releases`, `internal/update` | content, the server image's builds, the fleet's own updates |
| `internal/auth`, `internal/crypt`, `internal/httpx`, `internal/limit`, `internal/netaddr` | users and sessions, secrets at rest, proxies and errors, rate limits |
| `internal/web`, `internal/view`, `static` | the pages (templ, Tailwind, htmx with server-sent events), the API, the console's WebSocket |
| `tools/fakehalo` | the server without the game, for the tests (and its image: `tools/fakehalo/Dockerfile`) |
| `tests/fleet`, `tests/conformance`, `tests/browser`, `tests/e2e` | a whole fleet in one test; fakehalo against the game's simulator; the pages in Chromium; the real game |
| `deploy/`, `install.sh`, `agent.sh`, `compose*.yaml`, `Dockerfile` | deployment |

## Building and testing

```sh
go build ./cmd/opence-fleet
go test -race ./...
sh tools/generate.sh          # sqlc, templ and Tailwind's output, committed
```

The generated code is committed, so a build needs Go alone. More tests,
each skipped without what it needs:

```sh
# the container runtime on this machine's Docker, with fakehalo's image
docker build -f tools/fakehalo/Dockerfile -t fakehalo-server:test .
FLEET_TEST_DOCKER=1 go test ./tests/fleet ./internal/dockerapi

# fakehalo against the game's own protocol simulator
OPENCE_SERVER_SIM=../halo/tools/server_sim.py go test ./tests/conformance

# the pages in Chromium
(cd tests/browser && npm ci && npx playwright install chromium)
FLEET_BIN=$PWD/opence-fleet FAKEHALO_BIN=$(go env GOPATH)/bin/fakehalo node tests/browser/smoke.mjs

# the real game, in its server image, with the retail maps
OPENCE_E2E_IMAGE=opence-server:local OPENCE_MAPS=/path/to/halo go test ./tests/e2e
```

For a fleet on your machine: `FLEET_DATA=dev go run ./cmd/opence-fleet serve`
(it prints the setup link), `go run ./cmd/opence-fleet join-token --runtime
process home`, and the agent with `--join`; `tools/fakehalo --make-release
fake.zip` and `opence-fleet release-zip --latest build-900 fake.zip` give it a
release that needs no game.

## Dependencies

| Module | Licence | For |
| --- | --- | --- |
| `github.com/a-h/templ` | MIT | the pages |
| `modernc.org/sqlite` | BSD-3-Clause | SQLite without CGO |
| `github.com/gorilla/websocket` | BSD-2-Clause | the agents' connection, the console, the RCON |
| `golang.org/x/crypto` | BSD-3-Clause | argon2id, XChaCha20-Poly1305 |
| `github.com/BurntSushi/toml` | MIT | `config.toml` and gametype files |
| `github.com/robfig/cron/v3` | MIT | schedules |
| htmx 2.0.7, htmx-ext-sse 2.2.3 (`static/vendor`) | 0BSD | the pages' liveness |
| Lucide (`static/icons.svg`) | ISC | icons |
| Tailwind CSS standalone CLI, sqlc (development only) | MIT | the stylesheet, the queries' code |
| Playwright (tests only) | Apache-2.0 | the browser smoke test |

## Licence

AGPL-3.0-or-later. See `LICENSE`.
