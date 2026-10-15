// SPDX-License-Identifier: MIT

package sandbox

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"bnema.dev/ermite-runner/internal/cachesvc"
)

// Environment variables read by LoadConfig. The other ERMITE_RUNNER_* variables
// (URL, NAME, LABELS, ...) belong to the host process, not to the sandboxes.
const (
	EnvIsolation   = "ERMITE_RUNNER_ISOLATION"
	EnvStateDir    = "ERMITE_RUNNER_STATE_DIR"
	EnvCapacity    = "ERMITE_RUNNER_CAPACITY"
	EnvImage       = "ERMITE_RUNNER_IMAGE"
	EnvJobCPUs     = "ERMITE_RUNNER_JOB_CPUS"
	EnvJobMemory   = "ERMITE_RUNNER_JOB_MEMORY"
	EnvJobDisk     = "ERMITE_RUNNER_JOB_DISK"
	EnvUIDBase     = "ERMITE_RUNNER_UID_BASE"
	EnvNetwork     = "ERMITE_RUNNER_NETWORK"
	EnvFirecracker = "ERMITE_RUNNER_FIRECRACKER"
	EnvJailer      = "ERMITE_RUNNER_JAILER"
	EnvJailerMode  = "ERMITE_RUNNER_JAILER_MODE"
	EnvKernel      = "ERMITE_RUNNER_KERNEL"

	EnvJobPids         = "ERMITE_RUNNER_JOB_PIDS"
	EnvDenyPrivileged  = "ERMITE_RUNNER_DENY_PRIVILEGED"
	EnvLockFile        = "ERMITE_RUNNER_LOCK_FILE"
	EnvJobDiskBW       = "ERMITE_RUNNER_JOB_DISK_BANDWIDTH"
	EnvJobDiskIOPS     = "ERMITE_RUNNER_JOB_DISK_IOPS"
	EnvJobNetBandwidth = "ERMITE_RUNNER_JOB_NET_BANDWIDTH"
	// EnvNode=0 turns the Node.js externals off (nothing is downloaded or
	// attached).
	EnvNode = "ERMITE_RUNNER_NODE"
)

// Defaults.
const (
	DefaultStateDir  = "/var/lib/ermite-runner"
	DefaultOCIImage  = "ermite-runner-ubuntu-24.04:latest"
	DefaultUIDBase   = 900000
	DefaultNetwork   = "10.200.0.0/16"
	DefaultCPUs      = 2
	DefaultMemoryMiB = 4096
	DefaultDiskMiB   = 64 * 1024
	DefaultJobPids   = 32768
	// DefaultLockFile is the single-instance lock of the host: reaping removes
	// everything the runner labels as its own, so two runners (even with
	// different state directories, or one in a container) must never coexist.
	DefaultLockFile = "/run/ermite-runner.lock"
	// Rate limits of a micro-VM, generous so that they only stop a runaway job
	// from starving the host: 400 MiB/s and 20000 IOPS per disk, 1 Gbit/s per
	// network direction. Zero disables a limit.
	DefaultDiskBandwidthMiB = 400
	DefaultDiskIOPS         = 20000
	DefaultNetMbit          = 1000
)

// Config is the sandbox part of the runner configuration.
type Config struct {
	Isolation Mode
	StateDir  string
	Capacity  int

	// Image is ERMITE_RUNNER_IMAGE: a directory (VM: holds the kernel and
	// base.ext4) or an OCI reference (container). A value that is empty or does
	// not fit the selected mode falls back to the default for that mode.
	Image string

	JobCPUs      int
	JobMemoryMiB int
	JobDiskMiB   int

	// UIDBase+slot is the uid and gid the jailer drops to.
	UIDBase int
	// Network is the pool of /30 subnets, one per slot (VM TAPs, container
	// bridges).
	Network netip.Prefix

	// Firecracker and Jailer are binary paths; empty means look in PATH.
	Firecracker string
	Jailer      string
	// Kernel overrides <image dir>/vmlinux (Image on arm64).
	Kernel string
	// JailerOff runs Firecracker without the jailer (development and tests as
	// an unprivileged user). Never use it in production: it only drops the
	// chroot, uid and namespace confinement of the VMM process.
	JailerOff bool

	// KVMPath is the KVM device; tests point it elsewhere to hide /dev/kvm.
	KVMPath string
	// Executable is attached read-only to every sandbox. Empty means
	// os.Executable(). It must be a static binary (CGO_ENABLED=0).
	Executable string
	// DNSPort is the port of the DNS forwarder; 53 in production. Tests that
	// cannot bind it use another port with no guest.
	DNSPort int

	// JobPids is the process limit of a job (container PidsLimit, VM pids.max
	// is derived from the vCPU count and is not affected).
	JobPids int
	// DenyPrivileged refuses the container mode when it would have to run
	// privileged (no sysbox).
	DenyPrivileged bool
	// LockFile is the host-wide single-instance lock.
	LockFile string
	// GuestForgeEnabled is ERMITE_RUNNER_GUEST_FORGE=1. The endpoint itself
	// comes from the server URL, which is not a sandbox setting: the caller
	// sets GuestForge (ResolveGuestForge) before building the drivers.
	GuestForgeEnabled bool
	// GuestForge, when not nil, is the one host endpoint that every sandbox may
	// reach, and the name the DNS forwarder answers with the sandbox's gateway.
	GuestForge *GuestForge
	// Cache, when not nil, is the runner's cache service (package cachesvc),
	// served to every sandbox on a channel of its own (ServeCache), never on a
	// network. The caller makes it before building the drivers, like GuestForge.
	Cache *cachesvc.Service
	// Rate limits of micro-VMs; zero disables.
	DiskBandwidthMiB, DiskIOPS, NetMbit int

	// NodeExternals is false when ERMITE_RUNNER_NODE=0: the caller then sets no
	// Externals.
	NodeExternals bool
	// Externals, when not nil, is the host's set of external tools (Node.js),
	// attached read-only to every sandbox at GuestExternalsDir. The caller sets
	// it before building the drivers, like GuestForge.
	Externals ExternalsSource
}

// LoadConfig reads the sandbox settings from getenv (os.Getenv in production).
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Isolation:    Auto,
		StateDir:     DefaultStateDir,
		Capacity:     1,
		JobCPUs:      DefaultCPUs,
		JobMemoryMiB: DefaultMemoryMiB,
		JobDiskMiB:   DefaultDiskMiB,
		UIDBase:      DefaultUIDBase,
		KVMPath:      "/dev/kvm",
		DNSPort:      53,
		JobPids:      DefaultJobPids,
		LockFile:     DefaultLockFile,

		NodeExternals: true,

		DiskBandwidthMiB: DefaultDiskBandwidthMiB,
		DiskIOPS:         DefaultDiskIOPS,
		NetMbit:          DefaultNetMbit,
	}
	var err error
	if v := getenv(EnvIsolation); v != "" {
		switch m := Mode(strings.ToLower(strings.TrimSpace(v))); m {
		case Auto, VM, Container:
			c.Isolation = m
		default:
			return c, fmt.Errorf("%s: %q is not auto, vm or container", EnvIsolation, v)
		}
	}
	if v := getenv(EnvStateDir); v != "" {
		c.StateDir = v
	}
	if !filepath.IsAbs(c.StateDir) {
		return c, fmt.Errorf("%s: %q must be an absolute path", EnvStateDir, c.StateDir)
	}
	if c.Capacity, err = intEnv(getenv, EnvCapacity, c.Capacity, 1, 1024); err != nil {
		return c, err
	}
	c.Image = getenv(EnvImage)
	if c.JobCPUs, err = intEnv(getenv, EnvJobCPUs, c.JobCPUs, 1, 128); err != nil {
		return c, err
	}
	if v := getenv(EnvJobMemory); v != "" {
		if c.JobMemoryMiB, err = ParseSizeMiB(v); err != nil || c.JobMemoryMiB < 256 {
			return c, fmt.Errorf("%s: %q: need a size of at least 256M", EnvJobMemory, v)
		}
	}
	if v := getenv(EnvJobDisk); v != "" {
		if c.JobDiskMiB, err = ParseSizeMiB(v); err != nil || c.JobDiskMiB < 1024 {
			return c, fmt.Errorf("%s: %q: need a size of at least 1G", EnvJobDisk, v)
		}
	}
	if c.UIDBase, err = intEnv(getenv, EnvUIDBase, c.UIDBase, 1000, 4_000_000_000); err != nil {
		return c, err
	}
	if c.UIDBase+c.Capacity > 4_294_967_294 {
		return c, fmt.Errorf("%s: %d + capacity overflows the uid range", EnvUIDBase, c.UIDBase)
	}
	net := DefaultNetwork
	if v := getenv(EnvNetwork); v != "" {
		net = v
	}
	if c.Network, err = netip.ParsePrefix(net); err != nil || !c.Network.Addr().Is4() || c.Network.Bits() > 30 {
		return c, fmt.Errorf("%s: %q is not an IPv4 prefix of at most /30", EnvNetwork, net)
	}
	if 1<<(30-c.Network.Bits()) < c.Capacity {
		return c, fmt.Errorf("%s: %s has fewer /30 subnets than the capacity %d", EnvNetwork, c.Network, c.Capacity)
	}
	if c.JobPids, err = intEnv(getenv, EnvJobPids, c.JobPids, 64, 4_194_304); err != nil {
		return c, err
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvDenyPrivileged))) {
	case "", "0", "false", "no":
	case "1", "true", "yes":
		c.DenyPrivileged = true
	default:
		return c, fmt.Errorf("%s: %q is not 0 or 1", EnvDenyPrivileged, getenv(EnvDenyPrivileged))
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvGuestForge))) {
	case "", "0", "false", "no":
	case "1", "true", "yes":
		c.GuestForgeEnabled = true
	default:
		return c, fmt.Errorf("%s: %q is not 0 or 1", EnvGuestForge, getenv(EnvGuestForge))
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvNode))) {
	case "", "1", "true", "yes":
	case "0", "false", "no":
		c.NodeExternals = false
	default:
		return c, fmt.Errorf("%s: %q is not 0 or 1", EnvNode, getenv(EnvNode))
	}
	if v := getenv(EnvLockFile); v != "" {
		if !filepath.IsAbs(v) {
			return c, fmt.Errorf("%s: %q must be an absolute path", EnvLockFile, v)
		}
		c.LockFile = v
	}
	if c.DiskBandwidthMiB, err = intEnv(getenv, EnvJobDiskBW, c.DiskBandwidthMiB, 0, 1_000_000); err != nil {
		return c, err
	}
	if c.DiskIOPS, err = intEnv(getenv, EnvJobDiskIOPS, c.DiskIOPS, 0, 100_000_000); err != nil {
		return c, err
	}
	if c.NetMbit, err = intEnv(getenv, EnvJobNetBandwidth, c.NetMbit, 0, 1_000_000); err != nil {
		return c, err
	}
	c.Firecracker = getenv(EnvFirecracker)
	c.Jailer = getenv(EnvJailer)
	c.Kernel = getenv(EnvKernel)
	switch strings.ToLower(getenv(EnvJailerMode)) {
	case "", "on":
	case "off":
		c.JailerOff = true
	default:
		return c, fmt.Errorf("%s: %q is not on or off", EnvJailerMode, getenv(EnvJailerMode))
	}
	return c, nil
}

func intEnv(getenv func(string) string, key string, def, lo, hi int) (int, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s: %q is not an integer in [%d, %d]", key, v, lo, hi)
	}
	return n, nil
}

// ParseSizeMiB parses "4096", "512M", "4G", "64GiB", "1T" into MiB. A bare
// number is MiB. Suffixes are binary (1G = 1024 MiB) and case-insensitive.
func ParseSizeMiB(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "b"), "i")
	mult := 1
	if s != "" {
		switch s[len(s)-1] {
		case 'k':
			mult, s = 0, s[:len(s)-1]
		case 'm':
			mult, s = 1, s[:len(s)-1]
		case 'g':
			mult, s = 1024, s[:len(s)-1]
		case 't':
			mult, s = 1024*1024, s[:len(s)-1]
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || mult == 0 {
		return 0, errors.New("invalid size")
	}
	if n > (1<<31-1)/mult {
		return 0, errors.New("size too large")
	}
	return n * mult, nil
}

// ImageDir returns the VM image directory: Image when it is a directory path,
// else <StateDir>/image.
func (c Config) ImageDir() string {
	if c.Image != "" && looksLikePath(c.Image) {
		return c.Image
	}
	return filepath.Join(c.StateDir, "image")
}

// OCIImage returns the container image reference: Image when it is not a
// directory path, else DefaultOCIImage.
func (c Config) OCIImage() string {
	if c.Image != "" && !looksLikePath(c.Image) {
		return c.Image
	}
	return DefaultOCIImage
}

func looksLikePath(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// KernelPath returns the guest kernel: Kernel, else <ImageDir>/vmlinux (x86-64)
// or <ImageDir>/Image (arm64).
func (c Config) KernelPath() string {
	if c.Kernel != "" {
		return c.Kernel
	}
	name := "vmlinux"
	if runtime.GOARCH == "arm64" {
		name = "Image"
	}
	return filepath.Join(c.ImageDir(), name)
}

// BaseImagePath returns the shared read-only base disk.
func (c Config) BaseImagePath() string { return filepath.Join(c.ImageDir(), "base.ext4") }

// ExecutablePath returns the binary attached to sandboxes.
func (c Config) ExecutablePath() (string, error) {
	if c.Executable != "" {
		return c.Executable, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("sandbox: locate own executable: %w", err)
	}
	return filepath.EvalSymlinks(p)
}

// Resolve fills the zero resource fields of a JobSpec from the config.
func (c Config) Resolve(s JobSpec) JobSpec {
	if s.CPUs <= 0 {
		s.CPUs = c.JobCPUs
	}
	if s.MemoryMiB <= 0 {
		s.MemoryMiB = c.JobMemoryMiB
	}
	if s.DiskMiB <= 0 {
		s.DiskMiB = c.JobDiskMiB
	}
	return s
}
