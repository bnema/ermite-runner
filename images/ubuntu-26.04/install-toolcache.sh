#!/bin/sh
# SPDX-License-Identifier: MIT
set -eu

case "$(dpkg --print-architecture)" in
    amd64) arch=x64 ;;
    arm64) arch=arm64 ;;
    *) echo 'unsupported toolcache architecture' >&2; exit 1 ;;
esac
cache=${RUNNER_TOOL_CACHE:-/opt/hostedtoolcache}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
while read -r tool version entry_arch sha url; do
    case "$tool" in ''|'#'*) continue ;; esac
    [ "$entry_arch" = "$arch" ] || continue
    case "$tool" in go|node) ;; *) echo "unsupported tool: $tool" >&2; exit 1 ;; esac
    case "$url" in
        *.tar.gz) compression=z ;;
        *.tar.xz) compression=J ;;
        *) echo "unsupported archive: $url" >&2; exit 1 ;;
    esac
    curl -fsSL "$url" -o "$tmp/archive"
    printf '%s  %s\n' "$sha" "$tmp/archive" | sha256sum -c -
    dest="$cache/$tool/$version/$arch"
    mkdir -p "$dest"
    tar -x"$compression"f "$tmp/archive" -C "$dest" --strip-components=1 --no-same-owner
    touch "$dest.complete"
    rm "$tmp/archive"
done < "${1:?usage: install-toolcache.sh LOCK_FILE}"
chown -R runner:runner "$cache"
