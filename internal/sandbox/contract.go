// SPDX-License-Identifier: MIT

package sandbox

// The host/guest contract shared by the drivers and the guest side
// (internal/guest). Changing a value here changes both sides at once because
// the host always attaches its own executable to the sandbox.
const (
	// VsockPort is the vsock port the executor connects to on the host (CID 2).
	VsockPort = 1024
	// GuestExecDir and GuestExecName locate the runner executable inside the
	// sandbox (read-only), in both modes.
	GuestExecDir  = "/opt/ermite"
	GuestExecName = "ermite-runner"
	// ContainerRunDir is the per-job directory bind-mounted into containers;
	// ContainerSocket is the executor stream socket inside it.
	ContainerRunDir = "/run/ermite"
	ContainerSocket = "exec.sock"
	// GuestExternalsDir is where the host's external tools (Node.js) appear in
	// the sandbox, read-only, in GitHub's layout: <dir>/node24/bin/node.
	GuestExternalsDir = "/home/runner/externals"
	// ExternalUnavailableFile, in <GuestExternalsDir>/<name>, tells why a tool
	// is missing on this runner. A directory that has it has no tool (its
	// bin/node only prints the reason and fails).
	ExternalUnavailableFile = "UNAVAILABLE"
	// GuestScratch is where the per-job scratch disk (VM) or volume (container)
	// is mounted; dockerd keeps its data root there.
	GuestScratch = "/scratch"

	// LabelJob marks every Docker object created for a job; reaping lists by it.
	LabelJob = "dev.bnema.ermite-runner.job"

	// Kernel command line keys of the VM (read from /proc/cmdline by init).
	CmdlineSandbox = "ermite.sandbox" // "vm"
	CmdlineConnect = "ermite.connect" // "vsock:1024"
	CmdlineGateway = "ermite.gw"      // host side of the /30, also the DNS server
	// CmdlineExternals=1: the externals disk is attached as the fourth drive.
	CmdlineExternals = "ermite.externals"
)
