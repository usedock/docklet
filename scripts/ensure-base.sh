#!/usr/bin/env bash
# Makes sure a microvm host has the base image its docks boot from, plus the
# guest kernel staged from the host kernel:
#
#   ensure-base.sh
#
# 1. A host that already holds base.ext4 + vmlinux keeps it (a host-built or
#    earlier-pulled base stays current); only the kernel-mismatch warning shows.
# 2. Otherwise the signed Dockyard ubuntu-24.04 is pulled with `docklet image
#    pull` and base.ext4 is pointed at it. The same base on every host is what
#    lets a disk move between hosts and contexts (dock push / dock pull).
# 3. When Dockyard cannot be reached (or the docklet binary is too old to pull
#    the default image), the base is built on the host with build-image.sh.
#
# Env: OUT_DIR (image dir), DOCKLET_BIN (docklet binary), DOCKLET_DEFAULT_IMAGE
# (default ubuntu-24.04), DOCKLET_BUILD_BASE=1 or DOCKLET_REBUILD_IMAGE=1 (skip
# Dockyard and build on the host, replacing the current base).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${OUT_DIR:-/var/lib/docklet/images}"
DOCKLET_BIN="${DOCKLET_BIN:-$(command -v docklet || echo /usr/local/bin/docklet)}"
IMAGE="${DOCKLET_DEFAULT_IMAGE:-ubuntu-24.04}"
FORCE_BUILD=0
if [ "${DOCKLET_BUILD_BASE:-0}" = "1" ] || [ "${DOCKLET_REBUILD_IMAGE:-0}" = "1" ]; then
  FORCE_BUILD=1
fi
stamp="${OUT_DIR}/.kernel-release"
kernel="$(uname -r)"

log() { printf 'ensure-base: %s\n' "$*" >&2; }

stage_kernel() {
  if [ ! -f "${OUT_DIR}/vmlinux" ] || [ ! -f "$stamp" ] || [ "$(cat "$stamp")" != "$kernel" ]; then
    OUT_DIR="$OUT_DIR" bash "${here}/install-kernel.sh"
    printf '%s' "$kernel" > "$stamp"
  fi
}

if [ "$FORCE_BUILD" != "1" ] && [ -f "${OUT_DIR}/base.ext4" ] && [ -f "${OUT_DIR}/vmlinux" ]; then
  if [ -f "$stamp" ] && [ "$(cat "$stamp")" != "$kernel" ]; then
    log "WARNING: golden image was built for kernel $(cat "$stamp") but this host runs ${kernel}; re-run with DOCKLET_REBUILD_IMAGE=1 to rebuild"
  fi
  log "base image already present in ${OUT_DIR}; keeping it (DOCKLET_REBUILD_IMAGE=1 to rebuild on the host)"
  exit 0
fi

mkdir -p "$OUT_DIR"
if [ "$FORCE_BUILD" != "1" ]; then
  if [ -x "$DOCKLET_BIN" ] && DOCKLET_IMAGE_DIR="$OUT_DIR" "$DOCKLET_BIN" image pull "$IMAGE" >&2 && [ -f "${OUT_DIR}/base.ext4" ]; then
    stage_kernel
    log "base ready: ${OUT_DIR}/base.ext4 -> Dockyard ${IMAGE}"
    exit 0
  fi
  log "WARNING: could not pull Dockyard ${IMAGE} with ${DOCKLET_BIN}; building the base on this host instead"
fi

[ -f "${here}/build-image.sh" ] || { log "build-image.sh missing; cannot build the base image this host boots"; exit 1; }
[ -f "${here}/extract-vmlinux.sh" ] || { log "extract-vmlinux.sh missing; build-image.sh needs it"; exit 1; }
if ! command -v debootstrap >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends debootstrap e2fsprogs >&2
fi
log "building the base image for kernel ${kernel} (several minutes, ~20GB)"
OUT_DIR="$OUT_DIR" bash "${here}/build-image.sh"
printf '%s' "$kernel" > "$stamp"
log "base ready: ${OUT_DIR}/base.ext4 (built on this host)"
