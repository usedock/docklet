#!/usr/bin/env bash
# Stages the running host kernel and its initrd as the guest kernel
# ($OUT_DIR/vmlinux + initrd.img). Not part of the base image identity: resume
# is a fresh boot, so a generation restores under whatever kernel the host has.
# Each file is written beside its target, fsynced, and renamed, so a boot
# racing this (or a power loss) never sees a half-written kernel.
set -euo pipefail

OUT_DIR="${OUT_DIR:-/var/lib/docklet/images}"
RELEASE="$(uname -r)"
mkdir -p "$OUT_DIR"

cp "/boot/vmlinuz-${RELEASE}" "$OUT_DIR/.vmlinuz.next"
bash "$(dirname "$0")/extract-vmlinux.sh" "$OUT_DIR/.vmlinuz.next" "$OUT_DIR/.vmlinux.next"
rm -f "$OUT_DIR/.vmlinuz.next"
cp "/boot/initrd.img-${RELEASE}" "$OUT_DIR/.initrd.img.next"
chmod 0644 "$OUT_DIR/.vmlinux.next" "$OUT_DIR/.initrd.img.next"
sync "$OUT_DIR/.vmlinux.next" "$OUT_DIR/.initrd.img.next"
mv -T "$OUT_DIR/.vmlinux.next" "$OUT_DIR/vmlinux"
mv -T "$OUT_DIR/.initrd.img.next" "$OUT_DIR/initrd.img"
sync "$OUT_DIR"
echo "install-kernel: staged ${RELEASE} as $OUT_DIR/vmlinux + initrd.img" >&2
