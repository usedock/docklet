#!/usr/bin/env bash
# CI: pulls a fleet gVisor dock into a local dock, against a self-hosted control plane whose only host is
# this machine (runsc, no KVM needed for the host). Creates a dock on the control plane, writes a marker tree
# in /home/user/work (a file, a 0700 directory, a symlink), then runs dock pull --runtime gvisor twice with
# the CLI under test: once while the remote dock is running (the host pauses it for the export) and once
# more after a stop and resume (the control plane exports only ready docks). Each local copy is resumed and its content, modes, symlink and owner are checked with
# dock exec. A last leg pulls into a QEMU dock; it is skipped while the CLI still refuses with the issue #128
# text. Needs DOCK (binary), DOCK_URL, LOGS (dir); jq and curl. Docker with the runsc and runsc-suid runtimes
# must be set up, as the workflow does.
# Under Actions, fd 3 is the step's own stdout: the key mask goes there, never into the tee'd log.
set -euo pipefail
: "${DOCK:?path of the dock binary}" "${DOCK_URL:?control plane URL}" "${LOGS:?log dir}"
# shellcheck source=scripts/lib-e2e-selfhost.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-e2e-selfhost.sh"

setup_selfhost_context
# The local engine keeps its disks and images here, apart from the host's docklet state.
export DOCK_DATA_DIR="${DOCK_DATA_DIR:-${RUNNER_TEMP:-/tmp}/dock-local}"
mkdir -p "$DOCK_DATA_DIR"

SRC=gv-src
t=$(date +%s)
new_dock "$SRC" --cpus 1 --memory 1gb
timing "dock new (fleet gvisor)" "$t"
[ "$($DOCK ls --json | jq -r --arg n "$SRC" '[.. | objects | select(.name? == $n) | .runtime][0]')" = gvisor ] \
  || { $DOCK ls --json >&2; die "the fleet dock does not run on gVisor; is the host bootstrapped with --runtime gvisor?"; }

# The marker tree: content, a private directory, a restrictive file mode and a relative symlink.
$DOCK exec "$SRC" -- 'mkdir -p /home/user/work/tree/private \
  && chmod 0700 /home/user/work/tree/private \
  && printf marker-content > /home/user/work/tree/file.txt \
  && chmod 0600 /home/user/work/tree/file.txt \
  && printf inner-content > /home/user/work/tree/private/inner.txt \
  && ln -s file.txt /home/user/work/tree/link && sync'
[ "$($DOCK exec "$SRC" -- cat /home/user/work/tree/file.txt)" = marker-content ] || die "marker not written on the fleet dock"

# assert_tree <local dock> [late]: checks the pulled copy (after dock resume) with dock exec.
assert_tree() {
  local name="$1" want_late="${2:-}" out uid
  out="$($DOCK exec "$name" -- cat /home/user/work/tree/file.txt)"
  [ "$out" = marker-content ] || die "${name}: file.txt is '${out}'"
  out="$($DOCK exec "$name" -- cat /home/user/work/tree/private/inner.txt)"
  [ "$out" = inner-content ] || die "${name}: private/inner.txt is '${out}'"
  out="$($DOCK exec "$name" -- readlink /home/user/work/tree/link)"
  [ "$out" = file.txt ] || die "${name}: link points to '${out}'"
  out="$($DOCK exec "$name" -- cat /home/user/work/tree/link)"
  [ "$out" = marker-content ] || die "${name}: reading through the link gave '${out}'"
  out="$($DOCK exec "$name" -- stat -c '%a' /home/user/work/tree/private)"
  [ "$out" = 700 ] || die "${name}: private mode is ${out}, want 700"
  out="$($DOCK exec "$name" -- stat -c '%a' /home/user/work/tree/file.txt)"
  [ "$out" = 600 ] || die "${name}: file.txt mode is ${out}, want 600"
  if [ -n "$want_late" ]; then
    out="$($DOCK exec "$name" -- cat /home/user/work/tree/late.txt)"
    [ "$out" = "$want_late" ] || die "${name}: late.txt is '${out}'"
  fi
  uid="$($DOCK exec "$name" -- id -u)"
  out="$($DOCK exec "$name" -- stat -c '%u:%g' /home/user/work/tree /home/user/work/tree/file.txt /home/user/work/tree/private /home/user/work/tree/private/inner.txt | sort -u)"
  [ "$out" = "${uid}:${uid}" ] || die "${name}: owners are '${out}', want the dock user ${uid}:${uid}"
  log "${name}: marker tree intact, owned by uid ${uid}"
}

# pull_gvisor <new local name>: dock pull of the fleet dock into a gVisor dock, resumed afterwards.
pull_gvisor() {
  local name="$1" t
  $DOCK context use selfhost
  t=$(date +%s)
  $DOCK pull "$SRC" --from selfhost --runtime gvisor --name "$name" 2>&1 | tee "$LOGS/pull-${name}.log"
  timing "dock pull -> ${name}" "$t"
  $DOCK context use local
  t=$(date +%s); $DOCK resume "$name"; timing "local: dock resume (${name})" "$t"
}

# 1. Pull while the remote dock runs: the host pauses it for the export, so it must answer afterwards.
pull_gvisor gv-running
assert_tree gv-running
$DOCK context use selfhost
retry 6 $DOCK exec "$SRC" -- true || die "the fleet dock did not recover after the export of a running dock"
[ "$($DOCK exec "$SRC" -- cat /home/user/work/tree/file.txt)" = marker-content ] || die "the fleet dock lost data across the export"
log "running-dock pull ok"

# 2. Stop and resume the fleet dock (the host saves the work directory on stop and restores it), then pull again
# with a file written after the first pull. The control plane exports only ready docks, so a stopped one is not pulled.
$DOCK exec "$SRC" -- 'printf late-content > /home/user/work/tree/late.txt && sync'
t=$(date +%s); $DOCK stop "$SRC"; timing "dock stop (fleet gvisor)" "$t"
t=$(date +%s); $DOCK resume "$SRC"; timing "dock resume (fleet gvisor)" "$t"
retry 6 $DOCK exec "$SRC" -- true || die "the fleet dock did not come back after resume"
pull_gvisor gv-second
assert_tree gv-second late-content
log "second pull after stop and resume ok"

# 3. A QEMU dock from the same remote dock. The CLI refuses with the issue #128 text until that lands; skip then.
$DOCK context use selfhost
set +e
qemu_out="$($DOCK pull "$SRC" --from selfhost --runtime qemu --name gv-qemu 2>&1)"
qemu_rc=$?
set -e
echo "$qemu_out" | tee "$LOGS/pull-gv-qemu.log"
if [ "$qemu_rc" -ne 0 ] && printf '%s' "$qemu_out" | grep -q 'issue #128'; then
  echo "::notice::pull into a qemu dock skipped: the CLI does not support it yet (issue #128); this leg activates once it lands"
  echo "pull -> qemu: skipped (issue #128)" >> "$TIMINGS"
elif [ "$qemu_rc" -ne 0 ]; then
  die "dock pull --runtime qemu failed (exit ${qemu_rc}) with something other than the #128 refusal"
else
  $DOCK context use local
  t=$(date +%s); $DOCK resume gv-qemu; timing "local: dock resume (qemu)" "$t"
  [ "$($DOCK exec gv-qemu -- cat /home/user/work/tree/file.txt)" = marker-content ] || die "gv-qemu: marker missing"
  [ "$($DOCK exec gv-qemu -- readlink /home/user/work/tree/link)" = file.txt ] || die "gv-qemu: symlink missing"
  [ "$($DOCK exec gv-qemu -- stat -c %a /home/user/work/tree/private)" = 700 ] || die "gv-qemu: private mode lost"
  [ "$($DOCK exec gv-qemu -- cat /home/user/work/tree/late.txt)" = late-content ] || die "gv-qemu: late.txt missing"
  log "pull into a qemu dock ok"
  echo "pull -> qemu: ok" >> "$TIMINGS"
  $DOCK rm gv-qemu
fi

$DOCK context use local
$DOCK rm gv-running
$DOCK rm gv-second
$DOCK context use selfhost
$DOCK rm "$SRC"
log "gvisor pull round trip ok"
