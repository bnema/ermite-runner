#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Build the ermite-runner-ubuntu-26.04 image as an OCI image (container
# isolation) and as a sparse ext4 base image (vm isolation). No root needed:
# the OCI build uses docker or podman (rootless is fine), the ext4 image is
# written by `mkfs.ext4 -d` from the exported rootfs tarball.
#
#   scripts/build-image.sh [OUT_DIR]
#
# Environment:
#   ENGINE      docker or podman (default: first found)
#   IMAGE_TAG   OCI tag (default ermite-runner-ubuntu-26.04:latest, the
#               default of ERMITE_RUNNER_IMAGE in container mode)
#   EXT4_SLACK  free space added to the base image in MiB (default 512)
#
# Output: OUT_DIR/base.ext4 (read-only shared base disk; its UUID and size are
# stable per build) and the OCI image IMAGE_TAG in the engine's store.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-$root/tmp/image}
tag=${IMAGE_TAG:-ermite-runner-ubuntu-26.04:latest}
slack=${EXT4_SLACK:-512}
engine=${ENGINE:-}

if [ -z "$engine" ]; then
  for e in docker podman; do
    if command -v "$e" >/dev/null 2>&1; then engine=$e; break; fi
  done
fi
[ -n "$engine" ] || { echo "need docker or podman" >&2; exit 1; }
command -v mkfs.ext4 >/dev/null || { echo "need mkfs.ext4 (e2fsprogs >= 1.47.1 for tarball input)" >&2; exit 1; }

# mkfs.ext4 -d with a tarball needs e2fsprogs 1.47.1; older releases only take a
# directory. mke2fs prints its version on stderr and may exit non-zero on -V.
ver=$(mkfs.ext4 -V 2>&1 | sed -n 's/^mke2fs \([0-9]*\.[0-9]*\.[0-9]*\).*/\1/p' | head -n1)
[ -n "$ver" ] || ver=$(mkfs.ext4 -V 2>&1 | sed -n 's/^mke2fs \([0-9]*\.[0-9]*\).*/\1.0/p' | head -n1)
if [ -z "$ver" ] || [ "$(printf '%s\n1.47.1\n' "$ver" | sort -V | head -n1)" != "1.47.1" ]; then
  echo "mkfs.ext4 is e2fsprogs ${ver:-of unknown version}, need >= 1.47.1 (mkfs.ext4 -d with a tarball)" >&2
  exit 1
fi

mkdir -p "$out"
out=$(cd "$out" && pwd)
"$engine" build -t "$tag" "$root/images/ubuntu-26.04"

cid=$("$engine" create "$tag")
tmp=$(mktemp -d)
cleanup() { "$engine" rm -f "$cid" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

"$engine" export "$cid" > "$tmp/rootfs.tar"
bytes=$(stat -c %s "$tmp/rootfs.tar")
# ext4 metadata overhead is about 5 %; the slack leaves room for the guest's
# own writes into the (read-only mounted) base only when someone remounts it.
mib=$(( bytes / 1024 / 1024 * 110 / 100 + slack ))

img="$out/base.ext4.part"
rm -f "$img"
truncate -s "${mib}M" "$img"
# No journal: the base is mounted read-only and shared between VMs.
mkfs.ext4 -q -F -L ermite-base -O ^has_journal -E lazy_itable_init=1,nodiscard \
  -d "$tmp/rootfs.tar" "$img"
mv "$img" "$out/base.ext4"
echo "base image: $out/base.ext4 (${mib} MiB sparse), OCI image: $tag"
