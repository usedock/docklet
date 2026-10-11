#!/bin/bash
# Runs inside the ubuntu-24.04 rootfs: installs the current Node.js 22 release
# from nodejs.org, checked against the published SHASUMS256.txt.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends xz-utils
case "$(uname -m)" in x86_64) A=x64 ;; aarch64) A=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
BASE=https://nodejs.org/dist/latest-v22.x
cd /tmp
curl -fsSL "$BASE/SHASUMS256.txt" -o SHASUMS256.txt
FILE="$(grep -o "node-v22[0-9.]*-linux-$A\.tar\.xz" SHASUMS256.txt | head -1)"
[ -n "$FILE" ] || { echo "node tarball not listed" >&2; exit 1; }
curl -fsSL "$BASE/$FILE" -o "$FILE"
grep " $FILE\$" SHASUMS256.txt | sha256sum -c -
tar -xJf "$FILE" -C /usr/local --strip-components=1 --exclude='*.md' --exclude=LICENSE
rm -f "$FILE" SHASUMS256.txt
apt-get purge -y xz-utils || true
node --version
