#!/usr/bin/env bash
# CI: downloads the pinned Dockyard index, checks it against the hash pinned in the dock
# binary's source, and verifies its cosign bundle against the images workflow identity.
set -euo pipefail
cd "$(dirname "$0")/.."
tag="$(sed -n 's/.*DockyardTag = "\(images-[^"]*\)".*/\1/p' internal/local/dockyard_pin.go)"
want="$(sed -n 's/.*DockyardIndexSHA256 = "\([0-9a-f]\{64\}\)".*/\1/p' internal/local/dockyard_pin.go)"
[ -n "$tag" ] && [ -n "$want" ] || { echo "no Dockyard release pinned in internal/local/dockyard_pin.go" >&2; exit 1; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
base="https://github.com/usedock/docklet/releases/download/${tag}"
curl -fsSL -o "$tmp/dockyard.json" "${base}/dockyard.json"
curl -fsSL -o "$tmp/dockyard.json.bundle" "${base}/dockyard.json.bundle"
got="$(sha256sum "$tmp/dockyard.json" | cut -d' ' -f1)"
[ "$got" = "$want" ] || { echo "dockyard.json sha256 is $got, pinned $want" >&2; exit 1; }
cosign verify-blob \
  --bundle "$tmp/dockyard.json.bundle" \
  --certificate-identity "https://github.com/usedock/docklet/.github/workflows/images.yml@refs/tags/${tag}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "$tmp/dockyard.json"
echo "dockyard.json of ${tag} matches the pin and its cosign bundle"
