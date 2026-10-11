#!/usr/bin/env bash
# CI: drives the dock CLI against a self-hosted control plane whose only host is this machine
# (microvm, KVM). Creates the account, org, workspace and API key through the API, then checks that
# the root filesystem grows to the requested disk (docklet#10) across create, stop/resume and branch,
# and that a Dockyard image can be pulled and booted. Needs DOCK (binary), DOCK_URL, LOGS (dir) and
# T0 (epoch seconds at the start of the run); jq and curl.
set -euo pipefail
: "${DOCK:?path of the dock binary}" "${DOCK_URL:?control plane URL}" "${LOGS:?log dir}" "${T0:?start epoch}"
DISK_GIB="${DISK_GIB:-40}"
IMAGE="${IMAGE:-node-22}"
mkdir -p "$LOGS"
TIMINGS="$LOGS/timings.txt"
export XDG_CONFIG_HOME="${XDG_CONFIG_HOME:-$LOGS/config}"

log() { printf 'e2e-selfhost: %s\n' "$*"; }
die() { printf 'e2e-selfhost: FAIL: %s\n' "$*" >&2; exit 1; }
# timing <label> <start epoch>: seconds since <start epoch>.
timing() { printf '%-34s %5ss\n' "$1" "$(( $(date +%s) - $2 ))" | tee -a "$TIMINGS"; }

# api <method> <path> [json body]: prints the response body, fails on a non-2xx status.
api() {
  local method="$1" path="$2" body="${3:-}" out code
  out="$(mktemp)"
  code="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" "${DOCK_URL}${path}" \
    -b "$LOGS/cookies" -c "$LOGS/cookies" -H "Origin: ${DOCK_URL}" -H 'content-type: application/json' \
    ${body:+--data "$body"})"
  case "$code" in 2??) cat "$out" ;; *) cat "$out" >&2; die "${method} ${path} -> HTTP ${code}" ;; esac
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
echo "::add-mask::${KEY}"
log "org ${ORG}, workspace ${WS}, key created"

# 2. A self-hosted CLI context; the key comes from the environment, never argv or the log.
export DOCK_API_KEY="$KEY"
$DOCK context add selfhost --url "$DOCK_URL" --workspace "$WS"
$DOCK context use selfhost

# Root size in whole GiB as df reports it inside the dock ("39G" for a 40 GiB disk).
root_gib() { $DOCK exec "$1" -- df -BG --output=size / | tail -n 1 | tr -dc '0-9'; }
# ext4 metadata and the reserved blocks cost a few percent: accept >= 90% of the request, never the 20 GiB base.
assert_grown() {
  local name="$1" want="$2" got min
  got="$(root_gib "$name")"
  min=$(( want * 90 / 100 ))
  log "${name}: df / reports ${got}G (disk ${want} GiB, need >= ${min}G)"
  [ "$got" -ge "$min" ] || { $DOCK exec "$name" -- df -h / >&2 || true; die "${name}: root filesystem is ${got}G, did not grow to ${want} GiB"; }
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

# 6. A Dockyard image served by this host.
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
log "ok"
