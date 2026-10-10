#!/usr/bin/env bash
# Runs scripts/install.sh against a local file:// fake release and a fake cosign; run in CI.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0
expect_ok() { if ( "$@" ) >/dev/null 2>&1; then echo "ok: $*"; else echo "FAIL (expected success): $*"; fail=1; fi; }
expect_refuse() { if ( "$@" ) >/dev/null 2>&1; then echo "FAIL (expected refusal): $*"; fail=1; else echo "ok (refused): $*"; fi; }

url_of() { local p="$1"; command -v cygpath >/dev/null 2>&1 && p="/$(cygpath -m "$p")"; echo "file://$p"; }

mkdir -p "$tmp/fakebin"
# Fake cosigns report the pinned version; rec accepts only the identity for EXPECT_TAG.
ver='[ "$1" = version ] && { echo "GitVersion:    v2.4.1"; exit 0; }'
printf '#!/bin/sh\n%s\nexit 0\n' "$ver" > "$tmp/cosign-ok"
printf '#!/bin/sh\n%s\nexit 1\n' "$ver" > "$tmp/cosign-bad"
cat > "$tmp/cosign-rec" <<'SH'
#!/bin/sh
[ "$1" = version ] && { echo "GitVersion:    v2.4.1"; exit 0; }
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
# Fails only for the binary (its path is the last argument).
printf '#!/bin/sh\n%s\nfor a; do last="$a"; done\ncase "$last" in *dock-linux-amd64) exit 1 ;; esac\nexit 0\n' "$ver" > "$tmp/cosign-binbad"
chmod +x "$tmp"/cosign-*

rel="$tmp/rel/v0.0.1"
mkdir -p "$rel"
printf 'dock-cli-bytes' > "$rel/dock-linux-amd64"
(cd "$rel" && sha256sum dock-linux-amd64 > checksums.txt)
: > "$rel/checksums.txt.bundle"
: > "$rel/dock-linux-amd64.bundle"

n=0
# install_with <cosign> [VAR=value ...]: runs install.sh into a fresh dir exposed as $dest.
install_with() {
  local cos="$1"; shift
  n=$((n + 1)); dest="$tmp/dest$n"
  cp "$cos" "$tmp/fakebin/cosign"
  env DOCK_OS=linux DOCK_ARCH=amd64 DOCK_VERSION=v0.0.1 DOCK_RELEASE_BASE_URL="$(url_of "$tmp/rel")" \
    DOCK_INSTALL_DIR="$dest" PATH="$tmp/fakebin:$PATH" "$@" sh "$here/install.sh"
}
# Windows file systems have no exec bit, so the mode check is skipped under Git Bash.
is_exec() { [ -x "$1" ] || case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) true ;; *) false ;; esac; }
installs() { install_with "$@" && is_exec "$dest/dock" && [ "$(cat "$dest/dock")" = dock-cli-bytes ]; }
# A refusal must not come from a failed download unless the case removes a release file (ALLOW_DL=1).
refuses_clean() {
  local out
  if out="$(install_with "$@" 2>&1)"; then return 1; fi
  [ ! -e "$dest/dock" ] || return 1
  [ -n "${ALLOW_DL:-}" ] || ! grep -q 'could not download' <<< "$out"
}

expect_ok installs "$tmp/cosign-ok"
EXPECT_TAG=v0.0.1 expect_ok installs "$tmp/cosign-rec"
# Wrong tag: the signature is for another release.
EXPECT_TAG=v0.0.2 expect_refuse refuses_clean "$tmp/cosign-rec"
expect_refuse refuses_clean "$tmp/cosign-bad"
expect_refuse refuses_clean "$tmp/cosign-binbad"
# The refusal must name the binary signature, not an unrelated failure.
binbad_reason() { local out; if out="$(install_with "$tmp/cosign-binbad" 2>&1)"; then return 1; fi; grep -q 'signature verification FAILED for .*/dock-linux-amd64$' <<< "$out"; }
expect_ok binbad_reason
# Bad versions are refused before any download.
expect_refuse refuses_clean "$tmp/cosign-ok" 'DOCK_VERSION=v0.0.1/../x'
expect_refuse refuses_clean "$tmp/cosign-ok" 'DOCK_VERSION=0.0.1'
expect_refuse refuses_clean "$tmp/cosign-ok" 'DOCK_VERSION=v1.*'
# Unsupported platform.
expect_refuse refuses_clean "$tmp/cosign-ok" DOCK_OS=windows
expect_refuse refuses_clean "$tmp/cosign-ok" DOCK_ARCH=riscv64

# Latest release is resolved from the API response and validated.
printf '{\n  "tag_name": "v0.0.1",\n  "name": "v0.0.1"\n}\n' > "$tmp/latest.json"
printf '{"tag_name": "v0.0.1/../evil"}\n' > "$tmp/latest-bad.json"
expect_ok installs "$tmp/cosign-ok" DOCK_VERSION= "DOCK_LATEST_URL=$(url_of "$tmp/latest.json")"
expect_refuse refuses_clean "$tmp/cosign-ok" DOCK_VERSION= "DOCK_LATEST_URL=$(url_of "$tmp/latest-bad.json")"

# A failed install must leave an existing dock untouched.
keeps_existing() {
  mkdir -p "$tmp/keep"; printf 'old' > "$tmp/keep/dock"
  ! install_with "$tmp/cosign-bad" DOCK_INSTALL_DIR="$tmp/keep" && [ "$(cat "$tmp/keep/dock")" = old ]
}
expect_ok keeps_existing

# Tampered binary: signatures pass but the checksum does not match.
cp "$rel/dock-linux-amd64" "$tmp/orig"; printf 'tampered' > "$rel/dock-linux-amd64"
expect_refuse refuses_clean "$tmp/cosign-ok"
# Checksums with no entry for the asset.
cp "$tmp/orig" "$rel/dock-linux-amd64"; cp "$rel/checksums.txt" "$tmp/sums"
printf '%s  other-file\n' "$(sha256sum "$tmp/orig" | awk '{print $1}')" > "$rel/checksums.txt"
expect_refuse refuses_clean "$tmp/cosign-ok"
cp "$tmp/sums" "$rel/checksums.txt"
# Missing bundles and checksums.
rm "$rel/dock-linux-amd64.bundle"; ALLOW_DL=1 expect_refuse refuses_clean "$tmp/cosign-ok"; : > "$rel/dock-linux-amd64.bundle"
rm "$rel/checksums.txt.bundle"; ALLOW_DL=1 expect_refuse refuses_clean "$tmp/cosign-ok"; : > "$rel/checksums.txt.bundle"
rm "$rel/checksums.txt"; ALLOW_DL=1 expect_refuse refuses_clean "$tmp/cosign-ok"
cp "$tmp/sums" "$rel/checksums.txt"
expect_ok installs "$tmp/cosign-ok"

exit "$fail"
