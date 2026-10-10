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
expect_ok() { if ( "$@" ); then echo "ok: $*"; else echo "FAIL (expected success): $*"; fail=1; fi; }
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

# Only the exact file name matches; look-alike names and other columns must not.
{
  printf '%s  docklet-linux-amd64.sigstore.json\n' "$(printf 'a%.0s' {1..64})"
  printf '%s  xdocklet-linux-amd64\n' "$(printf 'b%.0s' {1..64})"
  printf '%s  docklet-linux-amd64-old\n' "$(printf 'c%.0s' {1..64})"
} > "$tmp/decoys.txt"
cat "$tmp/decoys.txt" "$tmp/good.txt" > "$tmp/decoys-good.txt"
printf '%s *docklet-linux-amd64\n' "$good" > "$tmp/binary-mode.txt"
expect_refuse verify_checksum "$tmp/docklet-linux-amd64" "$tmp/decoys.txt" docklet-linux-amd64
expect_ok verify_checksum "$tmp/docklet-linux-amd64" "$tmp/decoys-good.txt" docklet-linux-amd64
expect_ok verify_checksum "$tmp/docklet-linux-amd64" "$tmp/binary-mode.txt" docklet-linux-amd64

# ensure_cosign must refuse a downloaded cosign whose sha256 is not the pinned one.
no_cosign_path=""
IFS=: read -ra path_dirs <<< "$PATH"
for d in "${path_dirs[@]}"; do [ -x "$d/cosign" ] || no_cosign_path="${no_cosign_path:+$no_cosign_path:}$d"; done
fake_curl_ensure() {
  uname() { echo x86_64; }
  curl() { while [ "$#" -gt 0 ]; do [ "$1" = -o ] && { printf 'evil-cosign' > "$2"; return 0; }; shift; done; }
  mkdir -p "$tmp/cosign-dl"
  PATH="$no_cosign_path" ensure_cosign "$tmp/cosign-dl"
}
evil="$(printf 'evil-cosign' | sha256sum | awk '{print $1}')"
refuses_with_mismatch() { local out; if out="$(fake_curl_ensure 2>&1)"; then return 1; fi; grep -q 'checksum mismatch' <<< "$out"; }
expect_ok refuses_with_mismatch
pinned_ensure() { COSIGN_SHA256_AMD64="$evil"; [ "$(fake_curl_ensure)" = "$tmp/cosign-dl/cosign" ]; }
expect_ok pinned_ensure

# fetch_release end to end against a local release directory and a fake cosign on PATH.
rel="$tmp/rel/v0.0.1"
mkdir -p "$rel" "$tmp/fakebin"
cp "$tmp/docklet-linux-amd64" "$rel/"
(cd "$rel" && sha256sum docklet-linux-amd64 > checksums.txt)
: > "$rel/checksums.txt.sigstore.json"
: > "$rel/docklet-linux-amd64.sigstore.json"
fetch_with() {
  cp "$1" "$tmp/fakebin/cosign"
  local out
  out="$(mktemp -d "$tmp/out.XXXX")"
  local base="$tmp/rel"
  command -v cygpath >/dev/null 2>&1 && base="/$(cygpath -m "$base")"
  DOCKLET_RELEASE_BASE_URL="file://$base" PATH="$tmp/fakebin:$PATH" fetch_release v0.0.1 "$out" docklet-linux-amd64
}
# Fails only for the binary (its path is the last argument).
printf '#!/bin/sh\nfor a; do last="$a"; done\ncase "$last" in *docklet-linux-amd64) exit 1 ;; esac\nexit 0\n' > "$tmp/cosign-binbad"
chmod +x "$tmp/cosign-binbad"
expect_ok fetch_with "$tmp/cosign-ok"
expect_refuse fetch_with "$tmp/cosign-bad"
expect_refuse fetch_with "$tmp/cosign-binbad"
printf 'tampered' > "$tmp/tampered"; cp "$rel/docklet-linux-amd64" "$tmp/orig"; cp "$tmp/tampered" "$rel/docklet-linux-amd64"
expect_refuse fetch_with "$tmp/cosign-ok"
cp "$tmp/orig" "$rel/docklet-linux-amd64"; rm "$rel/docklet-linux-amd64.sigstore.json"
expect_refuse fetch_with "$tmp/cosign-ok"

exit "$fail"
