#!/usr/bin/env bash
# Sourced by scripts/e2e-selfhost.sh and scripts/e2e-selfhost-gvisor.sh: helpers for driving the dock CLI
# against a self-hosted control plane. The caller has set -euo pipefail and exports DOCK (binary), DOCK_URL,
# LOGS (dir). Under Actions, fd 3 is the step's own stdout: the key mask goes there, never into the tee'd log.

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

# Account, org, workspace, API key (the same calls the dashboard makes), then a "selfhost" CLI context.
# The key comes from the environment, never argv or the log.
setup_selfhost_context() {
  local email password org ws key
  email="e2e-$(date +%s)@example.com"
  password="$(openssl rand -hex 16)"
  api POST /api/auth/sign-up/email "$(jq -nc --arg e "$email" --arg p "$password" '{email:$e,password:$p,name:"e2e"}')" >/dev/null
  org="$(api POST /v1/organizations '{"name":"selfhost-e2e"}' | jq -r '.data.id // .id')"
  [ -n "$org" ] && [ "$org" != null ] || die "no organization id"
  ws="$(api POST "/v1/organizations/${org}/workspaces" '{"name":"e2e"}' | jq -r '.data.id // .id')"
  [ -n "$ws" ] && [ "$ws" != null ] || die "no workspace id"
  key="$(api POST /v1/api-keys '{"name":"e2e"}' | jq -r '.data.apiKey // .apiKey')"
  case "$key" in dock_*) ;; *) die "no API key in the response" ;; esac
  if { true >&3; } 2>/dev/null; then echo "::add-mask::${key}" >&3; fi
  log "org ${org}, workspace ${ws}, key created"
  export DOCK_API_KEY="$key"
  $DOCK context add selfhost --url "$DOCK_URL" --workspace "$ws"
  $DOCK context use selfhost
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

# retry <attempts> <cmd...>: a remote dock may need a beat after it reports running before exec works.
retry() {
  local n="$1" i; shift
  for i in $(seq 1 "$n"); do
    if "$@"; then return 0; fi
    sleep 10
  done
  return 1
}
