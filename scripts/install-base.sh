#!/usr/bin/env bash
# Installs a golden image content-addressed and makes it the current base:
#
#   install-base.sh IMAGE [EXPECTED_SHA256]
#
# The image lands at $OUT_DIR/bases/<sha256>/base.ext4 through a temp file and
# a same-filesystem rename, then $OUT_DIR/base.ext4 (a symlink) is atomically
# repointed at it. Every step is fsynced, so a power loss never leaves the
# pointer naming a truncated base. A base is never overwritten in place and
# never deleted, so KVM delta generations built against an older base stay
# restorable and a provision racing the install always clones a complete image.
# A legacy plain base.ext4 is first preserved under bases/<its sha256>/.
#
# An existing bases/<sha256>/ is re-hashed before it is trusted; one whose
# content does not match its name is moved aside to bases/.corrupt-<sha256>-<ts>
# and replaced. IMAGE may be that existing base itself to just make it current
# (fails if it turns out corrupt).
#
# The fingerprint is the sha256 of the image's full logical contents, the same
# value the docklet computes (disk.Fingerprint). Prints the installed fingerprint.
set -euo pipefail

OUT_DIR="${OUT_DIR:-/var/lib/docklet/images}"
SRC="${1:-}"
WANT="${2:-}"

if [ -z "$SRC" ] || [ ! -f "$SRC" ]; then
  echo "usage: install-base.sh IMAGE [EXPECTED_SHA256]" >&2
  exit 2
fi
if [ -n "$WANT" ] && ! printf '%s' "$WANT" | grep -Eq '^[0-9a-f]{64}$'; then
  echo "install-base: expected sha256 must be 64 lowercase hex characters" >&2
  exit 2
fi

BASES="$OUT_DIR/bases"
POINTER="$OUT_DIR/base.ext4"
mkdir -p "$BASES"

# Atomically repoints base.ext4 at bases/<fp>/base.ext4, durably.
point_at() {
  ln -sfn "bases/$1/base.ext4" "$OUT_DIR/.base.ext4.next"
  mv -T "$OUT_DIR/.base.ext4.next" "$POINTER"
  sync "$OUT_DIR"
}

# Succeeds when bases/<fp>/base.ext4 exists and hashes to fp; a mismatching
# directory is moved aside so a fresh copy can take its name.
verified() {
  local fp="$1" got
  [ -f "$BASES/$fp/base.ext4" ] || return 1
  got="$(sha256sum "$BASES/$fp/base.ext4" | cut -d' ' -f1)"
  [ "$got" = "$fp" ] && return 0
  echo "install-base: bases/$fp hashes to $got; moving it aside" >&2
  mv -T "$BASES/$fp" "$BASES/.corrupt-$fp-$(date +%s)"
  sync "$BASES"
  return 1
}

# Preserve a legacy plain base before the pointer replaces it.
if [ -f "$POINTER" ] && [ ! -L "$POINTER" ]; then
  OLD="$(sha256sum "$POINTER" | cut -d' ' -f1)"
  if ! verified "$OLD"; then
    mkdir -p "$BASES/$OLD"
    ln "$POINTER" "$BASES/$OLD/base.ext4"
    sync "$BASES/$OLD" "$BASES"
  fi
  point_at "$OLD"
  rm -f "$POINTER.fingerprint"
  echo "install-base: preserved legacy base as bases/$OLD" >&2
fi

if [ -n "$WANT" ] && [ "$SRC" -ef "$BASES/$WANT/base.ext4" ]; then
  if ! verified "$WANT"; then
    echo "install-base: installed base $WANT is corrupt; reinstall it from a good copy" >&2
    exit 1
  fi
  FP="$WANT"
  echo "install-base: base $FP already installed" >&2
elif [ -n "$WANT" ] && verified "$WANT"; then
  FP="$WANT"
  echo "install-base: base $FP already installed" >&2
else
  STAGE="$(mktemp -p "$BASES" .incoming.XXXXXX)"
  trap 'rm -f "$STAGE"' EXIT
  cp --sparse=always "$SRC" "$STAGE"
  sync "$STAGE"
  FP="$(sha256sum "$STAGE" | cut -d' ' -f1)"
  if [ -n "$WANT" ] && [ "$FP" != "$WANT" ]; then
    echo "install-base: $SRC hashes to $FP, expected $WANT; refusing" >&2
    exit 1
  fi
  if verified "$FP"; then
    echo "install-base: base $FP already installed" >&2
  else
    mkdir -p "$BASES/$FP"
    chmod 0644 "$STAGE"
    mv -T "$STAGE" "$BASES/$FP/base.ext4"
    sync "$BASES/$FP" "$BASES"
  fi
fi

point_at "$FP"
echo "$FP"
