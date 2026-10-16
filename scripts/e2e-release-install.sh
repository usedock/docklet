#!/usr/bin/env bash
# Runs bootstrap's release install against real GitHub releases with full cosign and sha256 verification:
# fresh install, skip an older pin, skip the same pin, and a forced downgrade. Needs network, not root or KVM.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
log() { printf 'e2e-release-install: %s\n' "$*"; }
die() { printf 'e2e-release-install: FATAL: %s\n' "$*" >&2; exit 1; }
# shellcheck source=lib-verify.sh
. "${here}/lib-verify.sh"

# Fixed published releases, not bootstrap's pin: a pin bump lands before its tag exists.
NEW="${E2E_NEW_VERSION:-v0.3.8}"
OLD="${E2E_OLD_VERSION:-v0.3.7}"
[ "$(semver_cmp "$OLD" "$NEW")" = -1 ] || die "E2E_OLD_VERSION ${OLD} must be older than E2E_NEW_VERSION ${NEW}"

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
dest="${root}/bin/docklet"

# Runs one install into a fresh workdir; prints nothing, leaves the workdir for inspection.
run_install() { local w="${root}/$1"; mkdir -p "$w"; install_release_docklet "$dest" "$2" "$3" "$w"; }
expect_version() {
  local got
  got="$("$dest" --version)"
  [ "$got" = "$1" ] || die "docklet --version printed '${got}', want '$1'"
  valid_version "$got" || die "docklet --version '${got}' does not parse as a release version; the downgrade guard would never fire"
  log "docklet --version: ${got}"
}
# A skipped run must not download anything or touch the binary.
expect_skipped() {
  local w="${root}/$1" before="$2"
  [ -z "$(ls -A "$w")" ] || die "$1: files were downloaded on a skipped run: $(ls -A "$w")"
  [ "$(file_sha256 "$dest")" = "$before" ] || die "$1: binary changed on a skipped run"
}

log "(a) fresh install of ${NEW}"
run_install a "$NEW" 0
expect_version "$NEW"
sum="$(file_sha256 "$dest")"
log "(a) PASS: installed ${NEW} (sha256 ${sum})"

log "(b) rerun with DOCKLET_VERSION=${OLD}"
run_install b "$OLD" 0
expect_skipped b "$sum"
expect_version "$NEW"
log "(b) PASS: skipped, newer ${NEW} kept, binary unchanged"

log "(c) rerun with DOCKLET_VERSION=${NEW}"
run_install c "$NEW" 0
expect_skipped c "$sum"
expect_version "$NEW"
log "(c) PASS: skipped, same version, binary unchanged"

log "(d) DOCKLET_ALLOW_DOWNGRADE=1 with DOCKLET_VERSION=${OLD}"
run_install d "$OLD" 1
expect_version "$OLD"
[ "$(file_sha256 "$dest")" != "$sum" ] || die "(d) binary did not change"
log "(d) PASS: downgraded to ${OLD} (sha256 $(file_sha256 "$dest"))"
