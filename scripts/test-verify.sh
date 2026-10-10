#!/usr/bin/env bash
# Asserts the release verifier refuses bad input; run in CI.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
log() { :; }
die() { echo "die: $*" >&2; exit 1; }
# shellcheck source=lib-verify.sh
. "${here}/lib-verify.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0
expect_ok() { if ( "$@" ) 2>/dev/null; then echo "ok: $*"; else echo "FAIL (expected success): $*"; fail=1; fi; }
expect_refuse() { if ( "$@" ) 2>/dev/null; then echo "FAIL (expected refusal): $*"; fail=1; else echo "ok (refused): $*"; fi; }

printf 'docklet-bytes' > "$tmp/docklet-linux-amd64"
good="$(file_sha256 "$tmp/docklet-linux-amd64")"
printf '%s  docklet-linux-amd64\n' "$good" > "$tmp/good.txt"
printf '%s  docklet-linux-amd64\n' "$(printf 'x%.0s' {1..64})" > "$tmp/bad.txt"
printf '%s  other-file\n' "$good" > "$tmp/missing.txt"

expect_ok verify_checksum "$tmp/docklet-linux-amd64" "$tmp/good.txt" docklet-linux-amd64
expect_refuse verify_checksum "$tmp/docklet-linux-amd64" "$tmp/bad.txt" docklet-linux-amd64
expect_refuse verify_checksum "$tmp/docklet-linux-amd64" "$tmp/missing.txt" docklet-linux-amd64
expect_refuse verify_checksum "$tmp/docklet-linux-amd64" "$tmp/nope.txt" docklet-linux-amd64

# A fake cosign that rejects everything must make verify_signature refuse.
printf '#!/bin/sh\nexit 1\n' > "$tmp/cosign-bad"; chmod +x "$tmp/cosign-bad"
printf '#!/bin/sh\nexit 0\n' > "$tmp/cosign-ok"; chmod +x "$tmp/cosign-ok"
: > "$tmp/bundle.json"
expect_refuse verify_signature "$tmp/cosign-bad" "$tmp/docklet-linux-amd64" "$tmp/bundle.json"
expect_refuse verify_signature "$tmp/cosign-ok" "$tmp/docklet-linux-amd64" "$tmp/no-bundle.json"
expect_refuse verify_signature "" "$tmp/docklet-linux-amd64" "$tmp/bundle.json"
expect_ok verify_signature "$tmp/cosign-ok" "$tmp/docklet-linux-amd64" "$tmp/bundle.json"

exit "$fail"
