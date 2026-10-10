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
# Fake cosigns report the pinned version so ensure_cosign accepts them.
ver='[ "$1" = version ] && { echo "GitVersion:    v2.4.1"; exit 0; }'
printf '#!/bin/sh\n%s\nexit 1\n' "$ver" > "$tmp/cosign-bad"; chmod +x "$tmp/cosign-bad"
printf '#!/bin/sh\n%s\nexit 0\n' "$ver" > "$tmp/cosign-ok"; chmod +x "$tmp/cosign-ok"
: > "$tmp/bundle.json"
expect_refuse verify_signature "$tmp/cosign-bad" "$tmp/docklet-linux-amd64" "$tmp/bundle.json" v0.0.1
expect_refuse verify_signature "$tmp/cosign-ok" "$tmp/docklet-linux-amd64" "$tmp/no-bundle.json" v0.0.1
expect_refuse verify_signature "" "$tmp/docklet-linux-amd64" "$tmp/bundle.json" v0.0.1
expect_ok verify_signature "$tmp/cosign-ok" "$tmp/docklet-linux-amd64" "$tmp/bundle.json" v0.0.1

# A recording fake cosign accepts only the identity for EXPECT_TAG and logs its args.
cat > "$tmp/cosign-rec" <<'SH'
#!/bin/sh
[ "$1" = version ] && { echo "GitVersion:    v2.4.1"; exit 0; }
echo "$@" >> "$COSIGN_LOG"
want="https://github.com/usedock/docklet/.github/workflows/release.yml@refs/tags/${EXPECT_TAG}"
id=""; issuer=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --certificate-identity) id="$2" ;;
    --certificate-oidc-issuer) issuer="$2" ;;
  esac
  shift
done
[ "$id" = "$want" ] && [ "$issuer" = "https://token.actions.githubusercontent.com" ] && exit 0
echo "no matching certificate identity" >&2
exit 1
SH
chmod +x "$tmp/cosign-rec"
export COSIGN_LOG="$tmp/cosign.log"
: > "$COSIGN_LOG"
EXPECT_TAG=v1.2.3 expect_ok verify_signature "$tmp/cosign-rec" "$tmp/docklet-linux-amd64" "$tmp/bundle.json" v1.2.3
grep -q -- '--certificate-identity https://github.com/usedock/docklet/.github/workflows/release.yml@refs/tags/v1.2.3 ' "$COSIGN_LOG" \
  && echo "ok: exact identity passed" || { echo "FAIL: exact identity not passed"; fail=1; }
grep -q -- 'identity-regexp' "$COSIGN_LOG" && { echo "FAIL: regexp identity used"; fail=1; }
EXPECT_TAG=v1.2.3 expect_refuse verify_signature "$tmp/cosign-rec" "$tmp/docklet-linux-amd64" "$tmp/bundle.json" v1.2.4
expect_refuse verify_signature "$tmp/cosign-rec" "$tmp/docklet-linux-amd64" "$tmp/bundle.json"
# cosign's stderr must be shown on failure.
shows_stderr() { local out; if out="$(EXPECT_TAG=v1.2.3 verify_signature "$tmp/cosign-rec" "$tmp/docklet-linux-amd64" "$tmp/bundle.json" v9.9.9 2>&1)"; then return 1; fi; grep -q 'no matching certificate identity' <<< "$out"; }
expect_ok shows_stderr

# Only the exact file name matches; look-alike names and other columns must not.
{
  printf '%s  docklet-linux-amd64.bundle\n' "$(printf 'a%.0s' {1..64})"
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
# A preinstalled cosign of another version is ignored in favour of the pinned download.
old_cosign_ignored() {
  mkdir -p "$tmp/oldbin"
  printf '#!/bin/sh\necho "GitVersion:    v2.3.0"\n' > "$tmp/oldbin/cosign"; chmod +x "$tmp/oldbin/cosign"
  COSIGN_SHA256_AMD64="$evil"
  uname() { echo x86_64; }
  curl() { while [ "$#" -gt 0 ]; do [ "$1" = -o ] && { printf 'evil-cosign' > "$2"; return 0; }; shift; done; }
  mkdir -p "$tmp/cosign-dl2"
  [ "$(PATH="$tmp/oldbin:$PATH" ensure_cosign "$tmp/cosign-dl2")" = "$tmp/cosign-dl2/cosign" ]
}
expect_ok old_cosign_ignored
# A preinstalled cosign is used only when its GitVersion field is exactly the pinned version.
preinstalled_used() {
  mkdir -p "$tmp/pre"
  printf '#!/bin/sh\ncat <<EOF\n  ____  ___  ____\ncosign: A tool for Container Signing\n\nGitVersion:    %s\nGitCommit:     abc\nGoVersion:     go1.22\nEOF\n' "$1" > "$tmp/pre/cosign"
  chmod +x "$tmp/pre/cosign"
  uname() { echo x86_64; }
  curl() { return 1; }
  [ "$(PATH="$tmp/pre:$PATH" ensure_cosign "$tmp/pre-dl" 2>/dev/null)" = "$tmp/pre/cosign" ]
}
expect_ok preinstalled_used v2.4.1
expect_refuse preinstalled_used v2.4.10
expect_refuse preinstalled_used v2x4x1
expect_refuse preinstalled_used v2.4.1-rc.1

# Only v<semver> release versions are accepted; anything else is refused before download or verify.
for v in v0.1.0 v1.22.333 v1.0.0-rc.1; do expect_ok valid_version "$v"; done
for v in "" 0.1.0 v1.0 v01.0.0 'v1.0.0 ' 'v1.0.0/../../x' 'v1.0.0$(id)' 'v.*' 'v1.0.0+build' 'v1.0.0-'; do
  expect_refuse valid_version "$v"
done
expect_refuse verify_signature "$tmp/cosign-ok" "$tmp/docklet-linux-amd64" "$tmp/bundle.json" 'v1.*'
no_download_on_bad_version() {
  curl() { echo called > "$tmp/curl-called"; return 0; }
  rm -f "$tmp/curl-called"
  if ( fetch_release 'v0.1.0/../evil' "$tmp" docklet-linux-amd64 ) 2>/dev/null; then return 1; fi
  [ ! -e "$tmp/curl-called" ]
}
expect_ok no_download_on_bad_version

# fetch_release end to end against a local release directory and a fake cosign on PATH.
rel="$tmp/rel/v0.0.1"
mkdir -p "$rel" "$tmp/fakebin"
cp "$tmp/docklet-linux-amd64" "$rel/"
(cd "$rel" && sha256sum docklet-linux-amd64 > checksums.txt)
: > "$rel/checksums.txt.bundle"
: > "$rel/docklet-linux-amd64.bundle"
fetch_with() {
  cp "$1" "$tmp/fakebin/cosign"
  local out
  out="$(mktemp -d "$tmp/out.XXXX")"
  local base="$tmp/rel"
  command -v cygpath >/dev/null 2>&1 && base="/$(cygpath -m "$base")"
  DOCKLET_RELEASE_BASE_URL="file://$base" PATH="$tmp/fakebin:$PATH" fetch_release v0.0.1 "$out" docklet-linux-amd64
}
# Fails only for the binary (its path is the last argument).
printf '#!/bin/sh\n%s\nfor a; do last="$a"; done\ncase "$last" in *docklet-linux-amd64) exit 1 ;; esac\nexit 0\n' "$ver" > "$tmp/cosign-binbad"
chmod +x "$tmp/cosign-binbad"
expect_ok fetch_with "$tmp/cosign-ok"
EXPECT_TAG=v0.0.1 expect_ok fetch_with "$tmp/cosign-rec"
EXPECT_TAG=v0.0.2 expect_refuse fetch_with "$tmp/cosign-rec"
expect_refuse fetch_with "$tmp/cosign-bad"
expect_refuse fetch_with "$tmp/cosign-binbad"
# The refusal must come from the binary's signature, not from rejecting the fake cosign.
binbad_reason() { local out; if out="$(fetch_with "$tmp/cosign-binbad" 2>&1)"; then return 1; fi; grep -q 'signature verification FAILED for .*/docklet-linux-amd64$' <<< "$out"; }
expect_ok binbad_reason
printf 'tampered' > "$tmp/tampered"; cp "$rel/docklet-linux-amd64" "$tmp/orig"; cp "$tmp/tampered" "$rel/docklet-linux-amd64"
expect_refuse fetch_with "$tmp/cosign-ok"
cp "$tmp/orig" "$rel/docklet-linux-amd64"; rm "$rel/docklet-linux-amd64.bundle"
expect_refuse fetch_with "$tmp/cosign-ok"

exit "$fail"
