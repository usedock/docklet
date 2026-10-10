#!/usr/bin/env bash
# Extracts the raw ELF vmlinux Firecracker boots from a distro bzImage.
# Usage: extract-vmlinux.sh <vmlinuz> <out-vmlinux>
set -euo pipefail
VMLINUZ="$1"
OUT="$2"
python3 - "$VMLINUZ" "$OUT" << 'EXTRACT'
import re, subprocess, sys
data = open(sys.argv[1], "rb").read()
cands = []
for pat, dec in [(b"\\x1f\\x8b\\x08\\x00", "gunzip"), (b"\\x28\\xb5\\x2f\\xfd", "unzstd")]:
    for m in re.finditer(pat, data):
        cands.append((m.start(), dec))
for pos, dec in sorted(cands, reverse=True):
    p = subprocess.run([dec, "-c"], input=data[pos:], capture_output=True)
    if p.stdout[:4] == b"\x7fELF":
        open(sys.argv[2], "wb").write(p.stdout)
        print("extracted vmlinux (%d bytes) via %s" % (len(p.stdout), dec))
        break
else:
    sys.exit("extract-vmlinux: no ELF payload found")
EXTRACT
