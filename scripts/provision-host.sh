#!/usr/bin/env bash
# Provisions a bare-metal Ubuntu 24.04 host for the docklet: KVM access,
# cgroupsv2, jailer + firecracker binaries, networking tools, state dirs.
# Idempotent; safe to re-run. Must run as root.
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.10.0}"
RUNSC_VERSION="${RUNSC_VERSION:-release-20260831.0}"
# Pinned sha256 of the upstream release archives for the default versions above.
FC_SHA256_X86_64="${FC_SHA256_X86_64:-6fb680b29ed3980e327db01b2b2dd1006f5be44e75120f90a274b033ee8e1cfa}"
FC_SHA256_AARCH64="${FC_SHA256_AARCH64:-bdf716de01531460666757f4bc9a26a0919a5e8b42d7717e4fd77f748c74f680}"
RUNSC_SHA256_X86_64="${RUNSC_SHA256_X86_64:-014b3871a5c698c802fd7a03758e0dbf4c1683f9e3f8c743979ea66bbf6553a4}"
DOCKLET_UID="${DOCKLET_UID:-1000}"
DOCKLET_GID="${DOCKLET_GID:-1000}"

if [ "$(id -u)" -ne 0 ]; then
  echo "provision-host: must run as root" >&2
  exit 1
fi

# Fails closed: the archive must match the expected sha256 before it is unpacked.
verify_sha256() {
  local file="$1" want="$2" got
  got="$(sha256sum "$file" | cut -d' ' -f1)"
  if [ "$got" != "$want" ]; then
    echo "provision-host: checksum mismatch for $(basename "$file"): want ${want}, got ${got}" >&2
    exit 1
  fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates curl iproute2 iptables kmod \
  cgroup-tools debootstrap squashfs-tools \
  qemu-utils e2fsprogs parted

# KVM must exist on real hardware; nested virt (which still exposes
# /dev/kvm in the guest) is supported for overflow pools. gvisor hosts
# (DOCKLET_RUNTIME=gvisor) need no KVM at all: Docker Engine plus the
# gVisor runsc runtime on any cheap kernel.
if [ "${DOCKLET_RUNTIME:-microvm}" = "gvisor" ]; then
  if ! command -v docker >/dev/null 2>&1; then
    echo "provision-host: docker missing, install Docker Engine for DOCKLET_RUNTIME=gvisor" >&2
    exit 1
  fi
  ARCH="$(uname -m)"
  if [ "${ARCH}" = "x86_64" ]; then
    curl -fsSL -o "${WORK}/gvisor.tar.bz2" "https://github.com/google/gvisor/releases/download/${RUNSC_VERSION}/gvisor-${ARCH}.tar.bz2"
    verify_sha256 "${WORK}/gvisor.tar.bz2" "${RUNSC_SHA256_X86_64}"
    tar -xjf "${WORK}/gvisor.tar.bz2" -C "${WORK}" runsc
    install -m 0755 "${WORK}/runsc" /usr/local/bin/runsc
    /usr/local/bin/runsc install || true
  else
    echo "provision-host: gvisor hosts are x86_64 only, got ${ARCH}" >&2
    exit 1
  fi
  # Register runsc in daemon.json without clobbering existing config.
  python3 - "${RUNSC_VERSION}" <<'EOF'
import json, sys
path = "/etc/docker/daemon.json"
try:
    with open(path) as f:
        cfg = json.load(f)
except (FileNotFoundError, json.JSONDecodeError):
    cfg = {}
runtimes = cfg.setdefault("runtimes", {})
runtimes["runsc"] = {"path": "/usr/local/bin/runsc"}
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
EOF
  systemctl restart docker || service docker restart || true
  docker info --format '{{.Runtimes}}' | grep -q runsc || {
    echo "provision-host: runsc runtime not registered with the daemon" >&2
    exit 1
  }
elif [ ! -e /dev/kvm ]; then
  echo "provision-host: /dev/kvm missing, need bare metal with virtualization (or nested virt exposing /dev/kvm)" >&2
  exit 1
fi
if [ "${DOCKLET_RUNTIME:-microvm}" != "gvisor" ]; then
  modprobe kvm 2>/dev/null || true
  chown "root:kvm" /dev/kvm || true
  chmod 660 /dev/kvm || true
fi

# cgroupsv2 is required for jailer resource limits.
if [ ! -f /sys/fs/cgroup/cgroup.controllers ]; then
  echo "provision-host: cgroupsv2 not mounted at /sys/fs/cgroup" >&2
  exit 1
fi

# Firecracker + jailer release binaries (microvm hosts only; gvisor
# hosts run the OCI image and skip the VMM entirely).
if [ "${DOCKLET_RUNTIME:-microvm}" != "gvisor" ]; then
ARCH="$(uname -m)"
RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
case "${ARCH}" in
  x86_64) FC_SHA256="${FC_SHA256_X86_64}" ;;
  aarch64) FC_SHA256="${FC_SHA256_AARCH64}" ;;
  *) echo "provision-host: unsupported arch ${ARCH}" >&2; exit 1 ;;
esac
curl -fsSL -o "${WORK}/firecracker.tgz" "${RELEASE_URL}/firecracker-${FC_VERSION}-${ARCH}.tgz"
verify_sha256 "${WORK}/firecracker.tgz" "${FC_SHA256}"
tar -xzf "${WORK}/firecracker.tgz" -C "${WORK}"
install -m 0755 "${WORK}/release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" /usr/bin/firecracker
install -m 0755 "${WORK}/release-${FC_VERSION}-${ARCH}/jailer-${FC_VERSION}-${ARCH}" /usr/bin/jailer
firecracker --version
jailer --version 2>/dev/null || true
fi

# State layout the docklet expects.
mkdir -p /var/lib/docklet/{images,vms,gens,state/sockets,state/logs,state/idem} /srv/jailer
# The docklet and jailer run as root; each dock drops to its own uid at boot.
# Re-own on every run so hosts installed with the old recursive chown are repaired.
chown root:root /var/lib/docklet
for d in images gens idem vms state; do
  [ -e "/var/lib/docklet/$d" ] && chown -R root:root "/var/lib/docklet/$d"
done
[ -e /var/lib/docklet/fleet-credential ] && chown root:root /var/lib/docklet/fleet-credential && chmod 0600 /var/lib/docklet/fleet-credential
mkdir -p /var/lib/docklet/idem
chmod 0700 /var/lib/docklet/images /var/lib/docklet/gens /var/lib/docklet/idem /var/lib/docklet/vms /var/lib/docklet/state
# The microVM boots from these via hard links inside the jail; they must stay world-readable.
for f in vmlinux initrd.img; do
  [ -e "/var/lib/docklet/images/$f" ] && chmod 0644 "/var/lib/docklet/images/$f"
done
# Each dock gets its own jailer uid at boot; the base stays root.
chown root:root /srv/jailer
chmod 0755 /srv/jailer

# Microcode freshness is a readiness input; warn loudly here, gate at boot.
if grep -rq "^Vulnerable" /sys/devices/system/cpu/vulnerabilities/ 2>/dev/null; then
  echo "provision-host: WARNING: CPU vulnerabilities report Vulnerable, update microcode + firmware" >&2
fi

echo "provision-host: done (firecracker ${FC_VERSION})"
