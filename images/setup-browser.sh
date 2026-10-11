#!/bin/bash
# Runs inside the ubuntu-24.04 rootfs: Chromium and its system dependencies.
# The noble chromium package is a snap stub, so use the Playwright build.
#
# Pinning: Playwright comes from images/browser (package.json + package-lock.json,
# copied to /tmp/dock-setup-data by build.sh) and is installed with `npm ci`, which
# checks every package against the lockfile's sha512 integrity. Playwright itself
# does not verify the Chromium zip it downloads (browsers.json carries only a
# revision, no hash), so the zip cannot be pinned directly. Instead the extracted
# chrome binary is checked against CHROME_SHA256 below, which fails the build if
# the CDN serves anything but the expected build. To upgrade Playwright, regenerate
# the lockfile and update PW_CHROMIUM_REV and CHROME_SHA256 together
# (sha256 of chrome-linux/chrome inside chromium-linux.zip, and chromium-linux-arm64.zip
# for arm64, for that revision).
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
PW_CHROMIUM_REV=1169 # Playwright 1.52.0, Chromium 136.0.7103.25
CHROME_SHA256_AMD64=88aa5dcd6b16a1692c77445b64343e192fef3051ad6537bb2819ff97392b2ee2
CHROME_SHA256_ARM64=c31ada281033d3a97ec15ed32523537b535c7b616eb35f7b2db25d345dce74ce
case "$(uname -m)" in
  x86_64) CHROME_SHA256="$CHROME_SHA256_AMD64" ;;
  aarch64) CHROME_SHA256="$CHROME_SHA256_ARM64" ;;
  *) echo "unsupported arch" >&2; exit 1 ;;
esac
DATA=/tmp/dock-setup-data
apt-get update
apt-get install -y --no-install-recommends nodejs npm fonts-liberation
export PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
(cd "$DATA" && npm ci --ignore-scripts --no-audit --no-fund && ./node_modules/.bin/playwright install --with-deps chromium)
BIN="/opt/ms-playwright/chromium-$PW_CHROMIUM_REV/chrome-linux/chrome"
echo "$CHROME_SHA256  $BIN" | sha256sum -c -
chmod -R a+rX /opt/ms-playwright
ln -sf "$BIN" /usr/local/bin/chromium
echo 'PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright' >> /etc/environment
rm -rf "$DATA"
apt-get purge -y nodejs npm
apt-get autoremove -y --purge
rm -rf /root/.npm /root/.cache
chromium --version
