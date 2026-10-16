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

# valid_version <tag>: true only for v<semver> with an optional pre-release, the form release tags use.
valid_version() {
  [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$ ]]
}

# semver_cmp <a> <b>: prints -1, 0 or 1 (a<b, a=b, a>b) by semver precedence; returns 2 if either is not a valid v<semver>.
semver_cmp() {
  valid_version "$1" && valid_version "$2" || return 2
  # SemVer orders alphanumeric identifiers in ASCII, not by the host locale's collation.
  local LC_ALL=C a="${1#v}" b="${2#v}" ca cb pa pb i x y
  ca="${a%%-*}"; cb="${b%%-*}"
  pa=""; pb=""
  [ "$a" != "$ca" ] && pa="${a#*-}"
  [ "$b" != "$cb" ] && pb="${b#*-}"
  local -a na nb ia ib
  IFS=. read -r -a na <<< "$ca"
  IFS=. read -r -a nb <<< "$cb"
  for i in 0 1 2; do
    x=$((10#${na[i]})); y=$((10#${nb[i]}))
    if [ "$x" -lt "$y" ]; then echo -1; return 0; fi
    if [ "$x" -gt "$y" ]; then echo 1; return 0; fi
  done
  # Equal cores: a release outranks any pre-release of it.
  if [ -z "$pa" ] && [ -z "$pb" ]; then echo 0; return 0; fi
  if [ -z "$pa" ]; then echo 1; return 0; fi
  if [ -z "$pb" ]; then echo -1; return 0; fi
  IFS=. read -r -a ia <<< "$pa"
  IFS=. read -r -a ib <<< "$pb"
  for ((i = 0; i < ${#ia[@]} && i < ${#ib[@]}; i++)); do
    x="${ia[i]}"; y="${ib[i]}"
    [ "$x" = "$y" ] && continue
    if [[ "$x" =~ ^[0-9]+$ ]] && [[ "$y" =~ ^[0-9]+$ ]]; then
      if [ $((10#$x)) -lt $((10#$y)) ]; then echo -1; else echo 1; fi
    elif [[ "$x" =~ ^[0-9]+$ ]]; then echo -1
    elif [[ "$y" =~ ^[0-9]+$ ]]; then echo 1
    elif [[ "$x" < "$y" ]]; then echo -1
    else echo 1
    fi
    return 0
  done
  # A longer pre-release set outranks its prefix.
  if [ "${#ia[@]}" -lt "${#ib[@]}" ]; then echo -1
  elif [ "${#ia[@]}" -gt "${#ib[@]}" ]; then echo 1
  else echo 0
  fi
}

# release_install_decision <installed> <wanted> <allow_downgrade>: prints install, same or newer.
# An empty or unparsable installed version (dev build, no binary) always installs, as does allow_downgrade=1.
release_install_decision() {
  local installed="$1" wanted="$2" allow="$3" c
  if [ "$allow" = "1" ] || ! c="$(semver_cmp "$installed" "$wanted")"; then echo install; return 0; fi
  case "$c" in
    0) echo same ;;
    1) echo newer ;;
    *) echo install ;;
  esac
}

# installed_release_version <bin>: prints its --version line, or nothing when absent or unreadable; never fails.
installed_release_version() {
  [ -x "$1" ] || return 0
  timeout 10 "$1" --version 2>/dev/null | head -n1 || true
}

# install_release_docklet <dest> <version> <allow_downgrade> <workdir>: installs the verified release unless dest is the same or newer.
install_release_docklet() {
  local dest="$1" version="$2" allow="$3" dir="$4" arch asset installed
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  installed="$(installed_release_version "$dest")"
  case "$(release_install_decision "$installed" "$version" "$allow")" in
    same)
      log "docklet ${installed} is already installed; skipping the download (DOCKLET_ALLOW_DOWNGRADE=1 to reinstall)"
      return 0 ;;
    newer)
      log "installed docklet ${installed} is newer than DOCKLET_VERSION ${version}; keeping it (DOCKLET_ALLOW_DOWNGRADE=1 to downgrade)"
      return 0 ;;
  esac
  asset="docklet-linux-${arch}"
  log "downloading and verifying docklet ${version} (${asset})"
  fetch_release "$version" "$dir" "$asset"
  install -D -m 0755 "${dir}/${asset}" "$dest"
  log "installed ${dest} (${version}, signature verified)"
}

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
  valid_version "$version" || die "invalid release version '${version}' for ${file}; expected v<major>.<minor>.<patch>"
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
  local dir="$1" arch want url have out
  # Capture first so grep -q cannot SIGPIPE cosign under pipefail; compare the field exactly, not as a regexp.
  if have="$(command -v cosign 2>/dev/null)" && [ -n "$have" ] \
    && out="$("$have" version 2>/dev/null)" \
    && awk -v want="$COSIGN_VERSION" '$1 == "GitVersion:" && $2 == want { found = 1 } END { exit !found }' <<< "$out"; then
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
  valid_version "$version" || die "invalid DOCKLET_VERSION '${version}'; expected v<major>.<minor>.<patch>"
  base="${DOCKLET_RELEASE_BASE_URL:-https://github.com/usedock/docklet/releases/download}/${version}"
  for f in "$asset" checksums.txt checksums.txt.bundle "${asset}.bundle"; do
    curl -fsSL -o "${dir}/${f}" "${base}/${f}" || die "could not download ${f} from ${base}"
  done
  cosign="$(ensure_cosign "$dir")"
  verify_signature "$cosign" "${dir}/checksums.txt" "${dir}/checksums.txt.bundle" "$version"
  verify_signature "$cosign" "${dir}/${asset}" "${dir}/${asset}.bundle" "$version"
  verify_checksum "${dir}/${asset}" "${dir}/checksums.txt" "$asset"
}
