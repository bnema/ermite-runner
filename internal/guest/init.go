// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"

	"bnema.dev/ermite-runner/internal/sandbox"
)

// InitConfig configures Init. The zero value reads the real system; tests
// replace the fields.
type InitConfig struct {
	Version string
	// Cmdline is the kernel command line; empty reads /proc/cmdline.
	Cmdline string
	// Getpid reports the process id; Init only acts as a VM's PID 1 when it is 1.
	Getpid func() int
	// Signals delivers the termination signals to forward to the executor;
	// nil means "subscribe to SIGTERM and SIGINT". Tests inject their own.
	Signals <-chan os.Signal
	// SkipDocker does not start dockerd (tests, images without Docker).
	SkipDocker bool
	// ExecArgs is the executor command line; default
	// [GuestExecDir/GuestExecName exec].
	ExecArgs []string
}

// Init is `ermite-runner init`: the entrypoint of every sandbox.
//
// In a micro-VM it is the kernel's init (PID 1, root = the read-only exec disk,
// /dev/vdb = shared base image, /dev/vdc = per-job scratch disk): it mounts the
// pseudo filesystems, sets the clock and the DNS server, builds an overlayfs
// root (lower = base, upper = scratch) and pivots into it, then does what a
// container does.
//
// In a container the root and the pseudo filesystems are ready: it skips all of
// that and only prepares cgroups. Then, in both: start dockerd with its data
// root on the scratch space, wait for its socket, run the executor
// (`ermite-runner exec`) as a child, reap orphans, stop dockerd, and finish by
// powering the VM off, or exiting with the executor's code in a container.
func Init(ctx context.Context, cfg InitConfig) int {
	log := zerowrap.FromCtx(ctx)
	if cfg.Getpid == nil {
		cfg.Getpid = os.Getpid
	}
	if cfg.ExecArgs == nil {
		cfg.ExecArgs = []string{sandbox.GuestExecDir + "/" + sandbox.GuestExecName, "exec"}
	}
	cmdline := cfg.Cmdline
	if cmdline == "" {
		b, err := os.ReadFile("/proc/cmdline")
		if err != nil && cfg.Getpid() == 1 {
			// A VM's init starts with nothing mounted: /proc is needed to read
			// the kernel command line that says what we are. setupVM reuses it.
			if merr := unix.Mount("proc", "/proc", "proc", 0, ""); merr == nil {
				b, err = os.ReadFile("/proc/cmdline")
			}
		}
		if err == nil {
			cmdline = string(b)
		}
	}
	args := ParseCmdline(cmdline)
	inVM := args[sandbox.CmdlineSandbox] == string(sandbox.VM) && cfg.Getpid() == 1

	mode := string(sandbox.Container)
	if inVM {
		mode = string(sandbox.VM)
		if err := setupVM(log, args); err != nil {
			log.Error().Err(err).Msg("init: vm setup failed")
			powerOff()
			return 1
		}
	}
	code := runPayload(ctx, cfg, mode, args)
	if inVM {
		powerOff()
	}
	return code
}

// defaultPath is the PATH of a sandbox whose init was started by the kernel
// with an empty environment.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func runPayload(ctx context.Context, cfg InitConfig, mode string, args map[string]string) int {
	log := zerowrap.FromCtx(ctx)
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPath)
	}
	if os.Getenv("HOME") == "" {
		_ = os.Setenv("HOME", "/root")
	}
	if mode == string(sandbox.Container) {
		if err := nestCgroups(); err != nil {
			log.Warn().Err(err).Msg("init: cgroup setup (docker inside may not start)")
		}
	}
	target := args[sandbox.CmdlineConnect]
	if target == "" {
		target = os.Getenv("ERMITE_CONNECT")
	}
	if _, err := ParseTarget(target); err != nil {
		log.Error().Err(err).Msg("init: no executor target")
		return 1
	}

	var dockerPid int
	if _, err := os.Stat("/usr/bin/dockerd"); err == nil && !cfg.SkipDocker {
		_ = os.MkdirAll(sandbox.GuestScratch+"/docker", 0o711)
		pid, err := spawn([]string{"/usr/bin/dockerd", "--data-root", sandbox.GuestScratch + "/docker",
			"--storage-driver", "overlay2", "--host", "unix:///var/run/docker.sock"}, nil)
		if err != nil {
			log.Error().Err(err).Msg("init: start dockerd")
		} else {
			dockerPid = pid
		}
	}

	execPid, err := spawn(cfg.ExecArgs, append(os.Environ(),
		"ERMITE_CONNECT="+target, "ERMITE_SANDBOX="+mode, "ERMITE_VERSION="+cfg.Version))
	if err != nil {
		log.Error().Err(err).Msg("init: start executor")
		return 1
	}
	// SIGTERM/SIGINT (docker stop, or the host stopping the VM) go to the
	// executor, which cancels its script with the usual escalation and reports
	// its Exit; init then finishes normally instead of dying with children.
	sigs := cfg.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(ch)
		sigs = ch
	}
	stopFwd := make(chan struct{})
	defer close(stopFwd)
	go func() {
		for {
			select {
			case sig := <-sigs:
				if s, ok := sig.(syscall.Signal); ok {
					log.Info().Str("signal", s.String()).Msg("init: forwarding the signal to the executor")
					_ = syscall.Kill(-execPid, s) // the executor leads its own process group
					_ = syscall.Kill(execPid, s)
				}
			case <-stopFwd:
				return
			}
		}
	}()
	code := reapUntil(execPid)
	if dockerPid != 0 {
		_ = syscall.Kill(dockerPid, syscall.SIGTERM)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if pid, _ := wait4NoHang(dockerPid); pid == dockerPid {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = syscall.Kill(dockerPid, syscall.SIGKILL)
	}
	return code
}

// spawn starts argv[0] as a child (own process group) without os/exec, so that
// reapUntil is the only waiter and orphans can be reaped safely.
func spawn(argv, env []string) (int, error) {
	if env == nil {
		env = os.Environ()
	}
	return syscall.ForkExec(argv[0], argv, &syscall.ProcAttr{
		Env:   env,
		Files: []uintptr{0, 1, 2},
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
}

// reapUntil waits for every child (orphans included) and returns when pid
// exits, with its exit code (128+signal when killed).
func reapUntil(pid int) int {
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil { // ECHILD: no child left, the executor included
			return 1
		}
		if got == pid {
			switch {
			case ws.Exited():
				return ws.ExitStatus()
			case ws.Signaled():
				return 128 + int(ws.Signal())
			}
			return 1
		}
	}
}

func wait4NoHang(pid int) (int, error) {
	var ws syscall.WaitStatus
	return syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}

func powerOff() {
	unix.Sync()
	// On x86 Firecracker has no ACPI power off: a reboot makes the VMM exit
	// (kernel cmdline reboot=k). On arm64 PSCI handles power off.
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
}

// nestCgroups prepares cgroup v2 for a nested dockerd in a container: a cgroup
// with member processes cannot delegate controllers to children, so move every
// process to an "init" child and enable the controllers (like the upstream
// docker-in-docker entrypoint). No-op without cgroup v2.
func nestCgroups() error {
	const root = "/sys/fs/cgroup"
	ctl, err := os.ReadFile(root + "/cgroup.controllers")
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(root+"/init", 0o755); err != nil {
		return err
	}
	procs, err := os.ReadFile(root + "/cgroup.procs")
	if err != nil {
		return err
	}
	for _, p := range strings.Fields(string(procs)) {
		// Processes may exit meanwhile: ignore per-process errors.
		_ = os.WriteFile(root+"/init/cgroup.procs", []byte(p), 0o644)
	}
	var enable []string
	for _, c := range strings.Fields(string(ctl)) {
		enable = append(enable, "+"+c)
	}
	if len(enable) == 0 {
		return nil
	}
	if err := os.WriteFile(root+"/cgroup.subtree_control", []byte(strings.Join(enable, " ")), 0o644); err != nil {
		return fmt.Errorf("enable controllers: %w", err)
	}
	return nil
}
