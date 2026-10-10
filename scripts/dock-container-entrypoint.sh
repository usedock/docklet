#!/usr/bin/env bash
# gvisor container entrypoint: supervise the fast-agent sidecar with the same
# ping/exec/read/rekey protocol as the microvm systemd unit, then run the dock setup script in the background
# without delaying ready. PID 1; forwards SIGTERM to the sidecar.
set -euo pipefail

AGENT_CONFIG="${AGENT_CONFIG:-/etc/dock-fast-agent.json}"
AGENT_PROGRAM="${AGENT_PROGRAM:-/usr/local/libexec/dock-fast-agent.py}"
SETUP_SCRIPT="${DOCK_SETUP_SCRIPT:-}"

# Supervise the fast agent only for fast runtimes (pool slots mount the
# claim config); regular docks run no guest agent, same as microVMs.
# The container already runs as the dock user, so no privilege drop here.
AGENT_PID=""
if [ -f "${AGENT_CONFIG}" ]; then
  mkdir -p /run/dock-fast
  supervise() {
    while true; do
      # shellcheck disable=SC2015
      python3 "${AGENT_PROGRAM}" || true
      echo "dock-entrypoint: fast agent exited, restarting in 1s" >&2
      sleep 1
    done
  }
  supervise &
  AGENT_PID="$!"
fi

# Dock setup runs in the background; ready never waits for it.
if [ -n "${SETUP_SCRIPT}" ]; then
  (bash -c "${SETUP_SCRIPT}" >>/tmp/dock-setup.log 2>&1 || echo "dock-entrypoint: setup failed" >&2) &
fi

shutdown() {
  [ -n "${AGENT_PID}" ] && kill "${AGENT_PID}" 2>/dev/null || true
  wait 2>/dev/null || true
  exit 0
}
trap shutdown TERM INT

# PID 1 stays put: without an init the container exits when this ends.
while true; do sleep 86400 & wait "$!" || true; done
