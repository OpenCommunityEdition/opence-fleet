#!/bin/sh
# Installs an OpenCE fleet's control plane with Docker, behind Caddy (which
# gets its HTTPS certificate), and makes this machine the fleet's first node:
#
#   curl -fsSL https://github.com/OpenCommunityEdition/opence-fleet/releases/latest/download/install.sh | sh
#
# It asks for the fleet's domain, whether the bundled Caddy gets the HTTPS
# certificate or a reverse proxy of your own (nginx, Caddy, ...) on this
# machine forwards to the control plane, and whether this machine also runs
# servers. It installs Docker if it is missing (with your consent), writes
# compose.yaml, the settings and the proxy's files to /opt/opence-fleet,
# starts the control plane, prints the setup link that makes the owner's
# account, and, when this machine runs servers, enrols it as a node: the
# agent on the host under systemd (agent.sh), so that the games run as
# containers beside the control plane's, never inside it. Run it again to
# repair, move or switch an install: the data (the fleet-data volume), the
# settings and the node's enrolment are kept.
#
# Options (or the environment variables beside them):
#   --domain <name>    OPENCE_DOMAIN        the fleet's domain
#   --proxy <which>    OPENCE_PROXY         bundled (Caddy in Docker, on ports
#                                           80 and 443), external (your own
#                                           proxy, forwarding to 127.0.0.1), or
#                                           insecure (no HTTPS: plain HTTP on a
#                                           port of every address; passwords and
#                                           tokens cross the network unencrypted)
#   --port <n>         OPENCE_PORT          the control plane's port: on
#                                           127.0.0.1 for an external proxy
#                                           (8070), on every address for
#                                           insecure (80)
#   --servers <yes|no> OPENCE_SERVERS       this machine also runs servers (yes)
#   --node <name>      OPENCE_NODE          its name as a node (the host name)
#   --dir <path>       OPENCE_DIR           where it goes (/opt/opence-fleet)
#   --yes              OPENCE_YES=1         no questions: the bundled proxy
#                                           unless --proxy says otherwise, a
#                                           node unless --servers no, and
#                                           Docker installed if missing
set -eu

REPO="OpenCommunityEdition/opence-fleet"
BASE="${OPENCE_RELEASE_URL:-https://github.com/$REPO/releases/latest/download}"
IMAGE="ghcr.io/opencommunityedition/opence-fleet:latest"
DIR="${OPENCE_DIR:-/opt/opence-fleet}"
DOMAIN="${OPENCE_DOMAIN:-}"
PROXY="${OPENCE_PROXY:-}"
PORT="${OPENCE_PORT:-}"
SERVERS="${OPENCE_SERVERS:-}"
NODE="${OPENCE_NODE:-}"
YES="${OPENCE_YES:-}"

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
fail() {
	printf '\nopence-fleet install: %s\n' "$*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--domain) [ $# -ge 2 ] || fail "--domain needs a name"; DOMAIN="$2"; shift 2 ;;
	--domain=*) DOMAIN="${1#--domain=}"; shift ;;
	--proxy) [ $# -ge 2 ] || fail "--proxy needs bundled, external or insecure"; PROXY="$2"; shift 2 ;;
	--proxy=*) PROXY="${1#--proxy=}"; shift ;;
	--port) [ $# -ge 2 ] || fail "--port needs a number"; PORT="$2"; shift 2 ;;
	--port=*) PORT="${1#--port=}"; shift ;;
	--servers) [ $# -ge 2 ] || fail "--servers needs yes or no"; SERVERS="$2"; shift 2 ;;
	--servers=*) SERVERS="${1#--servers=}"; shift ;;
	--node) [ $# -ge 2 ] || fail "--node needs a name"; NODE="$2"; shift 2 ;;
	--node=*) NODE="${1#--node=}"; shift ;;
	--dir) [ $# -ge 2 ] || fail "--dir needs a path"; DIR="$2"; shift 2 ;;
	--dir=*) DIR="${1#--dir=}"; shift ;;
	-y | --yes) YES=1; shift ;;
	-h | --help) sed -n '2,41p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) fail "unknown option $1 (--help lists them)" ;;
	esac
done

# ask reads an answer from the terminal (the script itself may be standard
# input, through a pipe); with no terminal, the answer is empty.
ask() {
	printf '%s ' "$1"
	answer=""
	if [ -z "$YES" ] && [ -r /dev/tty ]; then
		read -r answer </dev/tty || answer=""
	fi
}

# ---------- what it needs

[ "$(uname -s)" = Linux ] || fail "this installer is for Linux (README.md tells of the other ways)"
if [ "$(id -u)" -eq 0 ]; then
	SUDO=""
elif command -v sudo >/dev/null 2>&1; then
	# (the node's agent is a systemd service: root, even when Docker is the
	# user's)
	SUDO="sudo"
	say "Some steps need root: sudo may ask for your password."
else
	fail "run it as root (or install sudo)"
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	fail "curl or wget is needed"
fi

step "Docker"
if ! command -v docker >/dev/null 2>&1; then
	if [ -n "$YES" ]; then
		answer=y
	else
		ask "Docker is not installed. Install it with Docker's own script (https://get.docker.com)? [Y/n]"
	fi
	case "$answer" in
	"" | y | Y | yes) ;;
	*) fail "Docker is needed: https://docs.docker.com/engine/install/ (or README.md, \"Without Docker\")" ;;
	esac
	if [ -n "$YES" ] || [ -r /dev/tty ]; then :; else fail "no terminal to ask on: run it with --yes"; fi
	tmp_docker="$(mktemp)"
	fetch https://get.docker.com "$tmp_docker"
	$SUDO sh "$tmp_docker"
	rm -f "$tmp_docker"
fi
$SUDO docker info >/dev/null 2>&1 || fail "Docker is installed but not running: start it (systemctl start docker)"
$SUDO docker compose version >/dev/null 2>&1 ||
	fail "Docker Compose is needed (the docker-compose-plugin package): https://docs.docker.com/compose/install/"
say "$($SUDO docker --version), $($SUDO docker compose version --short 2>/dev/null || echo compose)"

# ---------- the proxy, the domain, the node

# (an earlier install's choices are the defaults)
previous() {
	if [ -f "$DIR/.env" ]; then sed -n "s/^$1=//p" "$DIR/.env" | head -n 1; fi
}
if [ -z "$PROXY" ] && [ -n "$(previous OPENCE_PROXY)" ]; then
	PROXY="$(previous OPENCE_PROXY)"
	say "Keeping the $PROXY reverse proxy (--proxy changes it)."
fi
if [ -z "$PORT" ]; then
	PORT="$(previous FLEET_PORT)"
fi
if [ -z "$DOMAIN" ] && [ -n "$(previous FLEET_DOMAIN)" ]; then
	DOMAIN="$(previous FLEET_DOMAIN)"
	say "Keeping the domain $DOMAIN (--domain changes it)."
fi

step "HTTPS"
if [ -z "$PROXY" ]; then
	if [ -n "$YES" ] || [ ! -r /dev/tty ]; then
		PROXY=bundled
	else
		say "The fleet needs HTTPS in front of it (its agents connect only over HTTPS). Either:"
		say "  1) Caddy, in Docker beside the control plane, gets the certificate itself (ports 80 and 443)"
		say "  2) a reverse proxy of your own on this machine (nginx, Caddy, ...) forwards to the control plane"
		say "  3) no HTTPS: plain HTTP on a port of this machine. INSECURE: passwords, sessions and node"
		say "     tokens cross the network unencrypted (a trusted LAN or a test only)"
		ask "Which? [1]"
		case "$answer" in
		"" | 1 | bundled) PROXY=bundled ;;
		2 | external) PROXY=external ;;
		3 | insecure | http | none) PROXY=insecure ;;
		*) fail "\"$answer\" is not 1, 2 or 3" ;;
		esac
	fi
fi
case "$PROXY" in
bundled) say "Caddy in Docker gets the certificate." ;;
external)
	if [ -z "$PORT" ] && [ -z "$YES" ] && [ -r /dev/tty ]; then
		ask "The control plane's port on 127.0.0.1, which your proxy forwards to: [8070]"
		PORT="$answer"
	fi
	PORT="${PORT:-8070}"
	case "$PORT" in
	*[!0-9]*) fail "the port \"$PORT\" is not a number" ;;
	esac
	{ [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ]; } || fail "the port $PORT is not 1 to 65535"
	say "Your proxy forwards to http://127.0.0.1:$PORT."
	;;
insecure | http | none)
	PROXY=insecure
	say ""
	say "Plain HTTP, no HTTPS: the fleet's passwords, sign-in sessions and the nodes' tokens cross"
	say "the network unencrypted, readable by anyone on the way, who can then sign in as you or"
	say "enrol a node of their own. Use it on a network you trust, or for a test."
	if [ -z "$YES" ] && [ -r /dev/tty ]; then
		ask "Go on without HTTPS? [y/N]"
		case "$answer" in
		y | Y | yes) ;;
		*) fail "stopped: run it again and choose 1 or 2 for HTTPS" ;;
		esac
	fi
	if [ -z "$PORT" ] && [ -z "$YES" ] && [ -r /dev/tty ]; then
		ask "The fleet's port on this machine (every address: the pages and the agents): [80]"
		PORT="$answer"
	fi
	PORT="${PORT:-80}"
	case "$PORT" in
	*[!0-9]*) fail "the port \"$PORT\" is not a number" ;;
	esac
	{ [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ]; } || fail "the port $PORT is not 1 to 65535"
	say "The fleet takes plain HTTP on port $PORT."
	;;
*) fail "--proxy is bundled, external or insecure, not \"$PROXY\"" ;;
esac
while [ -z "$DOMAIN" ]; do
	[ -r /dev/tty ] || fail "no terminal to ask on: give the domain with --domain fleet.example.org"
	if [ "$PROXY" = external ]; then
		ask "The fleet's domain, which your proxy serves (such as fleet.example.org):"
	elif [ "$PROXY" = insecure ]; then
		ask "The name or address the fleet's users and nodes reach this machine by (fleet.lan, 192.168.1.20):"
	else
		ask "The fleet's domain (its DNS record points at this machine), such as fleet.example.org:"
	fi
	DOMAIN="$answer"
done
DOMAIN="$(printf '%s' "$DOMAIN" | tr '[:upper:]' '[:lower:]' | sed 's#^https\{0,1\}://##; s#/.*##')"
case "$DOMAIN" in
*[!a-z0-9.-]* | .* | *. | *..* | "") fail "\"$DOMAIN\" is not a domain name" ;;
*.*) ;;
# (a LAN's host name will do for plain HTTP; a certificate needs a full one)
*) [ "$PROXY" = insecure ] || fail "\"$DOMAIN\" is not a full domain name (fleet.example.org)" ;;
esac
# (the fleet's public URL: its pages, the links, the agents' one-liner)
if [ "$PROXY" = insecure ]; then
	URL="http://$DOMAIN"
	[ "$PORT" = 80 ] || URL="$URL:$PORT"
else
	URL="https://$DOMAIN"
fi

if [ "$PROXY" = bundled ] && command -v getent >/dev/null 2>&1; then
	if ! getent ahosts "$DOMAIN" >/dev/null 2>&1; then
		say "Warning: $DOMAIN has no DNS record yet. Caddy gets the certificate once it points here."
	fi
fi
# (a port taken by something other than this install)
running=""
if [ -f "$DIR/compose.yaml" ]; then
	running="$(cd "$DIR" && $SUDO docker compose ps --format '{{.Service}} {{.Ports}}' 2>/dev/null || true)"
fi
if command -v ss >/dev/null 2>&1; then
	if [ "$PROXY" = bundled ] && ss -ltn 2>/dev/null | grep -Eq '[:.](80|443)[[:space:]]' &&
		! printf '%s\n' "$running" | grep -q '^caddy'; then
		say "Warning: something already listens on port 80 or 443, which Caddy needs. If it is your own"
		say "web server, run the installer again and choose 2 (--proxy external)."
	fi
	if [ "$PROXY" != bundled ] && ss -ltn 2>/dev/null | grep -Eq "[:.]${PORT}[[:space:]]" &&
		! printf '%s\n' "$running" | grep -q ":$PORT->"; then
		fail "port $PORT on this machine is taken: choose another with --port"
	fi
fi

step "Servers on this machine"
enrolled=""
if [ -f /etc/opence-fleet/agent.env ]; then
	enrolled=1
	SERVERS=yes
	say "This machine is a node of a fleet already (/etc/opence-fleet/agent.env): kept."
fi
if [ -z "$SERVERS" ]; then
	if [ -n "$YES" ] || [ ! -r /dev/tty ]; then
		SERVERS=yes
	else
		ask "Will this machine also run game servers (it becomes the fleet's first node)? [Y/n]"
		case "$answer" in
		"" | y | Y | yes) SERVERS=yes ;;
		*) SERVERS=no ;;
		esac
	fi
fi
case "$SERVERS" in
yes | y | true | 1) SERVERS=yes ;;
no | n | false | 0) SERVERS=no ;;
*) fail "--servers is yes or no, not \"$SERVERS\"" ;;
esac
if [ "$SERVERS" = yes ] && [ -z "$enrolled" ]; then
	case "$(uname -m)" in
	x86_64 | amd64) ;;
	*)
		say "This machine is $(uname -m): the game's server is 32-bit x86, so it runs no servers (an amd64 machine does)."
		SERVERS=no
		;;
	esac
fi
if [ "$SERVERS" = yes ] && [ -z "$enrolled" ]; then
	[ -n "$NODE" ] || NODE="$(hostname 2>/dev/null || uname -n)"
	NODE="$(printf '%s' "$NODE" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g; s/^-*//; s/-*$//' | cut -c1-40)"
	[ -n "$NODE" ] || NODE=node-1
	say "This machine runs servers, as the node $NODE (--node changes its name)."
else
	say "This machine runs no servers: add nodes from the fleet's Nodes page."
fi

# ---------- the files

step "Files in $DIR"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for f in compose.yaml compose.proxy.yaml compose.http.yaml Caddyfile SHA256SUMS; do
	fetch "$BASE/$f" "$tmp/$f" || fail "$BASE/$f could not be downloaded"
done
if command -v sha256sum >/dev/null 2>&1; then
	(cd "$tmp" && grep -E '  (compose\.yaml|compose\.proxy\.yaml|compose\.http\.yaml|Caddyfile)$' SHA256SUMS | sha256sum -c --status) ||
		fail "the downloaded files are not the release's (SHA256SUMS)"
fi
$SUDO mkdir -p "$DIR/deploy"
$SUDO install -m 644 "$tmp/compose.yaml" "$DIR/compose.yaml"
$SUDO install -m 644 "$tmp/compose.proxy.yaml" "$DIR/compose.proxy.yaml"
$SUDO install -m 644 "$tmp/compose.http.yaml" "$DIR/compose.http.yaml"
$SUDO install -m 644 "$tmp/Caddyfile" "$DIR/deploy/Caddyfile"
{
	say "# install.sh's choices (docker compose reads this file)"
	say "FLEET_DOMAIN=$DOMAIN"
	say "OPENCE_PROXY=$PROXY"
	if [ "$PROXY" = external ]; then
		say "FLEET_PORT=$PORT"
		# (the control plane's port on loopback, and no Caddy)
		say "COMPOSE_FILE=compose.yaml:compose.proxy.yaml"
	elif [ "$PROXY" = insecure ]; then
		say "FLEET_PORT=$PORT"
		# (the fleet's port on every address, plain HTTP taken, and no Caddy)
		say "COMPOSE_FILE=compose.yaml:compose.http.yaml"
	fi
} >"$tmp/.env"
$SUDO install -m 644 "$tmp/.env" "$DIR/.env"
if $SUDO test -f "$DIR/fleet.env"; then
	# (kept, with the domain's URL)
	$SUDO sed -i "s#^FLEET_PUBLIC_URL=.*#FLEET_PUBLIC_URL=$URL#" "$DIR/fleet.env"
	say "Kept the settings in $DIR/fleet.env."
else
	cat >"$tmp/fleet.env" <<EOF
# The control plane's settings (every option: README.md, "Options"). The
# fleet's name, time zone, retention and webhooks are set on its Settings
# page; a variable set here wins over the page.
FLEET_PUBLIC_URL=$URL
# updates: notify (the owners update with a button), auto, or off
#FLEET_UPDATES=notify
#FLEET_TIMEZONE=Europe/Paris
EOF
	$SUDO install -m 600 "$tmp/fleet.env" "$DIR/fleet.env"
fi
if [ "$PROXY" = external ]; then
	cat >"$tmp/reverse-proxy.txt" <<PROXYEOF
Your reverse proxy serves https://$DOMAIN and forwards to http://127.0.0.1:$PORT,
with the X-Forwarded-Proto, X-Forwarded-For and X-Forwarded-Host headers (the
control plane refuses its forms and the agents without them), WebSocket
upgrades (the agents' /agent, the console) and unbuffered responses (the
pages' live /events stream).

Caddy (a Caddyfile; it gets the certificate itself and sends the headers):

$DOMAIN {
	reverse_proxy 127.0.0.1:$PORT {
		flush_interval -1
	}
}

nginx (a server block; the certificate is yours to get, with certbot say):

server {
	listen 443 ssl;
	listen [::]:443 ssl;
	server_name $DOMAIN;
	ssl_certificate     /etc/letsencrypt/live/$DOMAIN/fullchain.pem;
	ssl_certificate_key /etc/letsencrypt/live/$DOMAIN/privkey.pem;
	# (the library's uploads come in chunks of 8 MB)
	client_max_body_size 16m;

	location / {
		proxy_pass http://127.0.0.1:$PORT;
		proxy_http_version 1.1;
		proxy_set_header Host \$host;
		proxy_set_header Upgrade \$http_upgrade;
		proxy_set_header Connection \$connection_upgrade;
		proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
		proxy_set_header X-Forwarded-Proto \$scheme;
		proxy_set_header X-Forwarded-Host \$host;
		proxy_buffering off;
		proxy_read_timeout 1h;
	}
}

(with, in the http block: map \$http_upgrade \$connection_upgrade { default upgrade; '' close; })
PROXYEOF
	$SUDO install -m 644 "$tmp/reverse-proxy.txt" "$DIR/reverse-proxy.txt"
fi
say "Wrote compose.yaml, compose.proxy.yaml, compose.http.yaml, deploy/Caddyfile, .env and fleet.env."

# ---------- start

caddy=""
[ "$PROXY" = bundled ] && caddy=" and Caddy"
step "Starting the control plane$caddy"
cd "$DIR"
if ! $SUDO docker compose pull --quiet; then
	$SUDO docker image inspect "$IMAGE" >/dev/null 2>&1 ||
		fail "the image could not be downloaded ($IMAGE)"
	say "Warning: the images were not updated; using the ones already here."
fi
# (recreated: a repair runs the settings just written, and the log below is
# this start's)
if [ "$PROXY" != bundled ]; then
	# (the bundled Caddy of an earlier install goes)
	$SUDO docker compose --profile bundled-caddy rm -sf caddy >/dev/null 2>&1 || true
fi
$SUDO docker compose up -d --remove-orphans --force-recreate

step "Waiting for the control plane"
link=""
ready=""
i=0
while [ $i -lt 60 ]; do
	since="$($SUDO docker inspect -f '{{.State.StartedAt}}' "$($SUDO docker compose ps -q fleet)" 2>/dev/null || true)"
	logs="$($SUDO docker compose logs --no-color ${since:+--since "$since"} fleet 2>/dev/null || true)"
	if printf '%s' "$logs" | grep -q '"msg":"serving"'; then
		ready=1
		link="$(printf '%s\n' "$logs" | grep -o 'https\{0,1\}://[^ "]*/setup?code=[0-9a-zA-Z_-]*' | tail -n 1)"
		[ -n "$link" ] && break
		# (set up already: no link; a moment for the line to come)
		[ $i -ge 3 ] && break
	fi
	i=$((i + 1))
	sleep 2
done
[ -n "$ready" ] || fail "the control plane did not start: ${SUDO:+$SUDO }docker compose --project-directory $DIR logs fleet"

# ---------- this machine as a node

node_line=""
if [ "$SERVERS" = yes ] && [ -z "$enrolled" ]; then
	step "Enrolling this machine as the node $NODE"
	# (the join token from the control plane itself, over its own process: it
	# is good for an hour, once)
	token="$($SUDO docker compose exec -T fleet opence-fleet join-token --quiet "$NODE" 2>/dev/null | tail -n 1)" ||
		token=""
	[ -n "$token" ] || fail "no join token: ${SUDO:+$SUDO }docker compose --project-directory $DIR exec fleet opence-fleet join-token $NODE"
	# (the agent connects to the control plane over HTTPS, as every node's
	# does: the certificate first)
	say "Waiting for $URL to answer (the certificate, the proxy)..."
	up=""
	i=0
	while [ $i -lt 60 ]; do
		if fetch "$URL/healthz" "$tmp/healthz" 2>/dev/null; then
			up=1
			break
		fi
		i=$((i + 1))
		sleep 3
	done
	node_line="curl -fsSL $URL/agent.sh | sudo sh -s -- --token $token --name $NODE"
	if [ -n "$up" ] && fetch "$URL/agent.sh" "$tmp/agent.sh"; then
		if $SUDO sh "$tmp/agent.sh" --token "$token" --name "$NODE" ${YES:+--yes}; then
			node_line=""
			enrolled=1
		else
			say "The node was not enrolled (above: why)."
		fi
	else
		say "$URL does not answer yet."
	fi
fi

say ""
if [ "$PROXY" = external ]; then
	say "The OpenCE fleet listens on http://127.0.0.1:$PORT, for your reverse proxy."
	say ""
	say "Set your proxy up to serve https://$DOMAIN and forward to it; with Caddy:"
	say ""
	say "    $DOMAIN {"
	say "        reverse_proxy 127.0.0.1:$PORT {"
	say "            flush_interval -1"
	say "        }"
	say "    }"
	say ""
	say "(nginx's, and the headers the control plane needs: $DIR/reverse-proxy.txt). Then open the link."
elif [ "$PROXY" = insecure ]; then
	say "The OpenCE fleet runs at $URL: plain HTTP, NOT ENCRYPTED (passwords and tokens are"
	say "readable on the network). Let port $PORT/tcp in through this machine's firewall."
else
	say "The OpenCE fleet runs at $URL"
fi
if [ -n "$link" ]; then
	say ""
	say "Open this link to make the fleet's owner (it is good once):"
	say ""
	say "    $link"
	say ""
	if [ "$PROXY" = bundled ]; then
		say "Caddy may take a minute to get the certificate; reload if the page does not open yet."
	fi
else
	say "It is set up already: sign in at $URL/"
fi
if [ -n "$node_line" ]; then
	say ""
	say "To make this machine a node once $URL answers (within the hour; else the"
	say "Nodes page's Add a node makes another token):"
	say ""
	say "    $node_line"
elif [ -n "$enrolled" ] && [ "$SERVERS" = yes ]; then
	say "This machine is a node of the fleet: its servers are made on the Servers page."
fi
run="${SUDO:+$SUDO }"
say ""
say "In $DIR:"
say "  ${run}docker compose logs -f fleet      the log"
say "  ${run}docker compose pull && ${run}docker compose up -d   update (the Settings page does it with a"
say "                                            button too; the servers keep running)"
say "  ${run}docker compose down               stop (the data stays in the fleet-data volume; the"
say "                                            servers keep running on their nodes)"
say "Back up the fleet-data volume: it holds the database and fleet.key, which encrypts its secrets."
