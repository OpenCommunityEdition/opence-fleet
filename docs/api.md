# The fleet's API (`/api/v1`)

Everything the pages do, as JSON. The handlers call the same `internal/core`
functions as the pages, with the same rules, roles and audit; the code is
`internal/web/api.go`.

## Authentication

- **An API token** (the Team page makes them, `ocf_…`, each with a role no
  higher than its maker's): `Authorization: Bearer ocf_…`. A token needs no
  CSRF token and no `Origin`: a browser cannot send it.
- **A session** (the pages' cookie): a change also needs the session's CSRF
  token in `X-CSRF-Token`, and an `Origin` of the fleet's own.

Over plain HTTP the control plane answers loopback alone (`tls_required`
otherwise), unless `FLEET_INSECURE_HTTP` is on; behind a proxy,
`FLEET_TRUSTED_PROXIES` says whose `X-Forwarded-Proto` is believed.

## Roles

`viewer` < `moderator` < `admin` < `owner`; each can what the ones below
can. Viewers read (no addresses, no secrets); moderators act on players and
say; admins run servers, profiles, nodes, bans, releases and jobs; owners
manage the team, the keys, the library's folders, export and import.

## Answers and errors

A success is `200` with `{"ok": true, …}` (or the object asked). A refusal:

```json
{"ok": false, "error": {"code": "pinned", "message": "server.region is set by the environment"}}
```

| Status | Codes | When |
| --- | --- | --- |
| 400 | `schema`, `invalid`, `hash`, `empty`, and the server's `rejected` | the request is wrong; `rejected` names the refused rotation entries |
| 401 | `denied` | no token or session, or an unknown one |
| 403 | `forbidden`, `csrf`, `origin`, `tls_required` | the role is too low; a session's change without its CSRF token or from another origin; plain HTTP from afar |
| 404 | `not_found` | no such server, node, job… |
| 409 | `taken`, `in_use`, `full`, `finished`, `no_release`, `changed`, `pinned`, … | the state forbids it (a name in use, a profile with servers, a node without a free port, a job that ended, a pinned setting, a player who left) |
| 413 | `too_large` | a body over its limit |
| 429 | `rate_limited` | too many requests (`Retry-After`) |
| 502 | `unreachable`, `registry`, `release`, `download` | the server, the registry or a release did not answer as it should |
| 503 | `offline` | the server's node is not connected |

The game server's own refusals (`pinned`, `rejected`, `changed`, …) are
passed through with its codes, from the passthrough and from the calls that
reach a server.

## Endpoints

| Method and path | Role | What |
| --- | --- | --- |
| `GET /api/v1/me` | viewer | the token's user and role |
| `GET /api/v1/nodes` | viewer | nodes: online, versions, facts, settings, instances |
| `POST /api/v1/nodes/{name}/join-token` | admin | a join token and the one-liner (`{"runtime": "docker"\|"process", "public_address": ""}`) |
| `DELETE /api/v1/nodes/{name}` | admin | forget a node (its token revoked; refused while it has servers) |
| `GET /api/v1/instances` | viewer | every server with its live state (`?group=`) |
| `GET /api/v1/instances/{slug}` | viewer | one server, with its last status |
| `POST /api/v1/instances` | admin | make servers: `{"profile": "BTB", "nodes": ["eu-1"], "count": 3, "pattern": "BTB #{n}", "groups": ["eu"], "start": true, "channel": ""}` → `{"created": [slugs], "warnings": […]}` |
| `DELETE /api/v1/instances/{slug}` | admin | remove a server (its folder kept aside 7 days on the node) |
| `POST /api/v1/instances/{slug}/start\|stop\|restart` | admin | lifecycle |
| `POST /api/v1/instances/{slug}/drain` | admin | `{"after": "game"\|"empty"\|"now", "notice": "…", "deadline": "30m", "restart": true}` |
| `POST /api/v1/instances/{slug}/command` | moderator | `{"line": "sv_status"}` → `{"lines": […]}` (the owner's console for an admin, the RCON's role otherwise) |
| `POST /api/v1/instances/{slug}/say` | moderator | `{"text": "…"}` |
| `ANY /api/v1/instances/{slug}/server/{path}` | viewer (GET), moderator (players, say, rotation next/play, `vpn/check`), admin (the rest; bans and settings always) | the server's own API (`/api/{path}`), relayed through its node's agent (`vpn`: its VPN policy and lists, with the `vpn` capability) |
| `GET /api/v1/instances/{slug}/log` | viewer | the server's log lines (`?after=&limit=`) |
| `GET /api/v1/instances/{slug}/events` | viewer | its events as the fleet kept them (players' addresses and hardware ids for admins, their Discord accounts for moderators) |
| `GET /api/v1/instances/{slug}/games` | viewer | its games: map, gametype, players, scores |
| `GET /api/v1/profiles` | viewer | profiles |
| `POST /api/v1/profiles`, `PUT /api/v1/profiles/{id}` | admin | make or change a profile (`name`, `description`, `settings`, `rotation`, `region`, `init_txt`, `channel`, `password_policy`, `key_id`) |
| `DELETE /api/v1/profiles/{id}` | admin | remove a profile no server uses |
| `GET /api/v1/groups` | viewer | groups and their servers |
| `POST /api/v1/groups/{name}/{action}` | moderator | a job on a group's servers (the actions of jobs) |
| `GET /api/v1/jobs`, `GET /api/v1/jobs/{id}` | viewer | jobs, and one with its items |
| `POST /api/v1/jobs` | moderator | `{"action": "drain_restart", "slugs": […], "after": "game", "notice": "…", "per_node": 1}`; actions: `start`, `stop`, `restart`, `drain_restart`, `drain_stop`, `say`, `next`, `end_game`, `set_rotation` (all or none), `change_profile`, `update` (`channel`; rolling, with a rollback), `add_group`, `remove_group`, `delete`, `command` |
| `POST /api/v1/jobs/{id}/cancel` | admin | what runs finishes; the next waves do not start |
| `GET /api/v1/library` | viewer | the library's items |
| `POST /api/v1/library/folder` | owner | add a game data folder on the control plane's machine (`{"path": "/srv/halo"}`) |
| `DELETE /api/v1/library/{sha}` | admin | remove an item |
| `GET /api/v1/releases` | viewer | known builds and `latest` |
| `POST /api/v1/releases/check` | admin | resolve `latest` now |
| `POST /api/v1/releases/fetch` | admin | fetch a build's zip (the process runtime's) |
| `POST /api/v1/releases/local` | admin | `{"build": "dev-1", "image": "opence-server:dev", "latest": false}`: an image already on the nodes |
| `GET /api/v1/keys` | admin | publisher keys (public halves) |
| `POST /api/v1/keys` | owner | make a publisher key |
| `POST /api/v1/keys/{id}/sign` | owner | sign a text with a key (the master's proof of key) |
| `GET /api/v1/keys/{id}/me` | admin | what each master says of a key (`GET /v1/me`, signed) |
| `GET /api/v1/bans` | moderator | fleet bans |
| `POST /api/v1/bans` | admin | `{"address": "", "hwid": "", "discord_id": "", "names": "", "reason": "", "duration": "24h"}` (an address, a hardware id or a verified Discord account's id, up to 20 digits, or more than one; no duration: for ever); applied on every server (a Discord account's on those whose game has the `link` capability) |
| `DELETE /api/v1/bans/{id}` | admin | lift a fleet ban everywhere |
| `GET /api/v1/schedules`, `GET /api/v1/alerts` | viewer | schedules; open and recent alerts |
| `GET /api/v1/audit` | admin | the audit log, newest first, 500 at a time (`?actor=&action=&target=&before=`; the answer's `before` pages on) |
| `GET /api/v1/export` | owner | the fleet's configuration as JSON (`?keys=1`: with the keys' seeds, audited) |
| `POST /api/v1/import` | owner | an export, into this fleet |
| `GET /api/v1/stream` | viewer | the live changes, as server-sent events (below) |

## The stream

`GET /api/v1/stream` is `text/event-stream`: one event per change, with a
keepalive comment every 25 seconds.

```
event: instance
data: {"slug":"btb-eu-1","state":"up","reason":"","build":"build-206","status":{…the server's /api/status…}}

event: feed
data: {"ID":812,"Time":"2026-10-26T02:33:10Z","Slug":"btb-eu-1","Kind":"game","Event":"game_started","Text":"started ctf on bloodgulch, 12 players","Private":false}

event: job
data: {"key":"42"}
```

`instance` (with `"removed": true` when a server goes), `feed` (players'
addresses for admins alone), `node`, `alert` and `job` (keys: fetch the
object). The pages' own stream, `/events`, sends rendered fragments instead.
