#!/usr/bin/env bash
# Boots a built image under QEMU and checks the guest agent: images/boot-test.sh <name> <arch> <outdir>
# Passes the guest config as the fw_cfg file opt/usedock/cfg (plain JSON),
# forwards host port 2222 to the agent, pings it, and runs the checks in
# ping-agent.py. The ubuntu-24.04 image also boots a second time with the
# older dock.cfg=<base64 json> kernel argument. arm64 images boot on the QEMU
# virt machine (device tree, fw_cfg over MMIO), with KVM when /dev/kvm is
# writable and the slower TCG emulation otherwise.
set -euo pipefail

NAME="${1:?usage: boot-test.sh <name> <arch> <outdir>}"
ARCH="${2:?arch}"
OUT="${3:?outdir}"
HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
QPID=""
trap '[ -n "$QPID" ] && kill "$QPID" 2>/dev/null || true; rm -rf "$TMP"' EXIT

PREFIX="$OUT/$NAME-$ARCH"
ACCEL=tcg
[ -w /dev/kvm ] && ACCEL=kvm
case "$ARCH" in
  amd64)
    QEMU=(qemu-system-x86_64 -accel "$ACCEL")
    CONSOLE=ttyS0
    WAIT=300
    ;;
  arm64)
    CPU=max
    [ "$ACCEL" = kvm ] && CPU=host
    QEMU=(qemu-system-aarch64 -machine virt -accel "$ACCEL" -cpu "$CPU")
    CONSOLE=ttyAMA0
    # TCG on an arm64 runner without /dev/kvm boots several times slower.
    WAIT=300
    [ "$ACCEL" = tcg ] && WAIT=1200
    ;;
  *) echo "boot-test: unsupported arch $ARCH" >&2; exit 1 ;;
esac
echo "boot-test: $NAME ($ARCH) accel=$ACCEL"

guest_cfg() {
  printf '{"token":"%s","workspace":"/home/user/work","roots":["/home/user/work","/run/dock-fast"],"hostname":"boot-test"}' "$1"
}

# boot <mode> <token>: mode fwcfg or cmdline.
boot() {
  local mode="$1" token="$2" append="console=$CONSOLE root=/dev/vda rw rootfstype=ext4 rootwait" fw=()
  zstd -d -q -f "$PREFIX.rootfs.ext4.zst" -o "$TMP/disk.ext4"
  truncate -s 6G "$TMP/disk.ext4"
  if [ "$mode" = fwcfg ]; then
    (umask 077 && guest_cfg "$token" > "$TMP/guest.json")
    fw=(-fw_cfg "name=opt/usedock/cfg,file=$TMP/guest.json")
  else
    append="$append dock.cfg=$(guest_cfg "$token" | base64 -w0)"
  fi
  "${QEMU[@]}" -m 1024 -smp 2 -nographic -no-reboot \
    -kernel "$PREFIX.vmlinuz" -initrd "$PREFIX.initrd.img" -append "$append" "${fw[@]}" \
    -drive "file=$TMP/disk.ext4,if=virtio,format=raw" \
    -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2222-:2222 -device virtio-net-pci,netdev=n0 \
    > "$TMP/console.log" 2>&1 &
  QPID=$!
  if ! python3 -I "$HERE/ping-agent.py" 127.0.0.1 2222 "$token" "$WAIT" "$mode"; then
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

START=$SECONDS
boot fwcfg "boot-test-fwcfg-token"
if [ "$NAME" = ubuntu-24.04 ]; then
  boot cmdline "boot-test-cmdline-token"
fi
echo "boot-test: $NAME ($ARCH) passed in $((SECONDS - START))s"
