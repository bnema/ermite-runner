// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	// DockerdArgs replaces the dockerd command line, and the check that
	// /usr/bin/dockerd exists (tests); SkipDocker still wins.
	DockerdArgs []string
	// DockerSocket is the Docker API socket dockerd listens on and init waits
	// for; default dockerSocket.
	DockerSocket string
	// DockerReadyTimeout bounds the wait for the Docker API before the executor
	// is started anyway; default dockerReadyTimeout.
	DockerReadyTimeout time.Duration
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
// root on the scratch space, wait (bounded) for its API to answer, run the executor
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
	_, statErr := os.Stat("/usr/bin/dockerd")
	if !cfg.SkipDocker && (statErr == nil || cfg.DockerdArgs != nil) {
		_ = os.MkdirAll(sandbox.GuestScratch+"/docker", 0o711)
		argv := cfg.DockerdArgs
		if argv == nil {
			argv = []string{"/usr/bin/dockerd", "--data-root", sandbox.GuestScratch + "/docker",
				"--storage-driver", "overlay2", "--host", "unix:///var/run/docker.sock"}
		}
		pid, err := spawn(argv, nil)
		if err != nil {
			log.Error().Err(err).Msg("init: start dockerd")
		} else {
			dockerPid = pid
		}
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

	// The first job-container step needs the Docker API, so the executor must
	// not start before dockerd answers. If it never does (or dies), start the
	// executor anyway: jobs that do not use Docker still run. A shutdown signal
	// during the wait aborts the boot instead: see abortBootError.
	if dockerPid != 0 {
		sock := cfg.DockerSocket
		if sock == "" {
			sock = dockerSocket
		}
		timeout := cfg.DockerReadyTimeout
		if timeout <= 0 {
			timeout = dockerReadyTimeout
		}
		err := waitDockerReady(ctx, log, sock, dockerPid, timeout, sigs)
		var exited *dockerdExitError
		var abort *abortBootError
		switch {
		case errors.As(err, &abort):
			stopDockerd(dockerPid)
			return abort.code
		case errors.As(err, &exited):
			dockerPid = 0 // already reaped
		}
	}

	execEnv := append(os.Environ(), "ERMITE_CONNECT="+target, "ERMITE_SANDBOX="+mode, "ERMITE_VERSION="+cfg.Version)
	if gw := args[sandbox.CmdlineGateway]; gw != "" {
		// A VM learns its gateway from the kernel command line; a container is
		// given it in its environment (the container driver).
		execEnv = append(execEnv, "ERMITE_GATEWAY="+gw)
	}
	execPid, err := spawn(cfg.ExecArgs, execEnv)
	if err != nil {
		log.Error().Err(err).Msg("init: start executor")
		stopDockerd(dockerPid)
		return 1
	}
	forward := func(sig os.Signal) {
		if s, ok := sig.(syscall.Signal); ok {
			log.Info().Str("signal", s.String()).Msg("init: forwarding the signal to the executor")
			_ = syscall.Kill(-execPid, s) // the executor leads its own process group
			_ = syscall.Kill(execPid, s)
		}
	}
	stopFwd := make(chan struct{})
	defer close(stopFwd)
	go func() {
		for {
			select {
			case sig := <-sigs:
				forward(sig)
			case <-stopFwd:
				return
			}
		}
	}()
	code := reapUntil(execPid)
	stopDockerd(dockerPid)
	return code
}

// stopDockerd stops dockerd (SIGTERM, then SIGKILL after 5 s) and reaps it (at
// most 5 s more after the SIGKILL); a zero pid is a no-op.
func stopDockerd(pid int) {
	if pid == 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := wait4NoHang(pid); got == pid {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	// SIGKILL cannot be ignored, so the exit is prompt; poll rather than block
	// for ever on a process stuck in the kernel (uninterruptible sleep).
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		got, err := wait4NoHang(pid)
		if got == pid || (err != nil && err != syscall.EINTR) { // ECHILD: already reaped
			return
		}
	}
}

const (
	// dockerSocket is the Docker API socket of the nested dockerd.
	dockerSocket = "/var/run/docker.sock"
	// dockerReadyTimeout bounds the wait for the Docker API at boot.
	dockerReadyTimeout = 60 * time.Second
	dockerPollInterval = 100 * time.Millisecond
	dockerPingTimeout  = 2 * time.Second
)

// dockerdExitError reports that dockerd exited (and was reaped) before its API
// answered.
type dockerdExitError struct{ status syscall.WaitStatus }

func (e *dockerdExitError) Error() string {
	if e.status.Signaled() {
		return "dockerd killed by signal " + e.status.Signal().String()
	}
	return fmt.Sprintf("dockerd exited with code %d", e.status.ExitStatus())
}

// abortBootError means a shutdown signal (or the cancelled context) arrived
// while waiting for the Docker API: init must not start the executor, stop
// dockerd and exit with code (128+signal, as for a killed executor).
//
// Not starting the executor is safe for the host: the sandbox ends before the
// executor connected ("container exited ... before the executor connected" /
// "firecracker exited before the executor connected"), and the host reports the
// job cancelled (not failed) when a cancel or shutdown was requested
// (jobRun.execute: startSandbox error with cancelRequested). A stop that the
// host did not ask for is reported as "the sandbox did not start".
type abortBootError struct{ code int }

func (e *abortBootError) Error() string {
	return fmt.Sprintf("boot aborted by a shutdown signal (exit %d)", e.code)
}

// reapChildren reaps every child that has exited (orphans adopted by init
// included) without blocking, and reports whether pid was among them, with its
// wait status.
func reapChildren(pid int) (bool, syscall.WaitStatus) {
	var found bool
	var status syscall.WaitStatus
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || got <= 0 {
			return found, status
		}
		if got == pid {
			found, status = true, ws
		}
	}
}

// waitDockerReady blocks until the Docker API on sock answers GET /_ping with
// "OK", for at most timeout. It returns nil when ready, and otherwise:
//   - *dockerdExitError: dockerd (pid, a child of init) exited and was reaped;
//   - a plain error: the deadline passed;
//   - *abortBootError: a signal arrived on sigs or ctx was cancelled.
//
// The orphans that init adopts are reaped on every tick, and a ping in flight
// never delays the deadline, the signals or the exit check. Every outcome is
// logged; for the first two the caller starts the executor anyway.
func waitDockerReady(ctx context.Context, log zerowrap.Logger, sock string, pid int, timeout time.Duration, sigs <-chan os.Signal) error {
	start := time.Now()
	log.Info().Str("socket", sock).Dur("timeout", timeout).Msg("init: waiting for the Docker API")
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel() // also aborts a ping in flight
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(dockerPollInterval)
	defer ticker.Stop()

	var pinging <-chan bool // result of the ping in flight, nil if none
	startPing := func() {
		ch := make(chan bool, 1)
		pinging = ch
		go func() { ch <- dockerPing(wctx, client) }()
	}
	checkExit := func() error {
		if ok, ws := reapChildren(pid); ok {
			err := &dockerdExitError{status: ws}
			log.Warn().Err(err).Msg("init: dockerd died before its API was ready; starting the executor anyway (jobs using Docker will fail)")
			return err
		}
		return nil
	}
	if err := checkExit(); err != nil {
		return err
	}
	startPing()
	for {
		select {
		case ok := <-pinging:
			pinging = nil
			if ok {
				log.Info().Dur("after", time.Since(start).Round(time.Millisecond)).Msg("init: the Docker API is ready")
				return nil
			}
		case <-ticker.C:
			if err := checkExit(); err != nil {
				return err
			}
			if pinging == nil {
				startPing()
			}
		case sig := <-sigs:
			return abortBoot(log, sig)
		case <-wctx.Done():
			if ctx.Err() != nil {
				return abortBoot(log, syscall.SIGTERM)
			}
			log.Warn().Dur("timeout", timeout).Msg("init: the Docker API did not answer in time; starting the executor anyway (jobs using Docker will fail)")
			return fmt.Errorf("docker API not ready after %s", timeout)
		}
	}
}

func abortBoot(log zerowrap.Logger, sig os.Signal) error {
	code := 128 + int(syscall.SIGTERM)
	if s, ok := sig.(syscall.Signal); ok {
		code = 128 + int(s)
	}
	log.Warn().Str("signal", fmt.Sprint(sig)).Msg("init: shutdown signal while waiting for the Docker API; not starting the executor, stopping dockerd")
	return &abortBootError{code: code}
}

// dockerPing reports whether GET /_ping answers 200 "OK".
func dockerPing(ctx context.Context, client *http.Client) bool {
	// One ping is bounded on its own so that a daemon that accepts and stalls
	// does not hold the others back; ctx (the readiness deadline) bounds it too.
	ctx, cancel := context.WithTimeout(ctx, dockerPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16))
	return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "OK"
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
