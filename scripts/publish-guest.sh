#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Build the guest image (kernel + base disk) if needed and publish it as the
# prerelease guest-<id> of ermite-runner. Used by the Guest workflow and from a
# workstation (the fallback when CI cannot build it).
#
#   scripts/publish-guest.sh [OUT_DIR]       OUT_DIR defaults to tmp/image
#
# The id comes from scripts/guest-id.sh (--check: images/GUEST_ID must be
# current). Release assets: vmlinux, base.ext4.zst, kernel.config and
# SHA256SUMS (vmlinux, base.ext4, base.ext4.zst, kernel.config). Consumers
# download them with scripts/fetch-guest.sh.
#
# OUT_DIR/GUEST_ID records the id of the build in OUT_DIR. A build with the same
# id is reused; any other content is rebuilt.
#
# Environment:
#   ASSUME_BUILT=1  adopt the existing vmlinux, base.ext4 and kernel.config in
#                   OUT_DIR as the build of the current id, without building.
#                   For the first publication of a build made before OUT_DIR/GUEST_ID
#                   existed; only use it when that build is from the current inputs.
#   FORCE=1         delete the existing release and publish it again
#   DRY_RUN=1       build and package, print the ermite commands instead of running them
#   TARGET          commit or branch the tag is created on when the release is
#                   new (default: HEAD, which the forge must already have)
#   ERMITE_URL, ERMITE_TOKEN  forge and credentials, read by the ermite CLI
#   REPO            forge repository (default ermite-runner)
#   JOBS            kernel build parallelism (see scripts/build-kernel.sh)
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-$root/tmp/image}
repo=${REPO:-ermite-runner}
dry=${DRY_RUN:-0}

mkdir -p "$out"
out=$(cd "$out" && pwd)

id=$("$root/scripts/guest-id.sh" --check)
tag=guest-$id
target=${TARGET:-$(git -C "$root" rev-parse HEAD)}
echo "guest $id ($tag)"

ermite() {
  if [ "$dry" = 1 ]; then
    echo "DRY_RUN: ermite $*"
  else
    command ermite "$@"
  fi
}

if [ "$dry" != 1 ]; then
  command -v ermite >/dev/null || { echo "need the ermite CLI" >&2; exit 1; }
fi
command -v zstd >/dev/null || { echo "need zstd" >&2; exit 1; }
command -v jq >/dev/null || [ "$dry" = 1 ] || { echo "need jq" >&2; exit 1; }

# Decide first whether there is anything to do, before a long build.
exists=0
complete=0
if [ "$dry" != 1 ]; then
  if view=$(command ermite release view "$repo" "$tag" --json 2>&1); then
    exists=1
    if echo "$view" | jq -e '[.assets[].name] | (index("vmlinux") and index("base.ext4.zst") and index("kernel.config") and index("SHA256SUMS"))' >/dev/null; then
      complete=1
    fi
  elif ! echo "$view" | grep -qi 'not found'; then
    echo "cannot look up release $tag: $view" >&2
    exit 1
  fi
  if [ "$complete" = 1 ] && [ "${FORCE:-0}" != 1 ]; then
    echo "release $tag already has all its assets (FORCE=1 to publish it again)"
    exit 0
  fi
fi

have_build() {
  [ -f "$out/vmlinux" ] && [ -f "$out/base.ext4" ] && [ -f "$out/kernel.config" ]
}

if have_build && [ "$(cat "$out/GUEST_ID" 2>/dev/null || true)" = "$id" ]; then
  echo "reusing the build of guest $id in $out"
elif have_build && [ "${ASSUME_BUILT:-0}" = 1 ]; then
  echo "adopting the existing build in $out as guest $id (ASSUME_BUILT=1)"
  echo "$id" >"$out/GUEST_ID"
else
  rm -f "$out/GUEST_ID"
  "$root/scripts/build-kernel.sh" "$out"
  "$root/scripts/build-image.sh" "$out"
  echo "$id" >"$out/GUEST_ID"
fi

# Package: the compressed base is what is uploaded; SHA256SUMS also covers the
# raw files so a consumer verifies the unpacked image.
(
  cd "$out"
  zstd -q -f -T0 -3 base.ext4 -o base.ext4.zst
  sha256sum vmlinux base.ext4 base.ext4.zst kernel.config >SHA256SUMS
)
ls -l "$out/vmlinux" "$out/base.ext4.zst" "$out/kernel.config" "$out/SHA256SUMS"
cat "$out/SHA256SUMS"

assets=(vmlinux base.ext4.zst kernel.config SHA256SUMS)
files=()
for a in "${assets[@]}"; do files+=("$out/$a"); done

if [ "$exists" = 1 ] && [ "${FORCE:-0}" = 1 ]; then
  ermite release delete "$repo" "$tag" --yes
  exists=0
fi

if [ "$exists" = 1 ]; then
  # An interrupted publication left some assets: replace them all.
  ermite release upload "$repo" "$tag" "${files[@]}" --clobber
else
  args=()
  for f in "${files[@]}"; do args+=(--asset "$f"); done
  ermite release create "$repo" "$tag" --target "$target" --prerelease \
    --title "guest $id" \
    --notes "Guest image $id: the Firecracker kernel and the Ubuntu base disk, built from the guest inputs at $id. Download with scripts/fetch-guest.sh $id <dir>. base.ext4.zst unpacks to the sparse base.ext4 listed in SHA256SUMS." \
    "${args[@]}"
fi
echo "published $tag"
