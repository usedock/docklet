#!/usr/bin/env bash
# Builds the minimal Ubuntu 24.04 image: systemd, sshd, Docker, git, curl and
# python3 (the guest exec agent). The image name comes from image-versions.env.
# Needs root, debootstrap, and network. Installs the image as
# $OUT_DIR/bases/<sha256>/base.ext4 (install-base.sh), repoints the
# $OUT_DIR/base.ext4 symlink at it, and stages $OUT_DIR/{vmlinux,initrd.img}.
set -euo pipefail

OUT_DIR="${OUT_DIR:-/var/lib/docklet/images}"
VERSIONS_ENV="${VERSIONS_ENV:-$(dirname "$0")/image-versions.env}"
UBUNTU_RELEASE="${UBUNTU_RELEASE:-noble}"

if [ "$(id -u)" -ne 0 ]; then
  echo "build-image: must run as root" >&2
  exit 1
fi
if [ ! -f "${VERSIONS_ENV}" ]; then
  echo "build-image: missing ${VERSIONS_ENV}" >&2
  exit 1
fi
# shellcheck disable=SC1090
source "${VERSIONS_ENV}"

export DEBIAN_FRONTEND=noninteractive
WORK="$(mktemp -d)"
ROOT="$WORK/rootfs"
mkdir -p "$ROOT" "$OUT_DIR"
# Never rm the workdir while bind mounts may still exist: that path leads
# straight into the host /dev. Lazy-unmount first, refuse if still mounted.
cleanup() {
  umount -l "$ROOT/proc" "$ROOT/dev" 2>/dev/null || true
  if mount | grep -q " $ROOT"; then
    echo "build-image: refusing cleanup, $ROOT still mounted" >&2
    return 1
  fi
  if [ -n "${WORK:-}" ] && [ "$WORK" != "/" ] && [ "$WORK" != "/tmp" ]; then
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

debootstrap --arch=amd64 "$UBUNTU_RELEASE" "$ROOT" http://archive.ubuntu.com/ubuntu/
mount -t proc none "$ROOT/proc" || true
mount --bind /dev "$ROOT/dev" || true

chroot_exec() { chroot "$ROOT" env DEBIAN_FRONTEND=noninteractive "$@"; }

# debootstrap seeds main only; docker.io lives in universe.
sed -i 's/^Components:.*/Components: main universe restricted multiverse/' "$ROOT/etc/apt/sources.list.d/ubuntu.sources" 2>/dev/null || \
  echo "deb http://archive.ubuntu.com/ubuntu/ $UBUNTU_RELEASE universe multiverse" >> "$ROOT/etc/apt/sources.list"
chroot_exec apt-get update
chroot_exec apt-get install -y --no-install-recommends \
  ca-certificates curl git openssh-server sudo systemd docker.io python3
# Provenance stamp.
BUILD_DATE="$(date -u +%Y%m%d)"
cat > "$ROOT/etc/dock-image.json" << JSON
{"image":"${DOCK_IMAGE}","builtAt":"$(date -u +%FT%TZ)"}
JSON
chroot_exec systemctl enable systemd-networkd
chroot_exec useradd -m -s /bin/bash -G sudo,docker user || true
echo "user ALL=(ALL) NOPASSWD:ALL" > "$ROOT/etc/sudoers.d/user"
chmod 0440 "$ROOT/etc/sudoers.d/user"

# Fast boot: mask units that stall microVM startup (snap seeding, apt
# timers, motd) and drop caches to shrink the rootfs copy on every provision.
chroot_exec systemctl mask snapd.service snapd.socket snapd.seeded.service \
  apt-daily.service apt-daily.timer apt-daily-upgrade.timer \
  motd-news.timer e2scrub_all.timer e2scrub_reap.service || true
chroot_exec apt-get purge -y snapd || true
chroot_exec apt-get autoremove -y --purge || true
rm -rf "$ROOT/usr/share/doc" "$ROOT/usr/share/man" "$ROOT/var/cache/apt/archives" \
  "$ROOT/var/lib/apt/lists" "$ROOT/tmp"/* "$ROOT/var/tmp"/* \
  "$ROOT/root/.cache"

# Grow the root filesystem online at boot: disk.Grow only enlarges the rootfs
# file. The kernel mounts /dev/vda (see baseKernelArgs); x-systemd.growfs makes
# systemd-growfs resize it to the device, so `df /` matches diskGiB.
printf '/dev/vda / ext4 defaults,x-systemd.growfs 0 1
' > "$ROOT/etc/fstab"

# Pack the rootfs and fetch a matching kernel (same build host for now).
chroot_exec apt-get clean
umount "$ROOT/proc" "$ROOT/dev" || true
fallocate -l 20G "$WORK/base.ext4" || dd if=/dev/zero of="$WORK/base.ext4" bs=1M count=20480
mkfs.ext4 -F -L dockroot "$WORK/base.ext4"
mkdir -p "$WORK/mnt"
mount -o loop "$WORK/base.ext4" "$WORK/mnt"
cp -a "$ROOT/." "$WORK/mnt/"
umount "$WORK/mnt"
# Content-addressed install: the build is not reproducible, so the old base
# must survive for the generations built against it, and the pointer flips
# only once the new image is complete.
FP="$(OUT_DIR="$OUT_DIR" bash "$(dirname "$0")/install-base.sh" "$WORK/base.ext4")"
OUT_DIR="$OUT_DIR" bash "$(dirname "$0")/install-kernel.sh"

echo "build-image: installed $OUT_DIR/bases/$FP/base.ext4 as the current base + $OUT_DIR/vmlinux (${DOCK_IMAGE} ${BUILD_DATE})"
