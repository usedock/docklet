#!/bin/bash
# Boot-time guest setup. Runs before the guest agent.
# 1. Writes /etc/dock-fast-agent.json from the QEMU fw_cfg file opt/usedock/cfg
#    (plain JSON) when the engine passes one; it is rewritten on every boot.
#    Otherwise, when the file is missing, from dock.cfg=<base64 json> on the
#    kernel command line (older engines). The content is never logged.
# 2. Sets the hostname when the config carries one.
#    A config with "resetHostKeys": true (first boot of a copied disk) also deletes the
#    sshd host keys; ssh.service regenerates them (ssh-keygen -A) after this unit.
# 3. Grows the root filesystem to the size of its disk.
# Networking needs no step here: the image ships a DHCP networkd unit that a
# static 10-dock.network (written by fleet provisioning) outranks.
set -u

CFG=/etc/dock-fast-agent.json
FWCFG=/sys/firmware/qemu_fw_cfg/by_name/opt/usedock/cfg/raw
umask 077

# Installs $CFG.tmp as $CFG when it is valid JSON; never prints the content.
install_cfg() {
  if python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$CFG.tmp" 2>/dev/null; then
    chmod 0600 "$CFG.tmp"
    mv "$CFG.tmp" "$CFG"
  else
    echo "dock-guest-init: $1 is not valid json" >&2
    rm -f "$CFG.tmp"
  fi
}

[ -e "$FWCFG" ] || modprobe -q qemu_fw_cfg 2>/dev/null || true
if [ -r "$FWCFG" ]; then
  if cat "$FWCFG" > "$CFG.tmp" 2>/dev/null; then
    install_cfg "fw_cfg opt/usedock/cfg"
  else
    echo "dock-guest-init: cannot read fw_cfg opt/usedock/cfg" >&2
    rm -f "$CFG.tmp"
  fi
elif [ ! -s "$CFG" ]; then
  b64=""
  read -r -a toks < /proc/cmdline
  for tok in "${toks[@]}"; do
    case "$tok" in dock.cfg=*) b64="${tok#dock.cfg=}" ;; esac
  done
  if [ -n "$b64" ]; then
    if printf '%s' "$b64" | base64 -d > "$CFG.tmp" 2>/dev/null; then
      install_cfg "dock.cfg"
    else
      echo "dock-guest-init: dock.cfg is not valid base64" >&2
      rm -f "$CFG.tmp"
    fi
  fi
fi

if [ -s "$CFG" ]; then
  if python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get("resetHostKeys") is True else 1)' "$CFG" 2>/dev/null; then
    rm -f /etc/ssh/ssh_host_*
  fi
  name="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("hostname",""))' "$CFG" 2>/dev/null || true)"
  if [[ "$name" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,62})$ ]]; then
    hostnamectl set-hostname "$name" 2>/dev/null || hostname "$name" || true
  elif [ -n "$name" ]; then
    echo "dock-guest-init: ignoring invalid hostname" >&2
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
