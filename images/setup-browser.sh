#!/bin/bash
# Runs inside the ubuntu-24.04 rootfs: Chromium and its system dependencies.
# The noble chromium package is a snap stub, so use the pinned Playwright build.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
PW=1.52.0
apt-get update
apt-get install -y --no-install-recommends nodejs npm fonts-liberation
export PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
npx --yes "playwright@$PW" install --with-deps chromium
chmod -R a+rX /opt/ms-playwright
BIN="$(ls /opt/ms-playwright/chromium-*/chrome-linux/chrome | head -1)"
ln -sf "$BIN" /usr/local/bin/chromium
echo 'PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright' >> /etc/environment
apt-get purge -y nodejs npm
apt-get autoremove -y --purge
rm -rf /root/.npm /root/.cache
chromium --version
