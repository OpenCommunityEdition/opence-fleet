#!/bin/sh
# opence-fleet's node installer, served by the control plane at /agent.sh
# with its URL, version and release key filled in:
#
#   curl -fsSL https://fleet.example.org/agent.sh | sudo sh -s -- --join https://fleet.example.org --token <join token>
#
# It installs Docker if it is missing (asking first; --runtime process
# installs the 32-bit C runtime instead), makes the opence-fleet user and
# /var/lib/opence-fleet, installs the agent (checked against the release's
# signed SHA256SUMS, or the control plane's own binary for a development
# build), enrols the node (the join token, once, for the node's token in
# /etc/opence-fleet/agent.env, mode 0600) and starts opence-fleet-agent.service.
# Run again with a new token, it enrols the same node again and keeps its
# servers. POSIX sh; every question is also an option, and --yes answers all.
set -eu

FLEET_URL="@FLEET_URL@"
FLEET_VERSION="@FLEET_VERSION@"
RELEASE_KEY="@RELEASE_KEY@"
SELF_ARCH="@SELF_ARCH@"
SELF_SHA256="@SELF_SHA256@"
REPOSITORY="OpenCommunityEdition/opence-fleet"

JOIN=""
TOKEN=""
RUNTIME="${FLEET_AGENT_RUNTIME:-docker}"
NAME="${FLEET_AGENT_NAME:-}"
DIR="${FLEET_AGENT_DIR:-/var/lib/opence-fleet}"
DATA_ROOT="${FLEET_AGENT_DATA_ROOT:-}"
YES=""
BIN=/usr/local/bin/opence-fleet
ENV_FILE=/etc/opence-fleet/agent.env
UNIT=/etc/systemd/system/opence-fleet-agent.service

say() { printf '%s\n' "$*" >&2; }
die() {
	say "agent.sh: $*"
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--join) JOIN="$2"; shift 2 ;;
	--token) TOKEN="$2"; shift 2 ;;
	--runtime) RUNTIME="$2"; shift 2 ;;
	--name) NAME="$2"; shift 2 ;;
	--dir) DIR="$2"; shift 2 ;;
	--data-root) DATA_ROOT="$2"; shift 2 ;;
	--yes | -y) YES=1; shift ;;
	-h | --help)
		sed -n '2,16p' "$0" 2>/dev/null || true
		exit 0
		;;
	*) die "unknown option $1" ;;
	esac
done

[ -n "$JOIN" ] || JOIN="$FLEET_URL"
[ -n "$TOKEN" ] || die "--token <join token> is needed (the Add a node page makes one)"
case "$RUNTIME" in docker | process) ;; *) die "--runtime is docker or process" ;; esac
[ "$(id -u)" = 0 ] || die "run it as root (sudo)"

ask() {
	# ask <question>: yes with --yes, else the answer read from the terminal
	if [ -n "$YES" ]; then
		return 0
	fi
	if [ ! -r /dev/tty ]; then
		die "$1 (no terminal to ask: run with --yes)"
	fi
	printf '%s [y/N] ' "$1" >/dev/tty
	read -r answer </dev/tty || answer=""
	case "$answer" in y | Y | yes | YES) return 0 ;; esac
	return 1
}

case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
armv7l) ARCH=arm ;;
*) die "no agent for $(uname -m)" ;;
esac
if [ "$ARCH" != amd64 ]; then
	say "agent.sh: this machine is $ARCH: the game is 32-bit x86, so this node runs no server (an amd64 machine does)."
	ask "Install the agent anyway?" || exit 1
fi

download() {
	# download <url> <file>
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$2" "$1"
	else
		wget -q -O "$2" "$1"
	fi
}

unhex() {
	# unhex <hex>: its bytes (octal escapes for printf's %b, which POSIX has)
	printf '%b' "$(printf '%s' "$1" | awk '{
		h = "0123456789abcdef"; s = tolower($0)
		for (i = 1; i < length(s); i += 2) {
			n = (index(h, substr(s, i, 1)) - 1) * 16 + index(h, substr(s, i + 1, 1)) - 1
			printf "\\0%03o", n
		}
	}')"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# ---------- the runtime
if [ "$RUNTIME" = docker ]; then
	if ! command -v docker >/dev/null 2>&1; then
		if ask "Docker is not installed. Install it (get.docker.com)?"; then
			download https://get.docker.com /tmp/get-docker.sh
			sh /tmp/get-docker.sh
			rm -f /tmp/get-docker.sh
		else
			die "Docker is needed for the container runtime (or run again with --runtime process)"
		fi
	fi
	systemctl enable --now docker >/dev/null 2>&1 || true
else
	if [ ! -e /lib/ld-linux.so.2 ]; then
		if command -v apt-get >/dev/null 2>&1; then
			if ask "The process runtime needs the 32-bit C runtime (libc6:i386, libgcc-s1:i386). Install it?"; then
				dpkg --add-architecture i386
				apt-get update -qq
				apt-get install -y -qq libc6:i386 libgcc-s1:i386
			else
				die "the 32-bit C runtime is needed for the process runtime"
			fi
		else
			say "agent.sh: install the 32-bit C runtime (glibc i686 and libgcc) with your distribution's packages."
		fi
	fi
fi

# ---------- the user and the folders
if ! id opence-fleet >/dev/null 2>&1; then
	useradd --system --home-dir "$DIR" --shell /usr/sbin/nologin opence-fleet 2>/dev/null ||
		adduser --system --home "$DIR" --no-create-home --group opence-fleet
fi
if [ "$RUNTIME" = docker ] && getent group docker >/dev/null 2>&1; then
	usermod -aG docker opence-fleet
fi
install -d -o opence-fleet -g opence-fleet -m 0750 "$DIR"
install -d -m 0755 /etc/opence-fleet

# ---------- the agent's binary
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
case "$FLEET_VERSION" in
v[0-9]*.[0-9]*.[0-9]*)
	asset="opence-fleet-linux-$ARCH"
	[ "$ARCH" = arm ] && asset="opence-fleet-linux-armv7"
	base="https://github.com/$REPOSITORY/releases/download/$FLEET_VERSION"
	download "$base/$asset" "$work/opence-fleet"
	download "$base/SHA256SUMS" "$work/SHA256SUMS"
	download "$base/SHA256SUMS.sig" "$work/SHA256SUMS.sig"
	if command -v openssl >/dev/null 2>&1 && openssl version | grep -q '^OpenSSL [3-9]'; then
		# (the release key as a PEM public key: the Ed25519 SubjectPublicKeyInfo prefix, then the key)
		{
			echo "-----BEGIN PUBLIC KEY-----"
			unhex "302a300506032b6570032100$RELEASE_KEY" | base64
			echo "-----END PUBLIC KEY-----"
		} >"$work/key.pem"
		unhex "$(tr -d ' \n' <"$work/SHA256SUMS.sig")" >"$work/sig.bin"
		openssl pkeyutl -verify -pubin -inkey "$work/key.pem" -rawin -in "$work/SHA256SUMS" -sigfile "$work/sig.bin" >/dev/null ||
			die "the release's SHA256SUMS is not signed by the release key: not installed"
	else
		say "agent.sh: no OpenSSL 3 to check the release's signature: the sums are checked, over TLS"
	fi
	want="$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1 }' "$work/SHA256SUMS")"
	[ -n "$want" ] || die "the release's SHA256SUMS has no $asset"
	;;
*)
	# (a development build's control plane serves its own binary, for a
	# machine of its kind)
	[ "$ARCH" = "$SELF_ARCH" ] || die "the control plane is a development build for $SELF_ARCH: install the agent for $ARCH by hand"
	download "$JOIN/agent/binary/$ARCH" "$work/opence-fleet"
	want="$SELF_SHA256"
	;;
esac
[ "$(sha256 "$work/opence-fleet")" = "$want" ] || die "the agent's SHA-256 is not the one expected: not installed"
install -m 0755 "$work/opence-fleet" "$BIN"
"$BIN" version >&2

# ---------- the enrolment
FLEET_AGENT_DIR="$DIR" FLEET_AGENT_RUNTIME="$RUNTIME" FLEET_AGENT_DATA_ROOT="$DATA_ROOT" FLEET_AGENT_NAME="$NAME" \
	"$BIN" agent --join "$JOIN" --token "$TOKEN" --env-file "$ENV_FILE"
chmod 0600 "$ENV_FILE"

# ---------- the service
groups="docker"
[ "$RUNTIME" = docker ] || groups=""
syscalls="native"
[ "$RUNTIME" = process ] && syscalls="native x86"
writable="$DIR"
[ "$RUNTIME" = docker ] && writable="$DIR /run/docker.sock"
memory="256M"
[ "$RUNTIME" = process ] && memory="8G"
cat >"$UNIT" <<UNIT
# opence-fleet's agent (written by agent.sh): it runs this node's servers.
[Unit]
Description=opence-fleet agent
Documentation=https://github.com/$REPOSITORY
After=network-online.target docker.service
Wants=network-online.target

[Service]
User=opence-fleet
SupplementaryGroups=$groups
EnvironmentFile=$ENV_FILE
ExecStart=$BIN agent
Restart=always
RestartSec=5
# (SIGTERM to the agent, which stops its servers itself: 15 s each, at once)
KillMode=mixed
TimeoutStopSec=40
NoNewPrivileges=yes
CapabilityBoundingSet=
ProtectSystem=strict
ReadWritePaths=$writable
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=$syscalls
SystemCallFilter=@system-service
MemoryMax=$memory

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable opence-fleet-agent.service >/dev/null
systemctl restart opence-fleet-agent.service
say ""
say "The node is enrolled and its agent runs (journalctl -u opence-fleet-agent -f)."
say "Open its tunnel ports to the internet (UDP 2302-2399 by default), e.g. ufw allow 2302:2399/udp."
