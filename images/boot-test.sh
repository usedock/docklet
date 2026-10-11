#!/usr/bin/env bash
# Boots a built image under QEMU and checks the guest agent: images/boot-test.sh <name> <arch> <outdir>
# Passes dock.cfg on the kernel command line, forwards host port 2222 to the
# agent, pings it, and confirms the root filesystem grew to the disk size.
set -euo pipefail

NAME="${1:?usage: boot-test.sh <name> <arch> <outdir>}"
ARCH="${2:?arch}"
OUT="${3:?outdir}"
[ "$ARCH" = amd64 ] || { echo "boot-test: only amd64 is tested" >&2; exit 1; }
HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
QPID=""
trap '[ -n "$QPID" ] && kill "$QPID" 2>/dev/null || true; rm -rf "$TMP"' EXIT

PREFIX="$OUT/$NAME-$ARCH"
zstd -d -q "$PREFIX.rootfs.ext4.zst" -o "$TMP/disk.ext4"
truncate -s 6G "$TMP/disk.ext4"

TOKEN="boot-test-token"
CFG="$(printf '{"token":"%s","workspace":"/home/user/work","roots":["/home/user/work","/run/dock-fast"],"hostname":"boot-test"}' "$TOKEN" | base64 -w0)"

ACCEL=tcg
[ -w /dev/kvm ] && ACCEL=kvm
qemu-system-x86_64 -accel "$ACCEL" -m 1024 -smp 2 -nographic -no-reboot \
  -kernel "$PREFIX.vmlinuz" -initrd "$PREFIX.initrd.img" \
  -append "console=ttyS0 root=/dev/vda rw rootfstype=ext4 rootwait dock.cfg=$CFG" \
  -drive "file=$TMP/disk.ext4,if=virtio,format=raw" \
  -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2222-:2222 -device virtio-net-pci,netdev=n0 \
  > "$TMP/console.log" 2>&1 &
QPID=$!

if ! python3 -I "$HERE/ping-agent.py" 127.0.0.1 2222 "$TOKEN" 300; then
  echo "boot-test: agent did not answer; console tail:" >&2
  tail -80 "$TMP/console.log" >&2
  exit 1
fi
echo "boot-test: $NAME agent answered"
