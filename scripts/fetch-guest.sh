#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Download a published guest image (kernel + base disk) into OUT_DIR, the
# layout the container build and the runner expect:
#
#   scripts/fetch-guest.sh <guest-id> <out_dir>
#   scripts/fetch-guest.sh "$(cat images/GUEST_ID)" tmp/image
#
# The guest is the release guest-<id> of ermite-runner (see
# scripts/publish-guest.sh). Every file is checked against the release's
# SHA256SUMS; base.ext4 is unpacked sparse. OUT_DIR/GUEST_ID is written last,
# and a copy of that id that still matches its sums is not downloaded again.
#
# Environment:
#   ERMITE_RELEASES_URL  forge base URL (default $GITHUB_SERVER_URL, then https://git.bnema.dev)
#   GITHUB_TOKEN         sent as a bearer token to $GITHUB_SERVER_URL only (CI job token)
#
# Needs curl, sha256sum and zstd. No process substitution: it must work in a
# sandbox without /dev/fd.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: fetch-guest.sh <guest-id> <out_dir>" >&2; exit 2; }
id=$1
out=$2
case "$id" in
  '' | *[!0-9a-f]*) echo "invalid guest id '$id' (expected lowercase hex)" >&2; exit 2 ;;
esac

for tool in curl sha256sum zstd; do
  command -v "$tool" >/dev/null || { echo "need $tool" >&2; exit 1; }
done

base=${ERMITE_RELEASES_URL:-${GITHUB_SERVER_URL:-https://git.bnema.dev}}
base=${base%/}
url=$base/ermite-runner/releases/download/guest-$id

mkdir -p "$out"
out=$(cd "$out" && pwd)

# verified reports whether OUT_DIR already holds guest $id, bytes included.
verified() {
  [ "$(cat "$out/GUEST_ID" 2>/dev/null || true)" = "$id" ] || return 1
  [ -f "$out/vmlinux" ] && [ -f "$out/base.ext4" ] && [ -f "$out/SHA256SUMS" ] || return 1
  local list=$out/.verify.sums
  local rc=0
  if ! grep -E '  (vmlinux|base\.ext4)$' "$out/SHA256SUMS" >"$list" || [ "$(wc -l <"$list")" -ne 2 ]; then
    rc=1
  elif ! (cd "$out" && sha256sum --quiet -c "$list" >/dev/null 2>&1); then
    rc=1
  fi
  rm -f "$list"
  return $rc
}

if verified; then
  echo "guest $id already present in $out"
  exit 0
fi

# From here the directory is not a verified guest until GUEST_ID is rewritten.
rm -f "$out/GUEST_ID"

get() {
  local name=$1 code
  if [ -n "${GITHUB_TOKEN:-}" ] && [ -n "${GITHUB_SERVER_URL:-}" ] && [ "${url#"${GITHUB_SERVER_URL%/}"/}" != "$url" ]; then
    # The header goes through stdin so the token never shows in the process list.
    code=$(printf 'header = "Authorization: Bearer %s"\n' "$GITHUB_TOKEN" |
      curl -sSL --retry 3 --retry-delay 2 -K - -o "$out/$name" -w '%{http_code}' "$url/$name") || code=000
  else
    code=$(curl -sSL --retry 3 --retry-delay 2 -o "$out/$name" -w '%{http_code}' "$url/$name") || code=000
  fi
  if [ "$code" = 404 ]; then
    rm -f "$out/$name"
    echo "guest $id is not published: wait for the Guest workflow or run scripts/publish-guest.sh ($url/$name answered 404)" >&2
    exit 1
  fi
  if [ "$code" != 200 ]; then
    rm -f "$out/$name"
    echo "download of $url/$name failed (HTTP $code)" >&2
    exit 1
  fi
}

echo "fetching guest $id from $url"
for name in SHA256SUMS vmlinux base.ext4.zst kernel.config; do
  get "$name"
done

# Check the downloaded files first: SHA256SUMS also lists the unpacked base.ext4.
want=$out/.download.sums
for name in vmlinux base.ext4.zst kernel.config; do
  grep -E "  $name\$" "$out/SHA256SUMS" || { echo "SHA256SUMS has no entry for $name" >&2; exit 1; }
done >"$want"
(cd "$out" && sha256sum --quiet -c "$want") || { echo "checksum mismatch in the downloaded guest $id" >&2; exit 1; }
rm -f "$want"
chmod 0755 "$out/vmlinux"

# --sparse keeps the holes of the 3 GiB image: it is written as a sparse file.
zstd -d -q -f --sparse -o "$out/base.ext4" "$out/base.ext4.zst"
rm -f "$out/base.ext4.zst"
want=$out/.unpacked.sums
grep -E '  base\.ext4$' "$out/SHA256SUMS" >"$want" || { echo "SHA256SUMS has no entry for base.ext4" >&2; exit 1; }
(cd "$out" && sha256sum --quiet -c "$want") || { echo "checksum mismatch in the unpacked base.ext4 of guest $id" >&2; exit 1; }
rm -f "$want"

echo "$id" >"$out/GUEST_ID"
echo "guest $id ready in $out"
