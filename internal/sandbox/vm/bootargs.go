// SPDX-License-Identifier: MIT

package vm

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"bnema.dev/ermite-runner/internal/sandbox"
)

// bootArgs builds the guest kernel command line.
//
//   - console=ttyS0 reboot=k panic=1: serial console; a reboot (what init does
//     at the end) or a panic ends the VMM instead of hanging;
//   - ipv6.disable=1: the guest has no IPv6 (the host policy drops it too);
//   - root=/dev/vda ro: the exec disk; init=/ermite-runner -- init: the runner
//     executable is PID 1 and runs its init subcommand;
//   - ip=…: static address, gateway and DNS from the kernel, no DHCP client;
//   - ermite.*: read by init from /proc/cmdline (connect target, gateway, time,
//     and whether the externals disk is attached as the fourth drive).
func bootArgs(guest, gateway netip.Addr, unixSeconds int64, externals bool) string {
	args := []string{
		"console=ttyS0", "reboot=k", "panic=1", "pci=off", "acpi=off", "8250.nr_uarts=1",
		"ipv6.disable=1", "random.trust_cpu=on", "i8042.noaux", "i8042.nomux", "i8042.nopnp", "i8042.dumbkbd",
		"root=/dev/vda", "ro", "rootfstype=ext4",
		"init=/" + sandbox.GuestExecName,
		fmt.Sprintf("ip=%s::%s:255.255.255.252:ermite-job:eth0:off:%s", guest, gateway, gateway),
		sandbox.CmdlineSandbox + "=" + string(sandbox.VM),
		sandbox.CmdlineConnect + "=vsock:" + strconv.Itoa(sandbox.VsockPort),
		sandbox.CmdlineGateway + "=" + gateway.String(),
		"ermite.time=" + strconv.FormatInt(unixSeconds, 10),
	}
	if externals {
		args = append(args, sandbox.CmdlineExternals+"=1")
	}
	args = append(args, "--", "init")
	return strings.Join(args, " ")
}

// macFor derives a locally administered MAC from the slot.
func macFor(slot int) string {
	return fmt.Sprintf("06:00:ac:10:%02x:%02x", (slot>>8)&0xff, slot&0xff)
}
