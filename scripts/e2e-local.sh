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
[ -f "$dir/disk.qcow2" ]
t0=$(date +%s)
$DOCK resume e2e
echo "e2e: resume took $(( $(date +%s) - t0 ))s"
[ "$(stat -c %i "$dir/disk.qcow2")" = "$disk_before" ]
[ "$($DOCK exec e2e -- cat /home/user/work/marker)" = kept ]

$DOCK branch e2e --name e2e-b
[ "$($DOCK exec e2e-b -- cat /home/user/work/marker)" = kept ]
$DOCK ls
$DOCK rm e2e-b
$DOCK rm e2e
[ ! -e "$dir" ]
[ "$($DOCK ls --all --json)" = "[]" ]
echo "e2e: ok"
