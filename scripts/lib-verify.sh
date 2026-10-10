#!/usr/bin/env bash
# Release verification helpers, sourced by bootstrap-host.sh and tested by test-verify.sh.
# Callers must define log() and die(); die must not return.

# Pinned cosign, used unless a preinstalled one reports exactly this version; bump with its published sha256.
COSIGN_VERSION="${COSIGN_VERSION:-v2.4.1}"
COSIGN_SHA256_AMD64="${COSIGN_SHA256_AMD64:-8b24b946dd5809c6bd93de08033bcf6bc0ed7d336b7785787c080f574b89249b}"
COSIGN_SHA256_ARM64="${COSIGN_SHA256_ARM64:-3b2e2e3854d0356c45fe6607047526ccd04742d20bd44afb5be91fa2a6e7cb4a}"
# Only artifacts signed by this repo's release workflow at the exact release tag are accepted.
COSIGN_IDENTITY_PREFIX='https://github.com/usedock/docklet/.github/workflows/release.yml@refs/tags/'
COSIGN_OIDC_ISSUER='https://token.actions.githubusercontent.com'

# Prints the sha256 of a file.
file_sha256() {
  sha256sum "$1" | awk '{print $1}'
}

# Prints the checksum recorded for a file name in a checksums.txt, or nothing.
checksum_for() {
  awk -v n="$2" '{f=$2; sub(/^\*/, "", f); if (f == n) {print $1; exit}}' "$1"
}

# verify_checksum <file> <checksums.txt> <name>: dies unless the file matches its entry.
verify_checksum() {
  local file="$1" sums="$2" name="$3" want got
  [ -f "$file" ] || die "missing file to verify: ${file}"
  [ -f "$sums" ] || die "missing checksums file: ${sums}"
  want="$(checksum_for "$sums" "$name")"
  [ -n "$want" ] || die "no checksum for ${name} in checksums.txt"
  got="$(file_sha256 "$file")"
  [ "$got" = "$want" ] || die "checksum mismatch for ${name}: expected ${want}, got ${got}"
}

# verify_signature <cosign> <file> <bundle> <version>: dies unless the bundle is valid for the file at that tag.
verify_signature() {
  local cosign="$1" file="$2" bundle="$3" version="${4:-}" err
  [ -n "$version" ] || die "no release version given to verify ${file}"
  [ -n "$cosign" ] && [ -x "$cosign" ] || die "cosign is not available; cannot verify ${file}"
  [ -f "$bundle" ] || die "missing signature bundle for ${file}: ${bundle}"
  if ! err="$("$cosign" verify-blob \
    --bundle "$bundle" \
    --certificate-identity "${COSIGN_IDENTITY_PREFIX}${version}" \
    --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER" \
    "$file" 2>&1 >/dev/null)"; then
    printf '%s\n' "$err" >&2
    die "signature verification FAILED for ${file}"
  fi
}

# ensure_cosign <workdir>: prints a usable cosign; a preinstalled one must be exactly the pinned version.
ensure_cosign() {
  local dir="$1" arch want url have
  if have="$(command -v cosign 2>/dev/null)" && [ -n "$have" ] \
    && "$have" version 2>/dev/null | grep -Eq "^GitVersion:[[:space:]]+${COSIGN_VERSION}\$"; then
    printf '%s\n' "$have"
    return 0
  fi
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64; want="$COSIGN_SHA256_AMD64" ;;
    aarch64|arm64) arch=arm64; want="$COSIGN_SHA256_ARM64" ;;
    *) die "unsupported architecture for cosign: $(uname -m)" ;;
  esac
  url="https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}/cosign-linux-${arch}"
  curl -fsSL -o "${dir}/cosign" "$url" || die "could not download cosign ${COSIGN_VERSION}"
  [ "$(file_sha256 "${dir}/cosign")" = "$want" ] || die "cosign ${COSIGN_VERSION} checksum mismatch; refusing to run it"
  chmod 0755 "${dir}/cosign"
  printf '%s\n' "${dir}/cosign"
}

# fetch_release <version> <workdir> <asset>: downloads a verified release asset into workdir.
fetch_release() {
  local version="$1" dir="$2" asset="$3" base cosign
  base="${DOCKLET_RELEASE_BASE_URL:-https://github.com/usedock/docklet/releases/download}/${version}"
  for f in "$asset" checksums.txt checksums.txt.bundle "${asset}.bundle"; do
    curl -fsSL -o "${dir}/${f}" "${base}/${f}" || die "could not download ${f} from ${base}"
  done
  cosign="$(ensure_cosign "$dir")"
  verify_signature "$cosign" "${dir}/checksums.txt" "${dir}/checksums.txt.bundle" "$version"
  verify_signature "$cosign" "${dir}/${asset}" "${dir}/${asset}.bundle" "$version"
  verify_checksum "${dir}/${asset}" "${dir}/checksums.txt" "$asset"
}
