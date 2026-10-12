#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Run the VM integration tests WITHOUT root: inside a user + network namespace
# the process is "root" with CAP_NET_ADMIN over a private network stack, which is
# enough for TAP devices, nftables and the DNS forwarder on the TAP address.
# Only /dev/kvm access (group kvm) and the Firecracker binary are needed. The
# jailer needs real root, so it is switched off here (see README: this tests the
# whole driver except the jailer's chroot, uid drop and cgroups).
#
#   scripts/it-vm-userns.sh [go test args]
#
# Environment: FIRECRACKER (default: firecracker in PATH), IMAGE_DIR (directory
# with vmlinux and base.ext4, default tmp/image). Outbound internet from the
# private namespace is not available: the tests do not need it.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
fc=${FIRECRACKER:-$(command -v firecracker || true)}
[ -x "${fc:-}" ] || { echo "firecracker not found: set FIRECRACKER" >&2; exit 1; }
img=${IMAGE_DIR:-$root/tmp/image}
# Two separate namespaces: the VM tests leave TAP devices behind when they fail,
# and the packet-level firewall test must start from a clean network stack.
rc=0
unshare --user --map-root-user --net bash -c '
  ip link set lo up
  # A default route, so that forwarded packets reach the firewall instead of
  # being answered "network unreachable" by the routing table: the drops the
  # network test sees are then the nftables policy.
  ip link add dummy0 type dummy
  ip addr add 192.0.2.1/24 dev dummy0
  ip link set dummy0 up
  ip route add default via 192.0.2.254 dev dummy0
  export GOWORK=off ERMITE_RUNNER_IT_VM=1 \
    ERMITE_RUNNER_ISOLATION=vm ERMITE_RUNNER_IMAGE="$1" \
    ERMITE_RUNNER_FIRECRACKER="$2" ERMITE_RUNNER_JAILER_MODE=off \
    ERMITE_RUNNER_LOCK_FILE="${TMPDIR:-/tmp}/ermite-runner-it-$$.lock"
  shift 2
  exec go test -count=1 -v -timeout 10m ./internal/sandbox/integration "$@"
' _ "$img" "$fc" "$@" || rc=$?
# The packet-level firewall test needs only a namespace, not Firecracker.
unshare --user --map-root-user --net bash -c '
  ip link set lo up
  GOWORK=off ERMITE_RUNNER_IT_NET=1 exec go test -count=1 -v -timeout 10m -run TwoGuests ./internal/netpolicy
' || rc=$?
exit $rc
