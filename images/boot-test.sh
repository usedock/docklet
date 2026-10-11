#!/usr/bin/env bash
# Boots a built image under QEMU and checks the guest agent: images/boot-test.sh <name> <arch> <outdir>
# Passes the guest config as the fw_cfg file opt/usedock/cfg (plain JSON),
# forwards host port 2222 to the agent, pings it, and runs the checks in
# ping-agent.py. The ubuntu-24.04 image also boots a second time with the
# older dock.cfg=<base64 json> kernel argument.
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
ACCEL=tcg
[ -w /dev/kvm ] && ACCEL=kvm

guest_cfg() {
  printf '{"token":"%s","workspace":"/home/user/work","roots":["/home/user/work","/run/dock-fast"],"hostname":"boot-test"}' "$1"
}

# boot <mode> <token>: mode fwcfg or cmdline.
boot() {
  local mode="$1" token="$2" append="console=ttyS0 root=/dev/vda rw rootfstype=ext4 rootwait" fw=()
  zstd -d -q -f "$PREFIX.rootfs.ext4.zst" -o "$TMP/disk.ext4"
  truncate -s 6G "$TMP/disk.ext4"
  if [ "$mode" = fwcfg ]; then
    (umask 077 && guest_cfg "$token" > "$TMP/guest.json")
    fw=(-fw_cfg "name=opt/usedock/cfg,file=$TMP/guest.json")
  else
    append="$append dock.cfg=$(guest_cfg "$token" | base64 -w0)"
  fi
  qemu-system-x86_64 -accel "$ACCEL" -m 1024 -smp 2 -nographic -no-reboot \
    -kernel "$PREFIX.vmlinuz" -initrd "$PREFIX.initrd.img" -append "$append" "${fw[@]}" \
    -drive "file=$TMP/disk.ext4,if=virtio,format=raw" \
    -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2222-:2222 -device virtio-net-pci,netdev=n0 \
    > "$TMP/console.log" 2>&1 &
  QPID=$!
  if ! python3 -I "$HERE/ping-agent.py" 127.0.0.1 2222 "$token" 300 "$mode"; then
    echo "boot-test: $NAME ($mode) failed; console tail:" >&2
    tail -80 "$TMP/console.log" >&2
    exit 1
  fi
  if [ "$mode" = fwcfg ] && grep -qF "$token" "$TMP/console.log"; then
    echo "boot-test: $NAME ($mode) printed the token on the console" >&2
    exit 1
  fi
  kill "$QPID" 2>/dev/null || true
  wait "$QPID" 2>/dev/null || true
  QPID=""
  echo "boot-test: $NAME ($mode) agent answered"
}

boot fwcfg "boot-test-fwcfg-token"
if [ "$NAME" = ubuntu-24.04 ]; then
  boot cmdline "boot-test-cmdline-token"
fi
