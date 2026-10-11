#!/bin/bash
# Boot-time guest setup. Runs before the guest agent.
# 1. Writes /etc/dock-fast-agent.json from dock.cfg=<base64 json> on the kernel
#    command line when the file is missing (engines that cannot mount ext4).
# 2. Sets the hostname when the config carries one.
# 3. Grows the root filesystem to the size of its disk.
# Networking needs no step here: the image ships a DHCP networkd unit that a
# static 10-dock.network (written by fleet provisioning) outranks.
set -u

CFG=/etc/dock-fast-agent.json

if [ ! -s "$CFG" ]; then
  b64=""
  for tok in $(cat /proc/cmdline); do
    case "$tok" in dock.cfg=*) b64="${tok#dock.cfg=}" ;; esac
  done
  if [ -n "$b64" ]; then
    umask 077
    if printf '%s' "$b64" | base64 -d > "$CFG.tmp" && python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$CFG.tmp"; then
      chmod 0600 "$CFG.tmp"
      mv "$CFG.tmp" "$CFG"
    else
      echo "dock-guest-init: dock.cfg is not valid base64 json" >&2
      rm -f "$CFG.tmp"
    fi
  fi
fi

if [ -s "$CFG" ]; then
  name="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("hostname",""))' "$CFG" 2>/dev/null || true)"
  if [ -n "$name" ]; then
    hostnamectl set-hostname "$name" 2>/dev/null || hostname "$name" || true
  fi
fi

# Whole-disk ext4 (root=/dev/vda): resize2fs grows it online. A partitioned
# root is grown with growpart first.
dev="$(findmnt -no SOURCE / || echo /dev/vda)"
case "$dev" in
  /dev/vda[0-9]*) growpart /dev/vda "${dev#/dev/vda}" || true ;;
esac
resize2fs "$dev" || echo "dock-guest-init: resize2fs $dev failed" >&2
exit 0
