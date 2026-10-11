#!/bin/bash
# Runs inside the ubuntu-24.04 rootfs: installs a pinned Node.js 22 LTS release
# from nodejs.org. The version and tarball hashes are constants in this file
# (taken from https://nodejs.org/dist/v22.23.3/SHASUMS256.txt), so a changed or
# compromised origin cannot change what ships. To upgrade, bump all three.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
NODE_VERSION=v22.23.3
SHA256_X64=df450af89261115ef9f9e3830c3eeb2cc9213b63c720b1af623cb5dcbe2e02de
SHA256_ARM64=a44aeb94849a299b22df10b9e622ec2f605c2183501bc40590705131de7c740f
apt-get update
apt-get install -y --no-install-recommends xz-utils
case "$(uname -m)" in
  x86_64) A=x64; SHA="$SHA256_X64" ;;
  aarch64) A=arm64; SHA="$SHA256_ARM64" ;;
  *) echo "unsupported arch" >&2; exit 1 ;;
esac
FILE="node-$NODE_VERSION-linux-$A.tar.xz"
cd /tmp
curl -fsSL "https://nodejs.org/dist/$NODE_VERSION/$FILE" -o "$FILE"
echo "$SHA  $FILE" | sha256sum -c -
tar -xJf "$FILE" -C /usr/local --strip-components=1 --exclude='*.md' --exclude=LICENSE
rm -f "$FILE"
apt-get purge -y xz-utils || true
node --version | grep -qx "$NODE_VERSION"
