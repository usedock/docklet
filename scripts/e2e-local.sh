#!/usr/bin/env bash
# CI: drives the dock CLI through a local dock lifecycle on KVM. Needs DOCK (the binary) and
# DOCK_DATA_DIR; QEMU and /dev/kvm must be available.
set -euo pipefail
: "${DOCK:?path of the dock binary}" "${DOCK_DATA_DIR:?data dir}"
export DOCK_DATA_DIR

$DOCK doctor
$DOCK images
t0=$(date +%s)
$DOCK images pull ubuntu-24.04
echo "e2e: image pull took $(( $(date +%s) - t0 ))s"
$DOCK images --json | grep -q '"cached": true'

t0=$(date +%s)
$DOCK new e2e --cpus 2 --memory 1gb --disk 8gb --json | tee "$DOCK_DATA_DIR/new.json"
echo "e2e: dock new (boot to ready) took $(( $(date +%s) - t0 ))s"
id="$(sed -n 's/.*"id": "\(dk_[0-9a-f]*\)".*/\1/p' "$DOCK_DATA_DIR/new.json")"
[ -n "$id" ]
dir="$DOCK_DATA_DIR/docks/$id"

out="$($DOCK exec e2e -- uname -a)"; echo "e2e: uname: $out"
case "$out" in Linux*) ;; *) echo "unexpected uname" >&2; exit 1 ;; esac
out="$($DOCK exec e2e -- docker --version)"; echo "e2e: $out"
case "$out" in "Docker version"*) ;; *) echo "no docker in the image" >&2; exit 1 ;; esac
$DOCK exec e2e -- 'echo kept > /home/user/work/marker'
$DOCK exec e2e -- getent hosts github.com

# The agent token must not be visible to other users through the process list.
tok="$(cat "$dir/token")"
if ps -eo args | grep -F "$tok" | grep -v grep; then echo "token in a process argv" >&2; exit 1; fi
if ps -eo args | grep qemu-system | grep -q 'dock\.cfg'; then echo "dock.cfg on the qemu command line" >&2; exit 1; fi

disk_before="$(stat -c %i "$dir/disk.qcow2")"
t0=$(date +%s)
$DOCK stop e2e
echo "e2e: stop took $(( $(date +%s) - t0 ))s"
$DOCK ls --all | grep -q archived
# Stopping must end the VM process, not just forget about it.
if pgrep -f qemu-system >/dev/null; then echo "a qemu process is still running after dock stop" >&2; ps -eo pid,etime,args | grep '[q]emu-system' >&2; exit 1; fi
[ -f "$dir/disk.qcow2" ]
t0=$(date +%s)
$DOCK resume e2e
[ "$(pgrep -fc qemu-system)" = 1 ] || { echo "expected exactly one qemu process after resume" >&2; exit 1; }
echo "e2e: resume took $(( $(date +%s) - t0 ))s"
[ "$(stat -c %i "$dir/disk.qcow2")" = "$disk_before" ]
[ "$($DOCK exec e2e -- cat /home/user/work/marker)" = kept ]

# Host keys: a branch gets its own, and every dock keeps its keys across stop/resume. Needs a guest-init that
# knows resetHostKeys, so images from before that change are skipped.
# ssh.service makes missing keys at boot (ssh-keygen -A), so wait for them after each boot.
fp() { $DOCK exec "$1" -- "for i in \$(seq 60); do [ -s /etc/ssh/ssh_host_ed25519_key.pub ] && break; sleep 1; done; ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | cut -d' ' -f2"; }
hk=0
if $DOCK exec e2e -- grep -q resetHostKeys /usr/local/libexec/dock-guest-init; then hk=1; hk_src="$(fp e2e)"; [ -n "$hk_src" ]; fi
$DOCK branch e2e --name e2e-b
[ "$($DOCK exec e2e-b -- cat /home/user/work/marker)" = kept ]
if [ "$hk" = 1 ]; then
  hk_b="$(fp e2e-b)"; [ -n "$hk_b" ]
  [ "$hk_b" != "$hk_src" ] || { echo "branch kept its source ssh host keys" >&2; exit 1; }
  $DOCK stop e2e-b; $DOCK resume e2e-b
  [ "$(fp e2e-b)" = "$hk_b" ] || { echo "branch ssh host keys changed across stop/resume" >&2; exit 1; }
  [ "$(fp e2e)" = "$hk_src" ] || { echo "source ssh host keys changed after a branch" >&2; exit 1; }
else
  echo "e2e: image predates resetHostKeys, skipping the host key check"
fi
$DOCK ls
$DOCK rm e2e-b

# Network isolation: a --network off dock must not reach host listeners on 127.0.0.1 or ::1,
# through the slirp gateway 10.0.2.2 or its IPv6 counterpart fec0::2 (see README, local docks).
lport() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
p4="$(lport)"; p6="$(lport)"
python3 -m http.server "$p4" --bind 127.0.0.1 >/dev/null 2>&1 &
l4=$!
python3 -m http.server "$p6" --bind ::1 >/dev/null 2>&1 &
l6=$!
trap 'kill $l4 $l6 2>/dev/null || true' EXIT
sleep 1
# Control: the listeners work from the host, so a refusal in the guest means isolation.
curl -fsS --retry 10 --retry-connrefused --retry-delay 1 "http://127.0.0.1:$p4/" >/dev/null
curl -fsS -g --retry 10 --retry-connrefused --retry-delay 1 "http://[::1]:$p6/" >/dev/null
probe() { # name host port: exit 0 when the guest could connect
  $DOCK exec "$1" -- "timeout 8 bash -c 'exec 3<>/dev/tcp/$2/$3'" >/dev/null 2>&1
}
$DOCK new e2e-off --cpus 1 --memory 1gb --disk 8gb --network off
$DOCK exec e2e-off -- true
# ipv6=off: the guest has no IPv6 route out.
[ -z "$($DOCK exec e2e-off -- 'ip -6 route show default')" ] || { echo "guest has an IPv6 default route" >&2; exit 1; }
for h in 10.0.2.2 fec0::2; do
  if probe e2e-off "$h" "$p4"; then echo "network off dock reached host 127.0.0.1 via $h" >&2; exit 1; fi
  if probe e2e-off "$h" "$p6"; then echo "network off dock reached host ::1 via $h" >&2; exit 1; fi
done
$DOCK exec e2e-off -- 'echo agent-ok' | grep -q agent-ok
$DOCK rm e2e-off
# With the network on, slirp maps 10.0.2.2 to the host's 127.0.0.1. Images from the release that
# adds block_gateway refuse it in the guest; older images ignore the flag. CI pins such a release
# and sets DOCK_E2E_GATEWAY_BLOCK=1; without it the block is only reported, for older images.
$DOCK exec e2e -- getent hosts github.com >/dev/null
$DOCK exec e2e -- 'echo agent-ok' | grep -q agent-ok
if probe e2e 10.0.2.2 "$p4"; then
  if [ "${DOCK_E2E_GATEWAY_BLOCK:-}" = 1 ]; then
    echo "network on dock reached host 127.0.0.1 via 10.0.2.2" >&2; exit 1
  fi
  echo "e2e: note: this image does not block 10.0.2.2 yet (DOCK_E2E_GATEWAY_BLOCK unset)"
elif [ "${DOCK_E2E_GATEWAY_BLOCK:-}" != 1 ]; then
  echo "e2e: the image blocks 10.0.2.2; set DOCK_E2E_GATEWAY_BLOCK=1 to enforce it"
fi
# DNS and the internet must keep working with the gateway blocked.
$DOCK exec e2e -- 'curl -fsS -m 20 -o /dev/null https://github.com'
$DOCK rm e2e
[ ! -e "$dir" ]
[ "$($DOCK ls --all --json)" = "[]" ]
echo "e2e: ok"
