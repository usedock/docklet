#!/usr/bin/env bash
# Builds one Dockyard image: images/build.sh <name> <arch> <outdir>
# Writes <outdir>/<name>-<arch>.{rootfs.ext4.zst,vmlinuz,initrd.img}.
# Needs root, debootstrap, zstd, e2fsprogs and Go (for the guest agent).
# Layered images (catalog base=...) are the base tree plus their setup script.
set -euo pipefail

NAME="${1:?usage: build.sh <name> <arch> <outdir>}"
ARCH="${2:?arch}"
OUT="${3:?outdir}"
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(dirname "$HERE")"
UBUNTU_RELEASE="${UBUNTU_RELEASE:-noble}"
CATALOG="$HERE/catalog.toml"

if [ "$(id -u)" -ne 0 ]; then
  echo "build.sh: must run as root" >&2
  exit 1
fi
case "$ARCH" in
  amd64) MIRROR=http://archive.ubuntu.com/ubuntu/ ;;
  arm64) MIRROR=http://ports.ubuntu.com/ubuntu-ports/ ;;
  *) echo "build.sh: unsupported arch $ARCH" >&2; exit 1 ;;
esac
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

# Minimal catalog reader: prints key of the [[image]] table named $1.
catalog_get() {
  awk -v want="$1" -v key="$2" '
    /^\[\[image\]\]/ { cur = ""; next }
    $1 == "name" { v = $0; sub(/^[^=]*= *"/, "", v); sub(/".*$/, "", v); cur = v; next }
    cur == want && $1 == key { v = $0; sub(/^[^=]*= */, "", v); gsub(/"/, "", v); print v; exit }
  ' "$CATALOG"
}

export DEBIAN_FRONTEND=noninteractive
WORK="$(mktemp -d)"
cleanup() {
  for d in "$WORK"/trees/*/proc "$WORK"/trees/*/dev "$WORK"/trees/*/sys; do
    umount -l "$d" 2>/dev/null || true
  done
  if mount | grep -q " $WORK"; then
    echo "build.sh: refusing cleanup, $WORK still mounted" >&2
    return 1
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

mount_chroot() { mount -t proc none "$1/proc"; mount --bind /dev "$1/dev"; mount -t devpts devpts "$1/dev/pts" 2>/dev/null || true; mount -t sysfs none "$1/sys"; }
umount_chroot() { umount "$1/dev/pts" 2>/dev/null || true; umount "$1/sys" "$1/dev" "$1/proc"; }
chroot_exec() { local t="$1"; shift; chroot "$t" env DEBIAN_FRONTEND=noninteractive "$@"; }

# dock-guest-init reads the engine's config from QEMU fw_cfg. When the module
# is not in the virtual kernel's core set, take it alone from modules-extra.
ensure_fw_cfg() {
  local tree="$1" kver mods
  kver="$(ls "$tree/lib/modules" | sort -V | tail -1)"
  mods="$tree/lib/modules/$kver"
  echo "build.sh: kernel $kver: $(grep -h 'CONFIG_FW_CFG_SYSFS=' "$tree/boot/config-$kver" 2>/dev/null || echo 'no config')"
  if grep -qs '/qemu_fw_cfg\.ko$' "$mods/modules.builtin" ||
    ls "$mods"/kernel/drivers/firmware/qemu_fw_cfg.ko* >/dev/null 2>&1; then
    return 0
  fi
  chroot_exec "$tree" sh -c "cd /tmp && apt-get download linux-modules-extra-$kver"
  dpkg-deb --fsys-tarfile "$tree"/tmp/linux-modules-extra-"$kver"_*.deb |
    tar -x -C "$tree" --keep-directory-symlink --wildcards '*/kernel/drivers/firmware/qemu_fw_cfg.ko*'
  rm -f "$tree"/tmp/linux-modules-extra-*.deb
  if ! ls "$mods"/kernel/drivers/firmware/qemu_fw_cfg.ko* >/dev/null 2>&1; then
    echo "build.sh: kernel $kver has no qemu_fw_cfg module (CONFIG_FW_CFG_SYSFS)" >&2
    exit 1
  fi
  chroot_exec "$tree" depmod "$kver"
  echo "build.sh: qemu_fw_cfg taken from linux-modules-extra-$kver"
}

build_base() {
  local tree="$1"
  debootstrap --arch="$ARCH" "$UBUNTU_RELEASE" "$tree" "$MIRROR"
  # debootstrap writes main only; add updates, security and universe.
  printf 'deb %s %s main universe
deb %s %s-updates main universe
deb %s %s-security main universe
'     "$MIRROR" "$UBUNTU_RELEASE" "$MIRROR" "$UBUNTU_RELEASE" "$MIRROR" "$UBUNTU_RELEASE" > "$tree/etc/apt/sources.list"
  mount_chroot "$tree"
  chroot_exec "$tree" apt-get update
  # debootstrap installs the release pocket only; take security and updates.
  chroot_exec "$tree" apt-get -y dist-upgrade
  chroot_exec "$tree" apt-get install -y --no-install-recommends \
    systemd systemd-sysv dbus ca-certificates curl git openssh-server sudo python3 docker.io \
    cloud-guest-utils e2fsprogs initramfs-tools linux-image-virtual iproute2 iputils-ping systemd-resolved
  # The package may replace resolv.conf; keep the build's working copy for later apt steps.
  rm -f "$tree/etc/resolv.conf"
  cp /etc/resolv.conf "$tree/etc/resolv.conf"
  ensure_fw_cfg "$tree"
  # Same user as scripts/build-image.sh: sudo and Docker without a password.
  chroot_exec "$tree" useradd -m -s /bin/bash -G sudo,docker user
  echo "user ALL=(ALL) NOPASSWD:ALL" > "$tree/etc/sudoers.d/user"
  chmod 0440 "$tree/etc/sudoers.d/user"

  # Guest agent and boot units.
  install -d "$tree/usr/local/libexec" "$tree/etc/systemd/network"
  (cd "$REPO" && go run ./cmd/gen-fast-agent) > "$tree/usr/local/libexec/dock-fast-agent.py"
  chmod 0700 "$tree/usr/local/libexec/dock-fast-agent.py"
  install -m 0755 "$HERE/overlay/dock-guest-init.sh" "$tree/usr/local/libexec/dock-guest-init"
  install -m 0644 "$HERE/overlay/dock-guest-init.service" "$HERE/overlay/dock-fast-agent.service" "$tree/etc/systemd/system/"
  install -m 0644 "$HERE/overlay/99-dock-dhcp.network" "$tree/etc/systemd/network/"
  chroot_exec "$tree" systemctl enable dock-guest-init.service dock-fast-agent.service docker.service \
    systemd-networkd.service systemd-resolved.service ssh.service
  chroot_exec "$tree" systemctl mask apt-daily.service apt-daily.timer apt-daily-upgrade.timer \
    motd-news.timer e2scrub_all.timer e2scrub_reap.service systemd-networkd-wait-online.service || true
  umount_chroot "$tree"
}

build_tree() {
  local name="$1" tree="$WORK/trees/$1" base setup data
  [ -d "$tree" ] && return 0
  mkdir -p "$WORK/trees"
  base="$(catalog_get "$name" base)"
  setup="$(catalog_get "$name" setup)"
  if [ -z "$base" ]; then
    mkdir -p "$tree"
    build_base "$tree"
    return 0
  fi
  build_tree "$base"
  cp -a "$WORK/trees/$base" "$tree"
  if [ -n "$setup" ]; then
    install -m 0755 "$HERE/$setup" "$tree/tmp/dock-setup.sh"
    # Optional data for the script (e.g. a lockfile): images/<setup name without .sh and setup- prefix>/.
    data="$HERE/$(basename "${setup#setup-}" .sh)"
    [ -d "$data" ] && cp -a "$data" "$tree/tmp/dock-setup-data"
    mount_chroot "$tree"
    # apt and downloads need DNS inside the chroot.
    cp /etc/resolv.conf "$tree/etc/resolv.conf" 2>/dev/null || true
    chroot_exec "$tree" /tmp/dock-setup.sh
    umount_chroot "$tree"
    rm -rf "$tree/tmp/dock-setup.sh" "$tree/tmp/dock-setup-data"
  fi
}

SIZE_MIB="$(catalog_get "$NAME" size_mib)"
[ -n "$SIZE_MIB" ] || { echo "build.sh: $NAME not in catalog" >&2; exit 1; }
build_tree "$NAME"
TREE="$WORK/trees/$NAME"

# Finish: clean caches, export the kernel, drop build identity.
mount_chroot "$TREE"
chroot_exec "$TREE" apt-get -y dist-upgrade
# Refuse to ship an image with pending package updates.
pending="$(chroot_exec "$TREE" apt-get -s dist-upgrade | grep -c '^Inst ' || true)"
if [ "$pending" != 0 ]; then
  echo "build.sh: $NAME has $pending pending package updates" >&2
  exit 1
fi
chroot_exec "$TREE" apt-get clean
umount_chroot "$TREE"
printf '{"image":"%s","arch":"%s","builtAt":"%s"}\n' "$NAME" "$ARCH" "$(date -u +%FT%TZ)" > "$TREE/etc/dock-image.json"
rm -rf "$TREE/usr/share/doc" "$TREE/usr/share/man" "$TREE/var/cache/apt/archives" \
  "$TREE/var/lib/apt/lists" "$TREE/tmp"/* "$TREE/var/tmp"/* "$TREE/root/.cache"
# No resolved stub in the guest; fleet and DHCP networking both work with a static file.
rm -f "$TREE/etc/resolv.conf"
printf 'nameserver 1.1.1.1\n' > "$TREE/etc/resolv.conf"
: > "$TREE/etc/machine-id"
rm -f "$TREE/var/lib/dbus/machine-id" "$TREE"/etc/ssh/ssh_host_*
printf 'dock\n' > "$TREE/etc/hostname"
printf '/dev/vda / ext4 defaults 0 1\n' > "$TREE/etc/fstab"
# Regenerate sshd host keys on first boot.
install -d "$TREE/etc/systemd/system/ssh.service.d"
printf '[Service]\nExecStartPre=/usr/bin/ssh-keygen -A\n' > "$TREE/etc/systemd/system/ssh.service.d/hostkeys.conf"

KERNEL="$(ls "$TREE"/boot/vmlinuz-* | sort -V | tail -1)"
INITRD="$(ls "$TREE"/boot/initrd.img-* | sort -V | tail -1)"
install -m 0644 "$KERNEL" "$OUT/$NAME-$ARCH.vmlinuz"
install -m 0644 "$INITRD" "$OUT/$NAME-$ARCH.initrd.img"
rm -f "$TREE"/boot/vmlinuz* "$TREE"/boot/initrd.img*

RAW="$WORK/rootfs.ext4"
truncate -s "${SIZE_MIB}M" "$RAW"
mkfs.ext4 -F -q -L dockroot -d "$TREE" "$RAW"
zstd -19 -T0 -q -f "$RAW" -o "$OUT/$NAME-$ARCH.rootfs.ext4.zst"
echo "build.sh: built $NAME ($ARCH) into $OUT"
