#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Print the id of the guest image (kernel + base disk): the first 12 hex digits
# of the sha256 of the guest inputs as Git tracks them (modes, blob ids and
# paths of images/, scripts/build-kernel.sh and scripts/build-image.sh).
# images/GUEST_ID is not an input: it records the id.
#
#   scripts/guest-id.sh [--allow-dirty]          print the id of the inputs
#   scripts/guest-id.sh --check [--allow-dirty]  print it, fail if images/GUEST_ID differs
#   scripts/guest-id.sh --write [--allow-dirty]  write images/GUEST_ID
#
# A modified or untracked input makes the id meaningless, so it is refused
# unless --allow-dirty is given.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
inputs=(images scripts/build-kernel.sh scripts/build-image.sh ':(exclude)images/GUEST_ID')
mode=print
dirty=0

for arg in "$@"; do
  case "$arg" in
    --check) mode=check ;;
    --write) mode="write" ;;
    --allow-dirty) dirty=1 ;;
    *) echo "usage: guest-id.sh [--check|--write] [--allow-dirty]" >&2; exit 2 ;;
  esac
done

if [ "$dirty" = 0 ]; then
  changed=$(git -C "$root" status --porcelain -- "${inputs[@]}")
  if [ -n "$changed" ]; then
    echo "guest inputs have uncommitted changes (commit them or use --allow-dirty):" >&2
    echo "$changed" >&2
    exit 1
  fi
fi

id=$(git -C "$root" ls-files -s -- "${inputs[@]}" | sha256sum | cut -c1-12)
[ -n "$id" ] || { echo "cannot compute the guest id" >&2; exit 1; }

file=$root/images/GUEST_ID
case "$mode" in
  print) echo "$id" ;;
  write) echo "$id" >"$file"; echo "$id" ;;
  check)
    have=$(head -n1 "$file" 2>/dev/null || true)
    if [ "$have" != "$id" ]; then
      echo "images/GUEST_ID is '${have}' but the guest inputs hash to '${id}': run scripts/guest-id.sh --write and commit it" >&2
      exit 1
    fi
    echo "$id"
    ;;
esac
