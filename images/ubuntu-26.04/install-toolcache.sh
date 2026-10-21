#!/bin/sh
# SPDX-License-Identifier: MIT
# Installs the toolchains of a lock file (tool version arch sha256 url per
# line) for the image architecture into the @actions/tool-cache layout.
set -eu

lock=${1:?usage: install-toolcache.sh LOCK_FILE}
case "$(dpkg --print-architecture)" in
    amd64) arch=x64 ;;
    arm64) arch=arm64 ;;
    *) echo 'unsupported toolcache architecture' >&2; exit 1 ;;
esac
cache=${RUNNER_TOOL_CACHE:-/opt/hostedtoolcache}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT TERM
installed=0
# The last line is read even without a final newline.
while read -r tool version entry_arch sha url || [ -n "$tool" ]; do
    case "$tool" in ''|'#'*) continue ;; esac
    case "$tool" in go|node) ;; *) echo "unsupported tool: $tool" >&2; exit 1 ;; esac
    case "$entry_arch" in x64|arm64) ;; *) echo "unsupported arch for $tool $version: $entry_arch" >&2; exit 1 ;; esac
    [ "$entry_arch" = "$arch" ] || continue
    case "$url" in
        *.tar.gz) compression=z ;;
        *.tar.xz) compression=J ;;
        *) echo "unsupported archive: $url" >&2; exit 1 ;;
    esac
    curl -fsSL --retry 3 "$url" -o "$tmp/archive"
    printf '%s  %s\n' "$sha" "$tmp/archive" | sha256sum -c -
    dest="$cache/$tool/$version/$arch"
    mkdir -p "$dest"
    tar -x"$compression"f "$tmp/archive" -C "$dest" --strip-components=1 --no-same-owner
    touch "$dest.complete"
    rm "$tmp/archive"
    installed=$((installed + 1))
done < "$lock"
if [ "$installed" -eq 0 ]; then
    echo "no toolchain for $arch in $lock" >&2
    exit 1
fi
chown -R runner:runner "$cache"
