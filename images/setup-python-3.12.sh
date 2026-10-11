#!/bin/bash
# Runs inside the ubuntu-24.04 rootfs: Python 3.12 (the noble default) with pip and venv.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends python3-pip python3-venv python3-dev build-essential python-is-python3
python3 --version | grep -q '^Python 3\.12'
