// SPDX-License-Identifier: MIT

package guest

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"

	"bnema.dev/ermite-runner/internal/sandbox"
)

// Devices of the micro-VM, in the order the host attaches them (virtio-blk).
const (
	devBase    = "/dev/vdb" // shared read-only base image (ext4)
	devScratch = "/dev/vdc" // per-job scratch disk (ext4)
)

// KernelArg names of the clock and DNS settings (not part of the contract
// constants because only init reads them).
const (
	cmdlineTime = "ermite.time" // host unix time at boot, seconds
)

// setupVM turns the minimal boot root (the exec disk) into the job's root:
// overlayfs of base + scratch, with the exec disk bound read-only at
// /opt/ermite, then pivot_root into it and mount the pseudo filesystems.
func setupVM(log zerowrap.Logger, args map[string]string) error {
	// Pseudo filesystems of the boot root. Their mountpoints exist on the exec
	// disk (the root is read-only).
	for _, m := range []struct{ src, dst, typ string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
	} {
		// proc is mounted by Init (to read the command line) and devtmpfs by the
		// kernel (DEVTMPFS_MOUNT): mount only what is missing.
		if isMounted(m.dst) {
			continue
		}
		if err := mount(m.src, m.dst, m.typ, 0, ""); err != nil {
			return err
		}
	}
	// Firecracker has no RTC on x86: the guest clock starts at the epoch, which
	// breaks TLS. The host passes its time on the command line.
	if v, ok := args[cmdlineTime]; ok {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec > 1_600_000_000 {
			if err := unix.ClockSettime(unix.CLOCK_REALTIME, &unix.Timespec{Sec: sec}); err != nil {
				log.Warn().Err(err).Msg("init: set clock")
			}
		}
	}

	if err := mount(devBase, "/mnt/base", "ext4", unix.MS_RDONLY, ""); err != nil {
		return err
	}
	if err := mount(devScratch, "/mnt/scratch", "ext4", unix.MS_NOATIME, ""); err != nil {
		return err
	}
	for _, d := range []string{"/mnt/scratch/upper", "/mnt/scratch/work", "/mnt/scratch/docker"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	opts := "lowerdir=/mnt/base,upperdir=/mnt/scratch/upper,workdir=/mnt/scratch/work"
	if err := mount("overlay", "/mnt/newroot", "overlay", 0, opts); err != nil {
		return err
	}
	// The new root needs these mountpoints; they live in the (writable) upper.
	for _, d := range []string{"/mnt/newroot/proc", "/mnt/newroot/sys", "/mnt/newroot/dev",
		"/mnt/newroot/run", "/mnt/newroot/tmp", "/mnt/newroot/scratch", "/mnt/newroot/.oldroot",
		"/mnt/newroot" + sandbox.GuestExecDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// Scratch and the exec disk stay reachable after the pivot.
	if err := mount("/mnt/scratch", "/mnt/newroot"+sandbox.GuestScratch, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if err := mount("/", "/mnt/newroot"+sandbox.GuestExecDir, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if err := mount("", "/mnt/newroot"+sandbox.GuestExecDir, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return err
	}
	for _, d := range []string{"/proc", "/sys", "/dev"} {
		if err := mount(d, "/mnt/newroot"+d, "", unix.MS_MOVE, ""); err != nil {
			return err
		}
	}
	if err := unix.Chdir("/mnt/newroot"); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", ".oldroot"); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	_ = unix.Unmount("/.oldroot", unix.MNT_DETACH)

	for _, m := range []struct {
		src, dst, typ string
		flags         uintptr
		data          string
	}{
		{"devpts", "/dev/pts", "devpts", 0, "gid=5,mode=620,ptmxmode=666"},
		{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, ""},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=755"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
	} {
		if err := os.MkdirAll(m.dst, 0o755); err != nil {
			return err
		}
		if err := mount(m.src, m.dst, m.typ, m.flags, m.data); err != nil {
			// devpts and shm failures are not fatal for the stub.
			log.Warn().Err(err).Str("target", m.dst).Msg("init: mount")
		}
	}
	_ = os.Chmod("/tmp", 0o1777)

	gw := args[sandbox.CmdlineGateway]
	if err := writeResolvConf(gw); err != nil {
		return err
	}
	_ = os.WriteFile("/etc/hostname", []byte("ermite-job\n"), 0o644)
	_ = os.WriteFile("/etc/hosts", []byte("127.0.0.1 localhost ermite-job\n"), 0o644)
	_ = unix.Sethostname([]byte("ermite-job"))
	if err := loUp(); err != nil {
		log.Warn().Err(err).Msg("init: loopback")
	}
	// IPv6 is disabled on the kernel command line (ipv6.disable=1); this is a
	// second line of defence when the module is built in but not disabled.
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/disable_ipv6", []byte("1\n"), 0o644)
	return nil
}

// writeResolvConf points the guest at the gateway, where the runner's DNS
// forwarder listens. /etc/resolv.conf may be a dangling symlink in the image
// (systemd-resolved stub): replace it.
func writeResolvConf(gw string) error {
	if gw == "" {
		return fmt.Errorf("kernel command line has no %s", sandbox.CmdlineGateway)
	}
	_ = os.Remove("/etc/resolv.conf")
	return os.WriteFile("/etc/resolv.conf", []byte("nameserver "+gw+"\noptions timeout:2 attempts:2\n"), 0o644)
}

func mount(src, dst, typ string, flags uintptr, data string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil && !os.IsExist(err) {
		// Read-only boot root: the mountpoint must already exist.
		if _, serr := os.Stat(dst); serr != nil {
			return fmt.Errorf("mountpoint %s: %w", dst, err)
		}
	}
	if err := unix.Mount(src, dst, typ, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s (%s): %w", src, dst, strings.TrimSpace(typ), err)
	}
	return nil
}

// loUp brings the loopback interface up (the kernel leaves it down).
func loUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// isMounted reports whether path is a mount point listed in /proc/self/mountinfo.
func isMounted(path string) bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && f[4] == path {
			return true
		}
	}
	return false
}
