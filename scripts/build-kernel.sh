#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Build the Firecracker guest kernel (Linux 6.18 LTS) from kernel.org.
#
#   scripts/build-kernel.sh [OUT_DIR]
#
# Environment:
#   KERNEL_VERSION  version to build (default 6.18.56)
#   ARCH_NAME       x86_64 (default: uname -m) or arm64 (aarch64)
#   WORK_DIR        download and build directory (default tmp/kernel, gitignored)
#   JOBS            make parallelism (default: nproc)
#
# Output: OUT_DIR/vmlinux (x86_64 ELF, accepted by Firecracker) or
# OUT_DIR/Image (arm64), plus OUT_DIR/kernel.config. No root needed. The tarball
# is verified against the sha256sums.asc published next to it on kernel.org
# (integrity over TLS; the PGP signature is not checked, see README).
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
version=${KERNEL_VERSION:-6.18.56}
major=${version%%.*}
work=${WORK_DIR:-$root/tmp/kernel}
out=${1:-$root/tmp/image}
jobs=${JOBS:-$(nproc)}
arch_name=${ARCH_NAME:-$(uname -m)}

case "$arch_name" in
  x86_64|amd64) karch=x86;   arch_frag=x86_64; target=vmlinux; artifact=vmlinux ;;
  aarch64|arm64) karch=arm64; arch_frag=arm64;  target=Image;   artifact=arch/arm64/boot/Image ;;
  *) echo "unsupported architecture: $arch_name" >&2; exit 2 ;;
esac

mkdir -p "$work" "$out"
work=$(cd "$work" && pwd)
out=$(cd "$out" && pwd)
tarball="linux-$version.tar.xz"
base="https://cdn.kernel.org/pub/linux/kernel/v${major}.x"

if [ ! -f "$work/$tarball" ]; then
  curl -fsSL -o "$work/$tarball.part" "$base/$tarball"
  mv "$work/$tarball.part" "$work/$tarball"
fi
if [ ! -f "$work/sha256sums.asc" ] || ! grep -qF " $tarball" "$work/sha256sums.asc"; then
  curl -fsSL -o "$work/sha256sums.asc" "$base/sha256sums.asc"
fi
want=$(awk -v f="$tarball" '$2 == f {print $1; exit}' "$work/sha256sums.asc")
have=$(sha256sum "$work/$tarball" | awk '{print $1}')
if [ -z "$want" ] || [ "$want" != "$have" ]; then
  echo "checksum mismatch for $tarball (want '$want', have '$have')" >&2
  exit 1
fi

src="$work/linux-$version"
[ -d "$src" ] || tar -C "$work" -xf "$work/$tarball"

cd "$src"
export ARCH=$karch
make tinyconfig >/dev/null
scripts/kconfig/merge_config.sh -m .config \
  "$root/images/kernel/config-6.18" "$root/images/kernel/config-6.18.$arch_frag" >/dev/null
make olddefconfig >/dev/null

# Fail if an option of the fragments was dropped by a missing dependency.
missing=0
while IFS== read -r key val; do
  # Whole-line, fixed-string matches: option names contain no regex, but
  # CONFIG_FOO must never match CONFIG_FOO_BAR=y.
  if grep -qxF "$key=$val" .config; then continue; fi
  if [ "$val" = n ] && ! grep -q "^${key}=" .config; then continue; fi
  echo "kernel option not applied: $key=$val" >&2
  missing=1
done < <(cat "$root/images/kernel/config-6.18" "$root/images/kernel/config-6.18.$arch_frag" | grep -E '^CONFIG_[A-Z0-9_]+=')
[ "$missing" = 0 ] || exit 1

make -j"$jobs" "$target"
cp "$artifact" "$out/$(basename "$artifact")"
cp .config "$out/kernel.config"
echo "kernel: $out/$(basename "$artifact")"
