#!/usr/bin/env bash
# CI: drives the dock CLI against a self-hosted control plane whose only host is this machine
# (microvm, KVM). Creates the account, org, workspace and API key through the API, then checks that
# the root filesystem grows to the requested disk (docklet#10) across create, stop/resume and branch,
# and at the extra sizes in EXTRA_DISK_GIB, and that a Dockyard image can be pulled and booted. Needs
# DOCK (binary), DOCK_URL, LOGS (dir) and T0 (epoch seconds at the start of the run); jq and curl.
# With PUSH_PULL=auto|require (default auto) it also round-trips a local QEMU dock through dock push and dock pull;
# auto skips that when the control plane has no import route (the released v0.2.1), require fails instead.
# Under Actions, fd 3 is the step's own stdout: the key mask goes there, never into the tee'd log.
set -euo pipefail
: "${DOCK:?path of the dock binary}" "${DOCK_URL:?control plane URL}" "${LOGS:?log dir}" "${T0:?start epoch}"
DISK_GIB="${DISK_GIB:-40}"
EXTRA_DISK_GIB="${EXTRA_DISK_GIB:-20 80}"
IMAGE="${IMAGE:-node-22}"
PUSH_PULL="${PUSH_PULL:-auto}"
case "$PUSH_PULL" in auto|skip|require) ;; *) echo "PUSH_PULL must be auto, skip or require" >&2; exit 1 ;; esac
mkdir -p "$LOGS"
TIMINGS="$LOGS/timings.txt"
export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-$LOGS/config}"
# The session cookie stays out of LOGS, which is uploaded as an artifact.
COOKIES="$(mktemp)"
trap 'rm -f "$COOKIES"' EXIT

log() { printf 'e2e-selfhost: %s\n' "$*"; }
die() { printf 'e2e-selfhost: FAIL: %s\n' "$*" >&2; exit 1; }
# timing <label> <start epoch>: seconds since <start epoch>.
timing() { printf '%-34s %5ss\n' "$1" "$(( $(date +%s) - $2 ))" | tee -a "$TIMINGS"; }

# api <method> <path> [json body]: prints the response body, fails on a non-2xx status.
api() {
  local method="$1" path="$2" body="${3:-}" out code
  out="$(mktemp)"
  code="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "${DOCK_URL}${path}" \
    -b "$COOKIES" -c "$COOKIES" -H "Origin: ${DOCK_URL}" -H 'content-type: application/json' \
    ${body:+--data "$body"})"
  case "$code" in 2??) cat "$out"; rm -f "$out" ;; *) cat "$out" >&2; rm -f "$out"; die "${method} ${path} -> HTTP ${code}" ;; esac
}

# 1. Account, org, workspace, API key: the same calls the dashboard makes.
EMAIL="e2e-$(date +%s)@example.com"
PASSWORD="$(openssl rand -hex 16)"
api POST /api/auth/sign-up/email "$(jq -nc --arg e "$EMAIL" --arg p "$PASSWORD" '{email:$e,password:$p,name:"e2e"}')" >/dev/null
ORG="$(api POST /v1/organizations '{"name":"selfhost-e2e"}' | jq -r '.data.id // .id')"
[ -n "$ORG" ] && [ "$ORG" != null ] || die "no organization id"
WS="$(api POST "/v1/organizations/${ORG}/workspaces" '{"name":"e2e"}' | jq -r '.data.id // .id')"
[ -n "$WS" ] && [ "$WS" != null ] || die "no workspace id"
KEY="$(api POST /v1/api-keys '{"name":"e2e"}' | jq -r '.data.apiKey // .apiKey')"
case "$KEY" in dock_*) ;; *) die "no API key in the response" ;; esac
if { true >&3; } 2>/dev/null; then echo "::add-mask::${KEY}" >&3; fi
log "org ${ORG}, workspace ${WS}, key created"

# 2. A self-hosted CLI context; the key comes from the environment, never argv or the log.
export DOCK_API_KEY="$KEY"
$DOCK context add selfhost --url "$DOCK_URL" --workspace "$WS"
$DOCK context use selfhost

# Root size in whole GiB as df reports it inside the dock ("39G" for a 40 GiB disk).
root_gib() { $DOCK exec "$1" -- df -BG --output=size / | tail -n 1 | tr -dc '0-9'; }
# ext4 metadata and the reserved blocks cost a few percent: accept 90% to 100% of the request.
assert_grown() {
  local name="$1" want="$2" got min
  got="$(root_gib "$name")"
  min=$(( want * 90 / 100 ))
  log "${name}: df / reports ${got}G (disk ${want} GiB, need >= ${min}G)"
  [ "$got" -ge "$min" ] && [ "$got" -le "$want" ] || { $DOCK exec "$name" -- df -h / >&2 || true; die "${name}: root filesystem is ${got}G, does not match ${want} GiB"; }
  echo "rootfs ${name}: ${got}G of ${want} GiB" >> "$TIMINGS"
}

# A new host reaches the control plane's scheduler a beat after bootstrap reports ready; retry the first create.
new_dock() {
  local attempt
  for attempt in $(seq 1 30); do
    if $DOCK new "$@"; then return 0; fi
    log "dock new attempt ${attempt} failed; retrying"
    sleep 10
  done
  die "dock new $* never succeeded"
}

# 3. A dock whose disk is larger than the 20 GiB base.
t=$(date +%s)
new_dock e2e --cpus 2 --memory 2gb --disk "${DISK_GIB}gb"
timing "dock new (create to ready)" "$t"
timing "start to first dock" "$T0"
assert_grown e2e "$DISK_GIB"

# 4. exec, stop/resume keeps data and the grown disk.
[ "$($DOCK exec e2e -- uname -s)" = Linux ] || die "unexpected uname"
$DOCK exec e2e -- 'echo kept > /home/user/work/marker && sync'
t=$(date +%s); $DOCK stop e2e; timing "dock stop" "$t"
t=$(date +%s); $DOCK resume e2e; timing "dock resume" "$t"
[ "$($DOCK exec e2e -- cat /home/user/work/marker)" = kept ] || die "data lost across stop/resume"
assert_grown e2e "$DISK_GIB"

# 5. branch keeps the data and the grown disk.
t=$(date +%s); $DOCK branch e2e --name e2e-b; timing "dock branch" "$t"
[ "$($DOCK exec e2e-b -- cat /home/user/work/marker)" = kept ] || die "data missing in the branch"
assert_grown e2e-b "$DISK_GIB"
$DOCK ls
$DOCK rm e2e-b
$DOCK rm e2e

# 6. The other sizes docklet#10 asks for, one at a time.
for gib in $EXTRA_DISK_GIB; do
  t=$(date +%s)
  new_dock "e2e-${gib}" --cpus 2 --memory 2gb --disk "${gib}gb"
  timing "dock new --disk ${gib}gb" "$t"
  assert_grown "e2e-${gib}" "$gib"
  $DOCK rm "e2e-${gib}"
done

# 7. A Dockyard image served by this host.
t=$(date +%s)
sudo docklet image pull "$IMAGE"
timing "docklet image pull ${IMAGE}" "$t"
t=$(date +%s)
new_dock img --image "$IMAGE" --cpus 2 --memory 2gb
timing "dock new --image ${IMAGE}" "$t"
out="$($DOCK exec img -- node --version)"
log "node --version: ${out}"
case "$out" in v22.*) ;; *) die "expected node v22, got ${out}" ;; esac
$DOCK rm img

# 8. dock push / dock pull round trip: a local QEMU dock goes up to this control plane and its remote
# successor comes back down, carrying a marker written on each side.
# The control plane must have the import and export routes (usedock/dock#122, newer than v0.2.1). An empty
# POST to the import route is 404 when the route is missing and 400 when it validates the body.
import_supported() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${DOCK_URL}/v1/docks/import" \
    -b "$COOKIES" -c "$COOKIES" -H "Origin: ${DOCK_URL}" -H 'content-type: application/json' --data '{}')"
  case "$code" in
    404) return 1 ;;
    400|422) return 0 ;;
    *) die "probe of POST /v1/docks/import returned HTTP ${code}, expected 404 (unsupported) or 400" ;;
  esac
}

# retry <attempts> <cmd...>: the remote dock may need a beat after it reports running before exec works.
retry() {
  local n="$1" i; shift
  for i in $(seq 1 "$n"); do
    if "$@"; then return 0; fi
    sleep 10
  done
  return 1
}

push_pull() {
  local t out
  # The local engine keeps its disks and images here, apart from the host's docklet state.
  export DOCK_DATA_DIR="${DOCK_DATA_DIR:-${RUNNER_TEMP:-/tmp}/dock-local}"
  mkdir -p "$DOCK_DATA_DIR"
  # Local commands run with the local context active; push and pull run with the self-hosted one so
  # DOCK_API_KEY applies to it.
  $DOCK context use local
  t=$(date +%s); $DOCK images pull "$IMAGE"; timing "local: dock images pull ${IMAGE}" "$t"
  t=$(date +%s); $DOCK new pp --image "$IMAGE" --cpus 2 --memory 2gb --disk 20gb; timing "local: dock new" "$t"
  $DOCK exec pp -- 'echo local-marker > /home/user/work/local && sync'
  [ "$($DOCK exec pp -- cat /home/user/work/local)" = local-marker ] || die "local marker not written"
  t=$(date +%s); $DOCK stop pp; timing "local: dock stop" "$t"

  $DOCK context use selfhost
  t=$(date +%s); $DOCK push pp --to selfhost --name pp-remote; timing "dock push (to running remotely)" "$t"
  retry 12 $DOCK exec pp-remote -- true || die "pushed dock never became reachable"
  out="$($DOCK exec pp-remote -- cat /home/user/work/local)"
  [ "$out" = local-marker ] || die "local marker missing after push (got: ${out})"
  $DOCK exec pp-remote -- 'echo remote-marker > /home/user/work/remote && sync'
  [ "$($DOCK exec pp-remote -- cat /home/user/work/remote)" = remote-marker ] || die "remote marker not written"

  t=$(date +%s); $DOCK pull pp-remote --from selfhost --name pulled; timing "dock pull" "$t"
  $DOCK context use local
  t=$(date +%s); $DOCK resume pulled; timing "local: dock resume (pulled)" "$t"
  out="$($DOCK exec pulled -- cat /home/user/work/local)"
  [ "$out" = local-marker ] || die "pulled dock lost the local marker (got: ${out})"
  out="$($DOCK exec pulled -- cat /home/user/work/remote)"
  [ "$out" = remote-marker ] || die "pulled dock lost the remote marker (got: ${out})"
  log "push/pull round trip ok"

  $DOCK rm pulled
  $DOCK rm pp
  $DOCK context use selfhost
  $DOCK rm pp-remote
}

case "$PUSH_PULL" in
  skip) log "push/pull round trip skipped (PUSH_PULL=skip)"; echo "push/pull: skipped" >> "$TIMINGS" ;;
  *)
    if import_supported; then
      push_pull
    elif [ "$PUSH_PULL" = require ]; then
      die "PUSH_PULL=require, but this control plane has no POST /v1/docks/import"
    else
      echo "::notice::dock push/pull round trip skipped: this control plane has no import route (released usedock/dock before #122); run with images=build to exercise it"
      echo "push/pull: skipped (control plane too old)" >> "$TIMINGS"
    fi
    ;;
esac
log "ok"
