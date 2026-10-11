#!/usr/bin/env bash
# One-shot installer for a docklet host: turns a fresh Ubuntu server
# into a docklet host that talks to the control plane outbound-only.
#
#   bootstrap-host.sh --fleet-url https://api.example.com --fleet-token <token> \
#     [--host-id host-1] [--runtime microvm|gvisor] [--pool-size 8]
#
# Idempotent; safe to re-run. Snapshot storage and Restate live on the control
# plane, so this script never installs MinIO or Restate on the host.
#
# A microvm host also gets the image its microVMs boot from
# (build-image.sh); a gvisor host gets the OCI image (Dockerfile.gvisor).
# Both are skipped once current, so re-runs stay fast.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DOCKLET_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_DIR="/etc/docklet"
ENV_FILE="${ENV_DIR}/docklet.env"
UNIT_FILE="/etc/systemd/system/docklet.service"
DOCKLET_DEST="/usr/local/bin/docklet"
RUNSC_PATH="/usr/local/bin/runsc"
DOCKLET_ADDR_VALUE="127.0.0.1:8081"
READINESS_URL="http://${DOCKLET_ADDR_VALUE}/v1/readiness"

FC_VERSION="${FC_VERSION:-v1.10.0}"
RUNSC_VERSION="${RUNSC_VERSION:-release-20260831.0}"
# Pinned sha256 of the upstream release archives for the default versions above.
FC_SHA256_X86_64="${FC_SHA256_X86_64:-6fb680b29ed3980e327db01b2b2dd1006f5be44e75120f90a274b033ee8e1cfa}"
FC_SHA256_AARCH64="${FC_SHA256_AARCH64:-bdf716de01531460666757f4bc9a26a0919a5e8b42d7717e4fd77f748c74f680}"
RUNSC_SHA256_X86_64="${RUNSC_SHA256_X86_64:-014b3871a5c698c802fd7a03758e0dbf4c1683f9e3f8c743979ea66bbf6553a4}"
GVISOR_IMAGE="${DOCKLET_GVISOR_IMAGE:-dock-gvisor:latest}"
GVISOR_INPUTS_LABEL="dev.usedock.gvisor-inputs"
# Toolchain image used only when go is missing on the host.
GO_IMAGE="${DOCKLET_GO_IMAGE:-golang:1.25-bookworm}"
FLEET_URL="${DOCKLET_FLEET_URL:-}"
FLEET_TOKEN="${DOCKLET_FLEET_TOKEN:-}"
HOST_ID="${DOCKLET_HOST_ID:-}"
RUNTIME="${DOCKLET_RUNTIME:-}"
POOL_SIZE="${DOCKLET_POOL_SIZE:-}"
EGRESS_ALLOW_PRIVATE="${DOCKLET_EGRESS_ALLOW_PRIVATE:-}"
ALLOW_VULNERABLE="${DOCKLET_ALLOW_VULNERABLE:-}"
PREBUILT_BIN="${DOCKLET_BIN:-}"
# Release to install; bump with each docklet release. Verified by checksum and cosign.
DOCKLET_VERSION="${DOCKLET_VERSION:-v0.1.0}"
BUILD_FROM_SOURCE="${DOCKLET_BUILD_FROM_SOURCE:-0}"
READINESS_TIMEOUT="${READINESS_TIMEOUT:-60}"
# Enrollment retries: a control-plane restart during install is transient.
ENROLL_ATTEMPTS="${ENROLL_ATTEMPTS:-10}"
ENROLL_RETRY_DELAY="${ENROLL_RETRY_DELAY:-5}"
# Base image (microvm): where it lands, and whether to rebuild an existing one.
IMAGE_DIR="${DOCKLET_IMAGE_DIR:-/var/lib/docklet/images}"
REBUILD_IMAGE="${DOCKLET_REBUILD_IMAGE:-0}"
# Pre-built golden image (path or URL, optionally .zst) plus the sha256 of the
# raw image: installs the fleet's one image instead of building a local one.
BASE_IMAGE="${DOCKLET_BASE_IMAGE:-}"
BASE_IMAGE_SHA256="${DOCKLET_BASE_IMAGE_SHA256:-}"
# MicroVM jails (microvm): deliberately under the state dir. The driver hard-links
# the guest kernel from IMAGE_DIR into each jail root, so both must share one
# filesystem, and reflink provisioning needs the same.
JAIL_BASE="${DOCKLET_JAIL_BASE:-/var/lib/docklet/jail}"

ENROLLED_HOST_ID=""

TMP_FILES=()
cleanup() {
  if [ "${#TMP_FILES[@]}" -gt 0 ]; then
    rm -rf "${TMP_FILES[@]}"
  fi
}
trap cleanup EXIT

# shellcheck source=lib-verify.sh
. "${SCRIPT_DIR}/lib-verify.sh"

log() { printf '[%s] bootstrap-host: %s\n' "$(date -u +%FT%TZ)" "$*"; }
warn() { printf '[%s] bootstrap-host: WARNING: %s\n' "$(date -u +%FT%TZ)" "$*" >&2; }
die() {
  printf '[%s] bootstrap-host: ERROR: %s\n' "$(date -u +%FT%TZ)" "$*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Usage: bootstrap-host.sh --fleet-url <url> --fleet-token <token>
                         [--host-id <id>] [--runtime microvm|gvisor]
                         [--pool-size N] [--egress-allow-private CIDRS]
                         [--allow-vulnerable NAMES]
                         [--base-image PATH|URL --base-image-sha256 HEX]
                         [--build-from-source]

  The docklet binary is downloaded from the GitHub release DOCKLET_VERSION, and the
  install stops unless its sha256 matches checksums.txt AND the cosign signature of
  both files verifies (cosign is installed from a pinned, checksum-checked release if missing).
  --build-from-source  DEVELOPMENT ONLY: build from this checkout instead; nothing is verified

  --fleet-url    control plane base URL, e.g. https://api.example.com
  --fleet-token  shared token used to enroll and register (used once; not stored on the host);
                 prefer DOCKLET_FLEET_TOKEN in the environment, argv is visible to local users
  --host-id      stable host identity; defaults to the enrolled id on disk,
                 otherwise the control plane derives one
  --runtime      microvm | gvisor; defaults to microvm when /dev/kvm exists
                 (microvm also gets the base image + a jail base under /var/lib/docklet)
  --pool-size    warm pool target written to DOCKLET_POOL_SIZE
  --egress-allow-private
                 comma-separated private CIDRs confined runs may reach through
                 the egress proxy, e.g. a control plane on a LAN (10.0.0.0/24)
  --allow-vulnerable
                 comma-separated CPU vulnerabilities to accept, e.g.
                 spec_rstack_overflow; single-tenant or dev hosts only
  --base-image   microvm: install this pre-built base image (local path or http(s)
                 URL; a .zst suffix is decompressed) instead of building one, so
                 every host holds the same base and delta generations restore
                 anywhere; needs --base-image-sha256
  --base-image-sha256
                 sha256 of the raw (uncompressed) image, i.e. its base fingerprint

Env equivalents: DOCKLET_FLEET_URL, DOCKLET_FLEET_TOKEN, DOCKLET_HOST_ID,
DOCKLET_RUNTIME, DOCKLET_POOL_SIZE, DOCKLET_EGRESS_ALLOW_PRIVATE,
DOCKLET_ALLOW_VULNERABLE, DOCKLET_BASE_IMAGE, DOCKLET_BASE_IMAGE_SHA256.
Installer knobs: DOCKLET_VERSION (release to install), DOCKLET_BIN (prebuilt binary, NOT verified), DOCKLET_GVISOR_IMAGE,
DOCKLET_GO_IMAGE (toolchain container used when go is missing),
DOCKLET_IMAGE_DIR, DOCKLET_JAIL_BASE, FC_VERSION, RUNSC_VERSION (overriding either requires the
matching FC_SHA256_X86_64/FC_SHA256_AARCH64/RUNSC_SHA256_X86_64),
DOCKLET_REBUILD_IMAGE=1 (rebuild an existing base image), READINESS_TIMEOUT.

A microvm host builds the base image (base.ext4 + vmlinux + initrd.img) it boots
through build-image.sh: several minutes and ~20GB, skipped once present. Bases
are content-addressed under images/bases/<sha256>/ and never deleted; a rebuild
or --base-image adds one and repoints images/base.ext4.
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --fleet-url)
      [ "$#" -ge 2 ] || die "--fleet-url needs a value"
      FLEET_URL="$2"
      shift 2
      ;;
    --fleet-url=*)
      FLEET_URL="${1#*=}"
      shift
      ;;
    --fleet-token)
      [ "$#" -ge 2 ] || die "--fleet-token needs a value"
      FLEET_TOKEN="$2"
      shift 2
      ;;
    --fleet-token=*)
      FLEET_TOKEN="${1#*=}"
      shift
      ;;
    --host-id)
      [ "$#" -ge 2 ] || die "--host-id needs a value"
      HOST_ID="$2"
      shift 2
      ;;
    --host-id=*)
      HOST_ID="${1#*=}"
      shift
      ;;
    --runtime)
      [ "$#" -ge 2 ] || die "--runtime needs a value"
      RUNTIME="$2"
      shift 2
      ;;
    --runtime=*)
      RUNTIME="${1#*=}"
      shift
      ;;
    --pool-size)
      [ "$#" -ge 2 ] || die "--pool-size needs a value"
      POOL_SIZE="$2"
      shift 2
      ;;
    --pool-size=*)
      POOL_SIZE="${1#*=}"
      shift
      ;;
    --egress-allow-private)
      [ "$#" -ge 2 ] || die "--egress-allow-private needs a value"
      EGRESS_ALLOW_PRIVATE="$2"
      shift 2
      ;;
    --egress-allow-private=*)
      EGRESS_ALLOW_PRIVATE="${1#*=}"
      shift
      ;;
    --allow-vulnerable)
      [ "$#" -ge 2 ] || die "--allow-vulnerable needs a value"
      ALLOW_VULNERABLE="$2"
      shift 2
      ;;
    --allow-vulnerable=*)
      ALLOW_VULNERABLE="${1#*=}"
      shift
      ;;
    --base-image)
      [ "$#" -ge 2 ] || die "--base-image needs a value"
      BASE_IMAGE="$2"
      shift 2
      ;;
    --base-image=*)
      BASE_IMAGE="${1#*=}"
      shift
      ;;
    --base-image-sha256)
      [ "$#" -ge 2 ] || die "--base-image-sha256 needs a value"
      BASE_IMAGE_SHA256="$2"
      shift 2
      ;;
    --base-image-sha256=*)
      BASE_IMAGE_SHA256="${1#*=}"
      shift
      ;;
    --build-from-source)
      BUILD_FROM_SOURCE=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      die "unknown argument: $1"
      ;;
  esac
done

need_root() {
  [ "$(id -u)" -eq 0 ] || die "must run as root (sudo)"
}
need_root

[ -n "$FLEET_URL" ] || die "missing --fleet-url (or DOCKLET_FLEET_URL); see --help"
[ -n "$FLEET_TOKEN" ] || die "missing --fleet-token (or DOCKLET_FLEET_TOKEN); see --help"
FLEET_URL="${FLEET_URL%/}"

# Reads a value previously written by this script (quoted EnvironmentFile form).
env_value() {
  sed -n "s/^$1=//p" "$ENV_FILE" | head -n 1 | tr -d '"'
}

# Re-enrolls carry the identity and pool target already on disk: the control
# plane is idempotent per hostId, so the store config and host id stay stable.
if [ -f "$ENV_FILE" ]; then
  if [ -z "$HOST_ID" ]; then
    HOST_ID="$(env_value DOCKLET_HOST_ID)"
    if [ -n "$HOST_ID" ]; then
      log "reusing enrolled host id ${HOST_ID} from ${ENV_FILE}"
    fi
  fi
  if [ -z "$POOL_SIZE" ]; then
    POOL_SIZE="$(env_value DOCKLET_POOL_SIZE)"
  fi
  if [ -z "$ALLOW_VULNERABLE" ]; then
    ALLOW_VULNERABLE="$(env_value DOCKLET_ALLOW_VULNERABLE)"
  fi
fi

if [ -z "$RUNTIME" ]; then
  if [ -e /dev/kvm ]; then
    RUNTIME="microvm"
  else
    RUNTIME="gvisor"
  fi
  log "no --runtime given; detected ${RUNTIME}"
fi
case "$RUNTIME" in
  microvm|gvisor) ;;
  *) die "--runtime must be microvm or gvisor, got '${RUNTIME}'" ;;
esac
case "$POOL_SIZE" in
  '') ;;
  *[!0-9]*) die "--pool-size must be a non-negative integer, got '${POOL_SIZE}'" ;;
esac
if [ -n "$BASE_IMAGE" ]; then
  [ "$RUNTIME" = "microvm" ] || die "--base-image only applies to microvm hosts"
  printf '%s' "$BASE_IMAGE_SHA256" | grep -Eq '^[0-9a-f]{64}$' || die "--base-image needs --base-image-sha256 <64 lowercase hex>"
fi

if [ -d /usr/local/go/bin ]; then
  PATH="${PATH}:/usr/local/go/bin"
  export PATH
fi

# Build steps run as the user who invoked sudo: their toolchains (~/.bun,
# ~/go) are not on root's PATH, and the checkout must stay owned by them.
INVOKER="${SUDO_USER:-}"
[ "$INVOKER" = "root" ] && INVOKER=""
INVOKER_HOME=""
if [ -n "$INVOKER" ]; then
  INVOKER_HOME="$(getent passwd "$INVOKER" | cut -d: -f6)"
fi
INVOKER_PATH="${INVOKER_HOME:+${INVOKER_HOME}/.bun/bin:${INVOKER_HOME}/go/bin:}${PATH}"

as_invoker() {
  if [ -n "$INVOKER" ]; then
    sudo -u "$INVOKER" env HOME="$INVOKER_HOME" PATH="$INVOKER_PATH" "$@"
  else
    env PATH="$INVOKER_PATH" "$@"
  fi
}

docker_usable() {
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1
}

export DEBIAN_FRONTEND=noninteractive

pkg_installed() {
  [ "$(dpkg-query -W -f '${Status}' "$1" 2>/dev/null || true)" = "install ok installed" ]
}

ensure_packages() {
  local missing=() pkg
  for pkg in "$@"; do
    if ! pkg_installed "$pkg"; then
      missing+=("$pkg")
    fi
  done
  if [ "${#missing[@]}" -eq 0 ]; then
    log "packages already installed"
    return 0
  fi
  log "installing packages: ${missing[*]}"
  apt-get update
  apt-get install -y --no-install-recommends "${missing[@]}"
}

ensure_docker() {
  if ! command -v docker >/dev/null 2>&1; then
    log "docker missing; installing docker.io"
    ensure_packages docker.io
  fi
  systemctl enable --now docker >/dev/null 2>&1 || warn "docker not started by systemd"
  docker info >/dev/null 2>&1 || die "docker daemon unreachable; the gvisor runtime needs it"
  log "docker: $(docker --version)"
}

# Fails closed: an archive must match its pinned sha256 before it is unpacked.
verify_sha256() {
  local file="$1" want="$2" got
  got="$(sha256sum "$file" | cut -d' ' -f1)"
  [ "$got" = "$want" ] || die "checksum mismatch for $(basename "$file"): want ${want}, got ${got}"
}

install_runsc() {
  if [ -x "$RUNSC_PATH" ]; then
    log "runsc already installed at ${RUNSC_PATH}"
    return 0
  fi
  local arch tmp
  arch="$(uname -m)"
  [ "$arch" = "x86_64" ] || die "gVisor publishes x86_64 releases only, this host is ${arch}"
  tmp="$(mktemp -d)"
  TMP_FILES+=("$tmp")
  log "installing gVisor runsc ${RUNSC_VERSION}"
  curl -fsSL -o "${tmp}/gvisor.tar.bz2" \
    "https://github.com/google/gvisor/releases/download/${RUNSC_VERSION}/gvisor-${arch}.tar.bz2"
  verify_sha256 "${tmp}/gvisor.tar.bz2" "$RUNSC_SHA256_X86_64"
  tar -xjf "${tmp}/gvisor.tar.bz2" -C "$tmp" runsc
  install -D -m 0755 "${tmp}/runsc" "$RUNSC_PATH"
}

# daemon.json is merged, never replaced: the host may already carry mirrors,
# insecure registries, or other runtimes.
register_runsc() {
  python3 - "$RUNSC_PATH" <<'PY'
import json, os, shutil, sys
path = "/etc/docker/daemon.json"
os.makedirs(os.path.dirname(path), exist_ok=True)
cfg = {}
if os.path.exists(path):
    try:
        with open(path) as fh:
            cfg = json.load(fh)
    except json.JSONDecodeError:
        shutil.copy2(path, path + ".docklet-bak")
        print(f"bootstrap-host: {path} is not valid JSON; kept a copy at {path}.docklet-bak")
        cfg = {}
cfg.setdefault("runtimes", {})["runsc"] = {"path": sys.argv[1]}
with open(path, "w") as fh:
    json.dump(cfg, fh, indent=2)
    fh.write("\n")
PY
}

reload_docker() {
  # Reload only. A restart would kill unrelated containers on this host.
  if ! systemctl reload docker; then
    die "systemctl reload docker failed; restart docker manually to pick up runsc, then re-run"
  fi
}

verify_runsc() {
  docker info >/dev/null 2>&1 || die "docker daemon unreachable after reload"
  if ! docker info --format '{{.Runtimes}}' | grep -qw runsc; then
    die "docker does not list the runsc runtime; the reload did not apply daemon.json"
  fi
  log "docker runtime runsc registered"
}

install_firecracker() {
  local arch tmp url want
  arch="$(uname -m)"
  case "$arch" in
    x86_64) want="$FC_SHA256_X86_64" ;;
    aarch64) want="$FC_SHA256_AARCH64" ;;
    *) die "unsupported arch ${arch}" ;;
  esac
  url="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
  tmp="$(mktemp -d)"
  TMP_FILES+=("$tmp")
  log "installing firecracker + jailer ${FC_VERSION}"
  curl -fsSL -o "${tmp}/firecracker.tgz" "${url}/firecracker-${FC_VERSION}-${arch}.tgz"
  verify_sha256 "${tmp}/firecracker.tgz" "$want"
  tar -xzf "${tmp}/firecracker.tgz" -C "$tmp"
  install -D -m 0755 "${tmp}/release-${FC_VERSION}-${arch}/firecracker-${FC_VERSION}-${arch}" /usr/bin/firecracker
  install -D -m 0755 "${tmp}/release-${FC_VERSION}-${arch}/jailer-${FC_VERSION}-${arch}" /usr/bin/jailer
}

provision_microvm() {
  [ -e /dev/kvm ] || die "/dev/kvm missing; use --runtime gvisor on hosts without virtualization"
  if [ -f "${SCRIPT_DIR}/provision-host.sh" ]; then
    # provision-host.sh owns KVM device setup, cgroupsv2, firecracker and the
    # state layout; calling it keeps one implementation of that.
    log "provisioning microvm host via provision-host.sh"
    DOCKLET_RUNTIME=microvm bash "${SCRIPT_DIR}/provision-host.sh"
    return 0
  fi
  if [ ! -x /usr/bin/firecracker ] || [ ! -x /usr/bin/jailer ]; then
    install_firecracker
  fi
  modprobe kvm 2>/dev/null || true
}

ensure_state_dirs() {
  mkdir -p /var/lib/docklet/images /var/lib/docklet/vms /var/lib/docklet/gens \
    /var/lib/docklet/state/sockets /var/lib/docklet/state/logs /var/lib/docklet/state/idem
  if [ "$RUNTIME" = "microvm" ]; then
    # Under the state filesystem so kernel links and reflinks stay same-device.
    mkdir -p "$JAIL_BASE"
    # Docklet and jailer run as root; each dock drops to its own uid at boot.
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
    # Each dock gets its own jailer uid at boot (DOCKLET_UID_BASE range, no passwd entries needed); the base stays root.
    chown root:root "$JAIL_BASE"
    chmod 0755 "$JAIL_BASE"
  fi
}

# Where the docklet keeps its per-host fleet credential (0600).
CREDENTIAL_FILE="/var/lib/docklet/fleet-credential"
ENROLLED_CREDENTIAL=""

enroll() {
  local body code out hdr attempt
  # A re-run on an enrolled host keeps its identity and credential: the shared
  # token can no longer enroll an existing host, so there is nothing to redo.
  if [ -n "$HOST_ID" ] && [ "$HOST_ID" = "$(env_value DOCKLET_HOST_ID 2>/dev/null)" ] && [ -s "$CREDENTIAL_FILE" ]; then
    ENROLLED_HOST_ID="$HOST_ID"
    log "already enrolled as ${ENROLLED_HOST_ID}; keeping its credential"
    return
  fi
  out="$(mktemp)"
  hdr="$(mktemp)"
  TMP_FILES+=("$out" "$hdr")
  chmod 0600 "$hdr"
  # The token goes in a 0600 header file so it never appears in curl's argv.
  printf 'authorization: Bearer %s\n' "$FLEET_TOKEN" >"$hdr"
  body="$(jq -nc --arg runtime "$RUNTIME" --arg hostId "$HOST_ID" \
    '{runtime:$runtime} + (if $hostId == "" then {} else {hostId:$hostId} end)')"
  log "enrolling with ${FLEET_URL}/v1/fleet/enroll"
  # A control-plane restart must not fail a host install, so a transient
  # 5xx/connection error is retried before giving up. A 401 is not: a wrong
  # token will never fix itself, and retrying it just delays the real message.
  for attempt in $(seq 1 "$ENROLL_ATTEMPTS"); do
    code="$(curl -sS --connect-timeout 10 --max-time 30 -o "$out" -w '%{http_code}' \
      -X POST "${FLEET_URL}/v1/fleet/enroll" \
      -H "@${hdr}" \
      -H 'content-type: application/json' \
      --data "$body" 2>/dev/null || echo 000)"
    [ "$code" = "200" ] && break
    case "$code" in
      401|403|409) break ;;
    esac
    if [ "$attempt" -lt "$ENROLL_ATTEMPTS" ]; then
      warn "enroll attempt ${attempt}/${ENROLL_ATTEMPTS} got HTTP ${code}; retrying"
      sleep "$ENROLL_RETRY_DELAY"
    fi
  done
  if [ "$code" = "000" ]; then
    die "cannot reach ${FLEET_URL} to enroll"
  fi
  if [ "$code" = "409" ]; then
    die "host is already enrolled and its credential is not on this machine; an instance admin must reset it (POST ${FLEET_URL}/v1/hosts/<id>/reset-credential), then write the new credential to ${CREDENTIAL_FILE} (mode 0600) and re-run"
  fi
  if [ "$code" != "200" ]; then
    printf 'bootstrap-host: enroll response: %s\n' "$(cat "$out")" >&2
    die "enroll failed with HTTP ${code}; check --fleet-token and --fleet-url"
  fi
  if ! jq -e '.ok == true and (.data.hostId | type == "string")' "$out" >/dev/null; then
    printf 'bootstrap-host: enroll response: %s\n' "$(cat "$out")" >&2
    die "enroll returned an unexpected response shape"
  fi
  ENROLLED_HOST_ID="$(jq -r '.data.hostId' "$out")"
  ENROLLED_CREDENTIAL="$(jq -r '.data.credential // empty' "$out")"
  if [ -n "$ENROLLED_CREDENTIAL" ]; then
    mkdir -p "$(dirname "$CREDENTIAL_FILE")"
    ( umask 077; printf '%s\n' "$ENROLLED_CREDENTIAL" > "${CREDENTIAL_FILE}.tmp" )
    mv -f "${CREDENTIAL_FILE}.tmp" "$CREDENTIAL_FILE"
  else
    warn "control plane issued no host credential (older control plane); keeping the fleet token in ${ENV_FILE}. Upgrade the control plane, then re-run."
  fi
  # No object-storage coordinates: generations move through the control plane,
  # so a docklet host never holds bucket credentials.
  log "enrolled as ${ENROLLED_HOST_ID}"
}

# Values land in an EnvironmentFile that systemd parses: no whitespace,
# quotes, backslashes, backticks, or newlines survive safely there.
env_line() {
  local key="$1" value="$2"
  case "$value" in
    *[[:space:]]*|*'"'*|*'`'*|*\\*)
      die "value for ${key} contains characters unsafe in an EnvironmentFile"
      ;;
  esac
  printf '%s="%s"\n' "$key" "$value"
}

write_env_file() {
  local previous tmp
  install -d -m 0750 "$ENV_DIR"
  if [ -f "$ENV_FILE" ]; then
    previous="$(sed -n 's/^DOCKLET_HOST_ID=//p' "$ENV_FILE" | head -n 1 | tr -d '"')"
    if [ -n "$previous" ] && [ "$previous" != "$ENROLLED_HOST_ID" ]; then
      warn "host identity changed: ${previous} -> ${ENROLLED_HOST_ID}"
    fi
  fi
  tmp="$(mktemp "${ENV_DIR}/docklet.env.XXXXXX")"
  TMP_FILES+=("$tmp")
  {
    printf '# Written by scripts/bootstrap-host.sh; re-run the installer to change it.\n'
    printf '# Snapshot storage and Restate stay on the control plane: no MinIO or Restate on hosts.\n'
    printf '# The docklet authenticates with its own credential in %s (0600); the shared\n' "$CREDENTIAL_FILE"
    printf '# fleet token is not stored on this host.\n'
    env_line DOCKLET_FLEET_URL "$FLEET_URL"
    if [ -z "$ENROLLED_CREDENTIAL" ] && [ ! -s "$CREDENTIAL_FILE" ]; then
      env_line DOCKLET_FLEET_TOKEN "$FLEET_TOKEN"
    fi
    env_line DOCKLET_HOST_ID "$ENROLLED_HOST_ID"
    env_line DOCKLET_RUNTIME "$RUNTIME"
    env_line DOCKLET_ADDR "$DOCKLET_ADDR_VALUE"
    if [ "$RUNTIME" = "gvisor" ]; then
      env_line DOCKLET_GVISOR_IMAGE "$GVISOR_IMAGE"
    else
      env_line DOCKLET_JAIL_BASE "$JAIL_BASE"
    fi
    if [ -n "$POOL_SIZE" ]; then
      env_line DOCKLET_POOL_SIZE "$POOL_SIZE"
    fi
    if [ -n "$EGRESS_ALLOW_PRIVATE" ]; then
      env_line DOCKLET_EGRESS_ALLOW_PRIVATE "$EGRESS_ALLOW_PRIVATE"
    fi
    if [ -n "$ALLOW_VULNERABLE" ]; then
      env_line DOCKLET_ALLOW_VULNERABLE "$ALLOW_VULNERABLE"
    fi
  } > "$tmp"
  chmod 0600 "$tmp"
  mv -f "$tmp" "$ENV_FILE"
  log "wrote ${ENV_FILE} (0600)"
}

install_docklet_binary() {
  local out asset arch
  if [ -n "$PREBUILT_BIN" ]; then
    [ -f "$PREBUILT_BIN" ] || die "DOCKLET_BIN=${PREBUILT_BIN} not found"
    warn "installing DOCKLET_BIN without signature verification"
    install -D -m 0755 "$PREBUILT_BIN" "$DOCKLET_DEST"
    log "installed ${DOCKLET_DEST} from ${PREBUILT_BIN}"
    return 0
  fi
  if [ "$BUILD_FROM_SOURCE" = "1" ]; then
    warn "building from source: the binary is NOT signature-verified (development only)"
    [ -f "${DOCKLET_DIR}/go.mod" ] && [ -f "${DOCKLET_DIR}/cmd/docklet/main.go" ] \
      || die "--build-from-source needs a checkout of this repo"
    out="$(mktemp -d)"
    TMP_FILES+=("$out")
    if as_invoker sh -c 'command -v go' >/dev/null 2>&1; then
      log "building docklet from ${DOCKLET_DIR}"
      if [ -n "$INVOKER" ]; then
        chown "$INVOKER" "$out"
      fi
      ( cd "$DOCKLET_DIR" && as_invoker go build -o "${out}/docklet" ./cmd/docklet )
    elif docker_usable; then
      log "no go toolchain; building docklet in a ${GO_IMAGE} container"
      docker run --rm -v "${DOCKLET_DIR}:/src:ro" -v "${out}:/out" -w /src \
        -e GOFLAGS=-buildvcs=false "$GO_IMAGE" go build -o /out/docklet ./cmd/docklet
    else
      die "building docklet needs Go or Docker; install one, or drop --build-from-source"
    fi
    install -D -m 0755 "${out}/docklet" "$DOCKLET_DEST"
    log "installed ${DOCKLET_DEST} (built from checkout)"
    return 0
  fi
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  asset="docklet-linux-${arch}"
  out="$(mktemp -d)"
  TMP_FILES+=("$out")
  log "downloading and verifying docklet ${DOCKLET_VERSION} (${asset})"
  fetch_release "$DOCKLET_VERSION" "$out" "$asset"
  install -D -m 0755 "${out}/${asset}" "$DOCKLET_DEST"
  log "installed ${DOCKLET_DEST} (${DOCKLET_VERSION}, signature verified)"
}

install_unit() {
  log "writing ${UNIT_FILE}"
  cat > "$UNIT_FILE" <<'UNIT'
[Unit]
Description=docklet host agent
Wants=network-online.target
After=network-online.target docker.service

[Service]
Type=simple
EnvironmentFile=/etc/docklet/docklet.env
ExecStart=/usr/local/bin/docklet
Restart=always
RestartSec=5
LimitNOFILE=1048576
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable docklet
  if systemctl is-active --quiet docklet; then
    # A re-run must pick up a rebuilt binary or a changed env file.
    systemctl restart docklet || die "docklet.service failed to restart; check: journalctl -u docklet -n 50"
  else
    systemctl start docklet || die "docklet.service failed to start; check: journalctl -u docklet -n 50"
  fi
}

build_gvisor_image() {
  if [ ! -f "${SCRIPT_DIR}/Dockerfile.gvisor" ] || [ ! -f "${DOCKLET_DIR}/go.mod" ]; then
    log "no checkout at ${DOCKLET_DIR}; skipping the gvisor image build"
    log "expecting ${GVISOR_IMAGE} to come from a registry or a prebuilt tarball"
    return 0
  fi
  local inputs
  inputs="$(gvisor_image_inputs)"
  if [ "$(built_image_inputs)" = "$inputs" ]; then
    log "image ${GVISOR_IMAGE} is current; skipping the build (docker rmi to force)"
  else
    if gvisor_image_present; then
      log "image ${GVISOR_IMAGE} was built from different inputs (image inputs changed); rebuilding"
    fi
    log "building the gvisor OCI image ${GVISOR_IMAGE}"
    ( cd "$DOCKLET_DIR" && docker build -f scripts/Dockerfile.gvisor --label "${GVISOR_INPUTS_LABEL}=${inputs}" -t "$GVISOR_IMAGE" . )
  fi
}

gvisor_image_present() {
  docker image inspect "$GVISOR_IMAGE" >/dev/null 2>&1
}

# Digest of what this checkout bakes into the gvisor image, so a guest agent,
# entrypoint, Dockerfile or pin change rebuilds it.
gvisor_image_inputs() {
  local program
  program="$(sed -n '/^const fastAgentProgram = `/,/`$/p' "${DOCKLET_DIR}/internal/host/fast.go")"
  [ -n "$program" ] || die "guest agent source not found in internal/host/fast.go"
  {
    cat "${SCRIPT_DIR}/Dockerfile.gvisor" "${SCRIPT_DIR}/image-versions.env" "${SCRIPT_DIR}/dock-container-entrypoint.sh"
    printf '%s' "$program"
  } | sha256sum | cut -c1-16
}

built_image_inputs() {
  docker image inspect -f "{{ index .Config.Labels \"${GVISOR_INPUTS_LABEL}\" }}" "$GVISOR_IMAGE" 2>/dev/null || true
}

# The golden image is what a microVM boots: without it every provision fails.
# build-image.sh derives the guest kernel from the running host kernel, so the
# kernel release it was built for is stamped next to it and a mismatch is
# reported rather than silently rebuilt (that build is minutes and ~20GB).
build_microvm_image() {
  local stamp="${IMAGE_DIR}/.kernel-release" kernel
  kernel="$(uname -r)"
  if [ -n "$BASE_IMAGE" ]; then
    install_prebuilt_image
    return 0
  fi
  if [ -f "${IMAGE_DIR}/base.ext4" ] && [ -f "${IMAGE_DIR}/vmlinux" ] && [ "$REBUILD_IMAGE" != "1" ]; then
    if [ -f "$stamp" ] && [ "$(cat "$stamp")" != "$kernel" ]; then
      warn "golden image was built for kernel $(cat "$stamp") but this host runs ${kernel}; re-run with DOCKLET_REBUILD_IMAGE=1 to rebuild"
    fi
    log "golden image already present in ${IMAGE_DIR}; skipping the build (DOCKLET_REBUILD_IMAGE=1 to force)"
    return 0
  fi
  [ -f "${SCRIPT_DIR}/build-image.sh" ] || die "build-image.sh missing; cannot build the golden image this host boots"
  [ -f "${SCRIPT_DIR}/extract-vmlinux.sh" ] || die "extract-vmlinux.sh missing; build-image.sh needs it"
  if ! command -v debootstrap >/dev/null 2>&1; then
    ensure_packages debootstrap e2fsprogs
  fi
  mkdir -p "$IMAGE_DIR"
  log "building the golden image for kernel ${kernel} (several minutes, ~20GB)"
  OUT_DIR="$IMAGE_DIR" bash "${SCRIPT_DIR}/build-image.sh"
  printf '%s' "$kernel" > "$stamp"
  log "golden image ready: ${IMAGE_DIR}/base.ext4 + vmlinux + initrd.img"
}

# One image fleet-wide means one fingerprint, which is what lets a delta
# generation captured on one host restore on any other. The guest kernel is
# still staged from this host's kernel; it is not part of the base identity.
install_prebuilt_image() {
  local stamp="${IMAGE_DIR}/.kernel-release" kernel src="$BASE_IMAGE" fp tmp
  kernel="$(uname -r)"
  mkdir -p "${IMAGE_DIR}/bases"
  # Re-hashed before it is trusted: a corrupt copy is moved aside and reinstalled.
  if [ -f "${IMAGE_DIR}/bases/${BASE_IMAGE_SHA256}/base.ext4" ] \
    && OUT_DIR="$IMAGE_DIR" bash "${SCRIPT_DIR}/install-base.sh" "${IMAGE_DIR}/bases/${BASE_IMAGE_SHA256}/base.ext4" "$BASE_IMAGE_SHA256" >/dev/null; then
    log "base ${BASE_IMAGE_SHA256} already installed and verified; made it current"
  else
    case "$src" in
      http://*|https://*)
        tmp="$(mktemp -p "${IMAGE_DIR}/bases" .download.XXXXXX)"
        TMP_FILES+=("$tmp")
        log "downloading the golden image from ${src}"
        curl -fL --retry 3 -o "$tmp" "$src" || die "downloading ${src} failed"
        src="$tmp"
        ;;
    esac
    [ -f "$src" ] || die "base image ${src} not found"
    case "$BASE_IMAGE" in
      *.zst)
        command -v zstd >/dev/null 2>&1 || ensure_packages zstd
        tmp="$(mktemp -p "${IMAGE_DIR}/bases" .raw.XXXXXX)"
        TMP_FILES+=("$tmp")
        zstd -d -q -f --sparse -o "$tmp" "$src" || die "decompressing ${BASE_IMAGE} failed"
        src="$tmp"
        ;;
    esac
    fp="$(OUT_DIR="$IMAGE_DIR" bash "${SCRIPT_DIR}/install-base.sh" "$src" "$BASE_IMAGE_SHA256")" \
      || die "installing the golden image failed (checksum mismatch?)"
    log "installed base ${fp}"
  fi
  if [ ! -f "${IMAGE_DIR}/vmlinux" ] || [ ! -f "$stamp" ] || [ "$(cat "$stamp")" != "$kernel" ]; then
    OUT_DIR="$IMAGE_DIR" bash "${SCRIPT_DIR}/install-kernel.sh"
    printf '%s' "$kernel" > "$stamp"
  fi
  log "golden image ready: ${IMAGE_DIR}/base.ext4 -> bases/${BASE_IMAGE_SHA256} + vmlinux + initrd.img"
}

wait_ready() {
  local _
  log "waiting for ${READINESS_URL}"
  for _ in $(seq 1 "$READINESS_TIMEOUT"); do
    if curl -fsS --max-time 5 "$READINESS_URL" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  die "docklet did not answer ${READINESS_URL} within ${READINESS_TIMEOUT}s; check: journalctl -u docklet -n 50"
}

report_readiness() {
  local body vulns
  body="$(curl -fsS "$READINESS_URL")" || die "readiness request failed"
  log "readiness: ${body}"
  if [ "$(printf '%s' "$body" | jq -r '.data.ready // false')" != "true" ]; then
    printf '%s' "$body" | jq -r '.data.reasons[]?' | while IFS= read -r reason; do
      warn "$reason"
    done
    # Only CPU vulnerabilities left: name the opt-in instead of a dead end.
    vulns="$(printf '%s' "$body" | jq -r '[.data.reasons[] | select(startswith("cpu-vulnerable:")) | ltrimstr("cpu-vulnerable:")] | join(",")')"
    if [ -n "$vulns" ] && [ "$(printf '%s' "$body" | jq -r '[.data.reasons[] | select(startswith("cpu-vulnerable:") | not)] | length')" = "0" ]; then
      warn "the CPU/kernel lacks mitigations for: ${vulns}. Update microcode/kernel, or on a single-tenant or dev host accept them by re-running with --allow-vulnerable ${vulns}"
    fi
    die "docklet is running but reports itself unready (reasons above)"
  fi
}

summary() {
  local state
  state="$(systemctl is-active docklet 2>/dev/null || true)"
  log "bootstrap complete"
  printf 'host id        : %s\n' "$ENROLLED_HOST_ID"
  printf 'runtime        : %s\n' "$RUNTIME"
  printf 'fleet url      : %s\n' "$FLEET_URL"
  printf 'generations    : via %s (no store credentials on this host)\n' "$FLEET_URL"
  if [ "$RUNTIME" = "microvm" ]; then
    printf 'golden image   : %s/base.ext4 + vmlinux\n' "$IMAGE_DIR"
    printf 'jail base      : %s\n' "$JAIL_BASE"
  else
    printf 'gvisor image   : %s\n' "$GVISOR_IMAGE"
  fi
  printf 'env file       : %s (0600)\n' "$ENV_FILE"
  printf 'docklet        : %s (%s)\n' "$DOCKLET_DEST" "${state:-unknown}"
}

# 1. Base packages the rest of the script relies on.
ensure_packages curl ca-certificates iproute2 iptables bzip2 jq python3 e2fsprogs util-linux mount
for tool in mkfs.ext4 mountpoint losetup mount umount; do
  command -v "$tool" >/dev/null 2>&1 || die "${tool} is missing after installing e2fsprogs, util-linux and mount; install them and re-run"
done

# 2. Runtime setup for the chosen runtime.
if [ "$RUNTIME" = "gvisor" ]; then
  ensure_docker
  install_runsc
  register_runsc
  reload_docker
  verify_runsc
else
  provision_microvm
fi
ensure_state_dirs

# 3. Enroll with the control plane, then persist the identity it returns.
enroll
write_env_file

# 4. The docklet binary and its unit.
install_docklet_binary
# Built before the service starts so the host never comes online without the
# image its docks boot from.
if [ "$RUNTIME" = "gvisor" ]; then
  build_gvisor_image
else
  build_microvm_image
fi
install_unit

# 5. Readiness and summary.
wait_ready
report_readiness
summary
