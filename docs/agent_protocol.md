# The agent protocol (`opence-fleet-agent-1`)

How a node's agent and the control plane talk. The types are
`internal/agentproto`; the control plane's end is `internal/hub`, the
agent's `internal/agent`. The agent is the same binary as the control
plane, and a control plane offers its agents its own version, so both ends
normally speak the same protocol; the version is still checked, so that a
mismatch is a clear refusal and not a confusion.

## Enrolment

A join token (24 random bytes in hex, good for an hour, once; the Nodes page
or `opence-fleet join-token` makes it, and the database keeps its SHA-256)
is traded for a node token:

```
POST /agent/join
{"token": "<join token>", "name": "eu-1", "hostname": "…", "runtime": "docker", "os": "linux", "arch": "amd64"}

200 {"ok": true, "node": "eu-1", "token": "<node token>"}
```

The node token is the agent's identity from then on
(`FLEET_AGENT_TOKEN`, in `/etc/opence-fleet/agent.env`, mode 0600); the
control plane keeps its SHA-256 alone. Enrolling the same name again (a new
join token) replaces the token and keeps the node's servers. Forgetting a
node revokes its token and closes its connection.

## The connection

```
GET /agent
Authorization: Bearer <node token>
Sec-WebSocket-Protocol: opence-fleet-agent-1
```

Over HTTPS (or loopback). The upgrade is HTTP/1.1's: the agent offers
`http/1.1` alone in TLS's ALPN, so a proxy that also speaks HTTP/2 (Caddy
by default) needs nothing of its own. (Agents of v0.1.13 and before offered
`h2` too, which such a proxy took, failing the upgrade; until they update
themselves, they connect through a proxy limited to HTTP/1.1 for the
fleet's site, `servers { protocols h1 h3 }` in a Caddyfile.) One WebSocket
per node; a second connection of
the same node replaces the first. Text frames, one JSON object each, with
its `type`:

```json
{"type": "heartbeat", "load": 0.4, …}
```

- **The agent speaks first** (`hello`, within 15 seconds); the control plane
  answers with the node's `spec`, and sends it whole again on every change
  that concerns the node (never a diff; the same spec but its generation is
  not sent twice).
- **Requests** (`api`, `console`, `control`, `update_agent`) carry an `id`
  and get exactly one `reply` with that id.
- The control plane pings every 20 seconds; the agent's heartbeat comes
  every 15. A node silent for 45 seconds is offline (its servers' states
  show *unknown*, not *down*: they run on).
- A peer that cannot keep up (its queue full) is closed (`overflow`); the
  agent reconnects.
- A refusal before the close is a `close` message: `{"type": "close",
  "code": "version" | "revoked" | "replaced" | "bad_request", "message": "…"}`.
- The agent reconnects after 1 second, doubling to 30 at most. While the
  control plane is away it keeps supervising from its state file
  (`agent.state.json`: the last spec, what each server was started from),
  so a control plane's restart or outage changes nothing on the node.

## The agent's messages

| Type | When | Fields |
| --- | --- | --- |
| `hello` | first | `protocol` (1), `node`, `version`, `os`, `arch`, `hostname`, `kernel`, `cpus`, `memory`, `disk`, `runtime` (`docker` or `process`), `docker` (its version), `data_root` (`synced` or the folder), `instances` (below), `last_seq` (each server's last event seq the agent sent, by slug) |
| `heartbeat` | every 15 s | `load`, `memory_used`, `memory_total`, `disk_used`, `disk_total`, `cpu` (the CPUs' busy share since the last heartbeat, 0 to 100), `net_received` and `net_sent` (the interfaces' byte counters, the loopback's and containers' virtual ones left out; the three optional: an older agent, or one off Linux, sends none), `version`, `images` (builds ready), `library_synced`, `missing` (library items not there), `instances` |
| `instance` | a server's state changes | `slug`, `state`, `exit`, `signal`, `reason`, `tail` (its last log lines on a crash), `generation`, `build`, `since`, `server_build`, `capabilities` (the game's, from its RCON hello) |
| `status` | every 2 s per server (its RCON's status event) | `slug`, `status` (the server's `/api/status` object, whole) |
| `event` | each of a server's events | `slug`, `run` (the server's start, ns: a seq is the run's), `seq`, `time`, `event`, `data` (the event whole), `replay` |
| `log` | each log line | `slug`, `n`, `time`, `level`, `cat`, `text`, `source` (`stdout`, `stderr`, `agent`), `private` (admins alone: the owner's console, an invite) |
| `sync` | a download's progress | `kind` (`release`, `library`), `name`, `done`, `total`, `error` |
| `applied` | a spec's live changes to a running server | `slug`, `generation`, `applied` (setting names), `errors` (`name`, `code`, `message`: the server's refusals), `restart_needed`, `drift` |
| `reply` | a request's answer | `id`, `ok`, `status`, `body`, `lines`, `error` (`code`, `message`) |

An instance report (in `hello` and `heartbeat`): `slug`, `id` (the
container's id or the pid), `state`, `since` (ms), `build`, `generation`,
`rss`, `cpu`, `restart_needed`.

The states: `starting`, `up` (ready seen, the API answering), `stopping`,
`stopped`, `crashed` (started again after a backoff), `blocked` (stopped by
its configuration or game data, exit 2 or 3: not restarted until the spec's
next generation), `pending` (waiting for its build or library items).

## The control plane's messages

| Type | Fields |
| --- | --- |
| `spec` | the node's desired state, whole: `generation`, `node` (its settings: runtime, port ranges, loopback base, public address, limits), `images` / `releases` (the builds its servers need, with digests or hashes and URLs), `library` (items with SHA-256 and paths), `instances` (each with its slug, generation, build, running, ports, loopback index, the rendered `config.toml`, `init.txt`, gametypes and environment, its web password; see `internal/spec`) |
| `api` | `id`, `slug`, `method`, `path`, `body`: a request to the server's HTTP API, made by the agent with the server's own session; the reply carries its `status` and `body` as the server answered |
| `console` | `id`, `slug`, `line`: a line typed on the server's standard input (the owner's console); the reply's `lines` are what the server printed for it |
| `control` | `id`, `slug`, `action` and its fields (below) |
| `update_agent` | `id`, `version`, `url`, `sha256`, `sums` and `sig` (the release's `SHA256SUMS` and the release key's signature of it): the agent checks both, installs, replies, and restarts into the new binary without stopping its servers |
| `forget` | `slug`: stop the server, remove it, keep its folder aside 7 days |
| `ping` / `pong` | (either way) |

Control actions:

| Action | Fields | What |
| --- | --- | --- |
| `start`, `stop`, `restart` | | lifecycle; a stop while the spec says running holds until a start or the next generation |
| `drain` | `after` (`game`, `empty`, `now`), `notice`, `deadline` (s), `then` (`restart` or `stop`) | the game's own drain (`sv_stop`/`sv_restart` when it has the `stop` capability; otherwise unlisted, a notice, and a wait) |
| `signal` | `signal` (`hup`, `usr1`) | `hup`: reload; `usr1`: reopen the logs |
| `logs` | `after_n`, `limit` | the agent's ring of the server's log lines |
| `rotate_password` | `password` | the server's web password, changed live |
| `ban_file` | `line` | a line appended to the server's `bans.txt` (a fleet ban for a server whose API cannot take it) |
| `file` | `file` (`debug.txt`, `stderr.txt`, `bans.txt`), `limit` | a file's last lines |
| `prune` | | builds and library items no server needs, removed |

## Exit codes and the agent's policy

The server's exit code (`docs/dedicated_server.md` in the game's
repository) decides what the agent does next:

| Exit | Meaning | The agent |
| --- | --- | --- |
| 0 | stopped (a quit, a drain's end) | *stopped*; an operator's quit is held until a start or the spec's next generation |
| 5 | restart asked (`sv_restart`, a drain with restart, an update) | started again at once |
| 2 | configuration refused | *blocked* until the next generation |
| 3 | game data missing or refused | *blocked* until the next generation |
| 1, 4, 128+n | crashed (4: a port taken) | *crashed*, started again after 10 s, 30 s, 1 min, then 5 min for each failure in a row; a run of more than 10 minutes, or a ready line, resets the count |

## Versioning

The subprotocol names the version. A control plane refuses an agent that
offers no protocol it speaks (a `close` with code `version`, naming the
version to install); `hello.protocol` is checked again. A change that an
older peer would misread is a new version (`opence-fleet-agent-2`), with
the control plane speaking both for a release.
