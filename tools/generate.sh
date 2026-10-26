#!/bin/sh
# Writes the generated files, which are committed so that a build needs no
# tool (CI checks they are current):
#
#   - the queries' Go code (internal/store/db), with sqlc (go run, pinned);
#   - the templ components' Go code (*_templ.go), with the templ pinned in
#     go.mod (go tool templ);
#   - the stylesheet (static/app.css), from internal/view/style.css, with
#     Tailwind's standalone CLI: no Node. The CLI is downloaded once into
#     bin/ and its SHA-256 checked against the release's, pinned here.
set -eu
TAILWIND_VERSION=v4.3.3
SQLC_VERSION=v1.31.1
cd "$(dirname "$0")/.."

case "$(uname -s)-$(uname -m)" in
Linux-x86_64) asset=tailwindcss-linux-x64 sum=dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a ;;
Linux-aarch64 | Linux-arm64) asset=tailwindcss-linux-arm64 sum=55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195 ;;
Darwin-arm64) asset=tailwindcss-macos-arm64 sum=cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d ;;
Darwin-x86_64) asset=tailwindcss-macos-x64 sum=7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1 ;;
*)
	echo "generate: no Tailwind CLI for $(uname -s) $(uname -m)" >&2
	exit 1
	;;
esac
tailwind="bin/$asset-$TAILWIND_VERSION"
if [ ! -x "$tailwind" ]; then
	mkdir -p bin
	curl -fsSL --retry 3 -o "$tailwind.part" \
		"https://github.com/tailwindlabs/tailwindcss/releases/download/$TAILWIND_VERSION/$asset"
	if command -v sha256sum >/dev/null 2>&1; then
		got="$(sha256sum "$tailwind.part" | cut -d' ' -f1)"
	else
		got="$(shasum -a 256 "$tailwind.part" | cut -d' ' -f1)"
	fi
	if [ "$got" != "$sum" ]; then
		rm -f "$tailwind.part"
		echo "generate: $asset $TAILWIND_VERSION is not the release's (SHA-256 $got)" >&2
		exit 1
	fi
	chmod +x "$tailwind.part"
	mv "$tailwind.part" "$tailwind"
fi

if command -v sqlc >/dev/null 2>&1 && [ "$(sqlc version)" = "$SQLC_VERSION" ]; then
	(cd internal/store && sqlc generate)
else
	(cd internal/store && go run "github.com/sqlc-dev/sqlc/cmd/sqlc@$SQLC_VERSION" generate)
fi
go tool templ generate -log-level warn
"$tailwind" --input internal/view/style.css --output static/app.css --minify 2>&1 |
	grep -v '^≈ tailwindcss\|^Done in\|^$' || true
