#!/bin/sh
# Installs the dock CLI (macOS, Linux) after verifying its checksum and cosign signature.
#   curl -fsSL https://raw.githubusercontent.com/usedock/docklet/main/scripts/install.sh | sh
# Env: DOCK_VERSION (default: latest release), DOCK_INSTALL_DIR (default: $HOME/.local/bin).
# Test hooks only (CI fakes): DOCK_RELEASE_BASE_URL, DOCK_LATEST_URL, DOCK_OS, DOCK_ARCH. Whatever they point at must
# still pass the pinned cosign identity check, so they cannot bypass verification.
# Self-contained on purpose: the verify logic mirrors scripts/lib-verify.sh.
set -eu

COSIGN_VERSION='v2.4.1'
COSIGN_SHA256_LINUX_AMD64='8b24b946dd5809c6bd93de08033bcf6bc0ed7d336b7785787c080f574b89249b'
COSIGN_SHA256_LINUX_ARM64='3b2e2e3854d0356c45fe6607047526ccd04742d20bd44afb5be91fa2a6e7cb4a'
COSIGN_SHA256_DARWIN_AMD64='666032ca283da92b6f7953965688fd51200fdc891a86c19e05c98b898ea0af4e'
COSIGN_SHA256_DARWIN_ARM64='13343856b69f70388c4fe0b986a31dde5958e444b41be22d785d3dc5e1a9cc62'
# Only artifacts signed by this repo's release workflow at the exact release tag are accepted.
COSIGN_IDENTITY_PREFIX='https://github.com/usedock/docklet/.github/workflows/release.yml@refs/tags/'
COSIGN_OIDC_ISSUER='https://token.actions.githubusercontent.com'

die() { echo "install: $*" >&2; exit 1; }
log() { echo "install: $*"; }

# valid_version <tag>: v<semver> with an optional pre-release, the form release tags use.
valid_version() {
  case "$1" in *[!0-9A-Za-z.-]*) return 1 ;; esac
  printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$'
}

file_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "no sha256sum or shasum found"
  fi
}

# checksum_for <checksums.txt> <name>: the recorded sha256 for exactly that file name, or nothing.
checksum_for() {
  awk -v n="$2" '{f=$2; sub(/^\*/, "", f); if (f == n) {print $1; exit}}' "$1"
}

verify_checksum() {
  file="$1"; sums="$2"; name="$3"
  [ -f "$file" ] || die "missing file to verify: ${file}"
  [ -f "$sums" ] || die "missing checksums file: ${sums}"
  want="$(checksum_for "$sums" "$name")"
  [ -n "$want" ] || die "no checksum for ${name} in checksums.txt"
  got="$(file_sha256 "$file")"
  [ "$got" = "$want" ] || die "checksum mismatch for ${name}: expected ${want}, got ${got}"
}

# verify_signature <cosign> <file> <bundle> <version>
verify_signature() {
  cosign="$1"; file="$2"; bundle="$3"; version="$4"
  valid_version "$version" || die "invalid release version '${version}'; expected v<major>.<minor>.<patch>"
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

# ensure_cosign <workdir> <os> <arch>: prints a usable cosign; a preinstalled one must be exactly the pinned version.
ensure_cosign() {
  dir="$1"; os="$2"; arch="$3"
  if have="$(command -v cosign 2>/dev/null)" && [ -n "$have" ] \
    && out="$("$have" version 2>/dev/null)" \
    && printf '%s\n' "$out" | awk -v want="$COSIGN_VERSION" '$1 == "GitVersion:" && $2 == want { found = 1 } END { exit !found }'; then
    printf '%s\n' "$have"
    return 0
  fi
  case "${os}-${arch}" in
    linux-amd64) want="$COSIGN_SHA256_LINUX_AMD64" ;;
    linux-arm64) want="$COSIGN_SHA256_LINUX_ARM64" ;;
    darwin-amd64) want="$COSIGN_SHA256_DARWIN_AMD64" ;;
    darwin-arm64) want="$COSIGN_SHA256_DARWIN_ARM64" ;;
    *) die "no pinned cosign for ${os}-${arch}" ;;
  esac
  url="https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}/cosign-${os}-${arch}"
  curl -fsSL -o "${dir}/cosign" "$url" || die "could not download cosign ${COSIGN_VERSION}"
  [ "$(file_sha256 "${dir}/cosign")" = "$want" ] || die "cosign ${COSIGN_VERSION} checksum mismatch; refusing to run it"
  chmod 0755 "${dir}/cosign"
  printf '%s\n' "${dir}/cosign"
}

detect_platform() {
  os="${DOCK_OS:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
  m="${DOCK_ARCH:-$(uname -m)}"
  case "$os" in linux|darwin) ;; *) die "unsupported OS: ${os} (use install.ps1 on Windows)" ;; esac
  case "$m" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported architecture: ${m}" ;;
  esac
}

resolve_version() {
  version="${DOCK_VERSION:-}"
  if [ -z "$version" ]; then
    api="${DOCK_LATEST_URL:-https://api.github.com/repos/usedock/docklet/releases/latest}"
    body="$(curl -fsSL "$api")" || die "could not resolve the latest release from ${api}"
    version="$(printf '%s\n' "$body" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  fi
  valid_version "$version" || die "invalid release version '${version}'; expected v<major>.<minor>.<patch>"
}

main() {
  command -v curl >/dev/null 2>&1 || die "curl is required"
  detect_platform
  resolve_version
  asset="dock-${os}-${arch}"
  base="${DOCK_RELEASE_BASE_URL:-https://github.com/usedock/docklet/releases/download}/${version}"
  dest="${DOCK_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}"

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  log "downloading ${asset} ${version}"
  for f in "$asset" checksums.txt checksums.txt.bundle "${asset}.bundle"; do
    curl -fsSL -o "${tmp}/${f}" "${base}/${f}" || die "could not download ${f} from ${base}"
  done

  cosign="$(ensure_cosign "$tmp" "$os" "$arch")"
  verify_signature "$cosign" "${tmp}/checksums.txt" "${tmp}/checksums.txt.bundle" "$version"
  verify_signature "$cosign" "${tmp}/${asset}" "${tmp}/${asset}.bundle" "$version"
  verify_checksum "${tmp}/${asset}" "${tmp}/checksums.txt" "$asset"

  mkdir -p "$dest" || die "could not create ${dest}"
  cp "${tmp}/${asset}" "${dest}/.dock.$$" || die "could not write to ${dest}"
  chmod 0755 "${dest}/.dock.$$"
  mv -f "${dest}/.dock.$$" "${dest}/dock" || die "could not install to ${dest}/dock"
  log "installed dock ${version} to ${dest}/dock"

  case ":${PATH}:" in
    *":${dest}:"*) ;;
    *) log "${dest} is not on your PATH; add it with: export PATH=\"${dest}:\$PATH\"" ;;
  esac
}

main "$@"
