# ermite-runner

The CI runner of [ermite](https://git.bnema.dev/bnema/ermite). It runs workflows compatible with GitHub Actions (`.github/workflows/*.yml`), one job per sandbox:

- **`vm`**: a Firecracker micro-VM per job, destroyed at the end. Needs `/dev/kvm`.
- **`container`**: a container per job with its own Docker daemon, for hosts without KVM. Uses the sysbox runtime when available, else a privileged container.

`ERMITE_RUNNER_ISOLATION=auto` (default) picks `vm` when `/dev/kvm` opens read-write and Firecracker, the guest kernel and the base image exist, else `container`. The chosen mode and the reasons are logged at start.

Status: early development. This tree has the job sandboxes (phase D of the CI-1 plan) with a stub executor; the host loop that fetches jobs from ermite comes later.

## Sandboxes

Both modes run the same executable, the runner's own, attached **read-only** to the sandbox (never baked into the image, so a runner upgrade needs no image rebuild): `ermite-runner init` is the entrypoint (PID 1 of a VM), then `ermite-runner exec` connects back to the host over a framed protobuf stream (`internal/execstream/exec.proto`; vsock in a VM, a Unix socket in a container). The executable must be static: build with `CGO_ENABLED=0`.

### vm

- Firecracker is driven through its HTTP API on a unix socket with `net/http`, not through `firecracker-go-sdk` (its last release, v1.0.0 from 2022, predates Firecracker 1.17 and pulls CNI, go-openapi and netlink for a handful of PUT requests).
- Disks: the runner executable (read-only boot root, ext4 built with `mkfs.ext4 -d`, cached by the executable's hash), the shared read-only base image, and a per-job sparse scratch disk (`ERMITE_RUNNER_JOB_DISK`). The guest init builds an overlayfs root (lower = base, upper = scratch) and keeps dockerd's data root on the scratch disk. The scratch disk and `eth0` are rate limited (defaults 400 MiB/s and 20000 IOPS, 1 Gbit/s each way; `ERMITE_RUNNER_JOB_DISK_BANDWIDTH` in MiB/s, `_JOB_DISK_IOPS`, `_JOB_NET_BANDWIDTH` in Mbit/s; 0 disables).
- The jailer runs each VM as uid/gid `ERMITE_RUNNER_UID_BASE` + slot in its own chroot under the state directory, in a new PID namespace and in a cgroup v2 with `memory.max` (guest memory + 256 MiB for the VMM) and `pids.max`. The kernel, the base image and the state directory must be on the same filesystem (files are hard-linked into the jail) and the kernel and base image must be world-readable.
- One TAP (`ertap<slot>`) per VM with a /30 from `ERMITE_RUNNER_NETWORK` (default `10.200.0.0/16`), vsock for the executor stream, IPv6 disabled in the guest.
- Needs root, or `CAP_NET_ADMIN` and the rights the jailer requires (the plan runs the host process as root under systemd).

### container

- Docker Engine API via the maintained client `github.com/moby/moby/client` (`DOCKER_HOST` honoured). Runtime `sysbox-runc` when the daemon lists it, else a **privileged** container, which is host-root equivalent. That is allowed by default (ADR 0025) but never silent: the runner logs a warning at start and at the start of every job (`privileged container: host-root equivalent, network policy best effort`), `Driver.Isolation()` reports `container_privileged` (or `container_sysbox`, `vm`) for the registration with ermite, and `ERMITE_RUNNER_DENY_PRIVILEGED=1` refuses to start without sysbox.
- A **rootless** Docker daemon is refused with a clear error (a nested dockerd needs a rootful daemon or sysbox).
- Limits per job: memory with swap equal to memory (no swap), `ERMITE_RUNNER_JOB_PIDS` processes (default 32768), CPUs.
- Disk: the job's writable space is enforced like in `vm`: a sparse ext4 file of `ERMITE_RUNNER_JOB_DISK`, **loop-mounted on the host** (so this mode needs root, like the rest of it) and bind-mounted at `/scratch`. The nested dockerd's data root, `/home/runner/work` and `/tmp` are directories of that disk; the container's own writable layer stays small. A job that fills its disk gets "no space left on device" instead of filling the host.
- Per job: a bridge network (`erbr<slot>`), the scratch disk, a Unix socket in a bind-mounted per-job directory of the state directory (the job sees only that directory, not the scratch file). Every Docker object carries the label `dev.bnema.ermite-runner.job`; leftovers are reaped by label at start. The state directory path must keep the socket path under 107 bytes.
- The Docker daemon must run on the same host as the runner (host paths are bind-mounted).

### One runner per host

Reaping removes every process, device, firewall rule and Docker object the runner owns, so two runners on one host would destroy each other's jobs. Both modes take an exclusive lock at a fixed host path, `/run/ermite-runner.lock` (`ERMITE_RUNNER_LOCK_FILE` overrides it, for tests), and refuse to start when it is held, whatever their state directories or isolation modes. The kernel drops the lock when the process dies. A runner in a container or in another mount namespace must share that path (or it is not protected): do not run it so.

### Network policy (both modes)

One nftables table, `inet ermite_runner`, replaced atomically (`nft -f -`) and deleted with the last sandbox; nothing else is touched. From a sandbox: to the host only DNS to its own gateway address, where the runner's DNS forwarder listens (UDP and TCP, forwarding to the resolvers of `/etc/resolv.conf`; port 53 of a gateway is closed to every other interface); no traffic to RFC 1918, `100.64.0.0/10`, link-local (including `169.254.169.254`), loopback, the documentation, benchmarking and IETF-assigned ranges, multicast or reserved ranges; no IPv6; no traffic between sandboxes; the rest is NATed. A sandbox's own checks (source address, destination ranges) run before any "established" accept, so a guest cannot forge another's flow. The golden rulesets are in `internal/netpolicy/testdata`. The DNS forwarder limits each listener to 32 in-flight queries (UDP beyond that is dropped), 16 TCP connections, and 30 s per TCP connection.

**Side effect on the host:** the runner turns `net.ipv4.ip_forward` on (NAT needs it) and leaves it on when it exits. Forwarding is host-wide, not scoped to the runner's interfaces: if the machine must not route for other networks, restrict it with its own firewall rules (the runner's table only governs traffic of its sandboxes).

### Cancellation

The host sends a `Cancel{reason}` frame (`execstream.Drive` takes a cancel channel, not a context). The executor signals the script's process group like the official runner: SIGINT at once, SIGTERM after 7.5 s, SIGKILL 2.5 s later, with the timers stopped as soon as the shell exits and nothing signalled afterwards (the group id could be reused). Background children that keep the output pipes open are killed when the shell exits, after a short drain. Init forwards SIGTERM and SIGINT to the executor.

### Cleanup

Everything a job creates (process, TAP or bridge, nftables rules, files, loop mounts, containers, networks, cgroups) is removed by `Stop`, which also runs with a cancelled context, aborts a `Start` in progress, and by `Reap` at start for what a crash left behind. A VM leaves a state record (`<state dir>/vms/<id>.json`) before its first resource. **The network policy and the TAP or bridge are never removed while the VMM or the container may still be running**: when a process cannot be killed or a container cannot be removed, `Stop` returns an error wrapping `sandbox.ErrLeaked`, the sandbox's slot stays taken, and `Driver.Reap` (which returns `sandbox.ErrBusy` while a sandbox is running) finishes the job and frees the slot. The host should stop taking jobs on a leaked slot until then. `Sandbox.ID()` names a sandbox's files and Docker objects.

## Running jobs

```
printf %s "$TOKEN" | ermite-runner register --token-stdin   # once
ermite-runner run                                           # as a service
```

`register` picks the isolation mode like `run` will, registers the runner with
ermite (name, labels, version, isolation, architecture) and stores the runner
token in `$ERMITE_RUNNER_STATE_DIR/runner.json` (mode 0600, in a directory made
0700; a file readable by others is refused). `--replace` removes a runner of the
same name first.

**The token of `register`** (scope `runner:register`: it reads the code and the
secrets of every repository, see `runner.proto`) is looked up in this order:
standard input with `--token-stdin`, then `ERMITE_RUNNER_REGISTER_TOKEN`, then
`--token`. Prefer the first two: **the arguments of a process are visible to
every user of the host** (`ps`, `/proc/<pid>/cmdline`), the environment and
standard input are not.

**The server URL** must be `https://`. Plain `http://` is accepted for
`localhost`, `127.0.0.0/8` and `::1` only; `ERMITE_RUNNER_INSECURE_HTTP=1` allows
any other `http://` URL (tests, a trusted network) and sends every token in
clear. The client never follows a redirect, so a token cannot be carried to
another host.

`run` reaps what a previous run left, then polls `FetchJob` whenever a slot is
free (`ERMITE_RUNNER_CAPACITY`), with a `poll_id` that is kept when a response
is lost and obeying `retry_after`. For each job it sends the acknowledging
heartbeat first, then one every 30 s (a failed one is retried every 5 s), opens
`WatchJob` (a cancel request reaches the executor, which interrupts, terminates
and kills like the official runner), forwards the log with `AppendLog`
(batches of at most 1,000 lines and 64 KiB per step, numbered offsets, retried
with the same lines), sends step snapshots with `UpdateJob` and ends with an
idempotent `CompleteJob`. When ermite says the job ended (`JobEnded`) or revokes
the assignment (`unauthenticated`) the sandbox is destroyed and nothing more is
sent. On SIGINT or SIGTERM the running jobs are cancelled with
`runner_shutdown`, reported, and the sandboxes removed. The job token stays in
the host process; the sandbox receives the workflow and its contexts only.

| Variable | Meaning |
| --- | --- |
| `ERMITE_RUNNER_URL` | base URL of ermite |
| `ERMITE_RUNNER_NAME` | runner name (default: the host name) |
| `ERMITE_RUNNER_LABELS` | extra labels, comma separated (`ubuntu-latest,ubuntu-24.04`) |
| `ERMITE_RUNNER_REGISTER_TOKEN` | token for `register` (see above) |
| `ERMITE_RUNNER_INSECURE_HTTP` | `1` allows a non-loopback `http://` URL (tests only) |
| `ERMITE_RUNNER_LOG_LEVEL` | `debug`, `info` (default), `warn`, `error` |
| `ERMITE_RUNNER_LOG_FORMAT` | `console` (default, colours) or `json` |

## Installing the service

`contrib/ermite-runner.service` is a systemd unit. The runner runs as **root**
on the host (TAP devices, nftables, loop mounts, the jailer, containers); the
jobs never do: they run as user `runner` in a micro-VM or a container.

```sh
sudo install -m 0755 ermite-runner /usr/local/bin/ermite-runner     # a static build: CGO_ENABLED=0
sudo install -m 0644 contrib/ermite-runner.service /etc/systemd/system/
sudo install -d -m 0700 /etc/ermite-runner
sudo install -m 0600 /dev/stdin /etc/ermite-runner/env <<'ENV'
ERMITE_RUNNER_URL=https://ermite.example.org
ERMITE_RUNNER_NAME=igor
ERMITE_RUNNER_LABELS=ubuntu-latest,ubuntu-24.04
ERMITE_RUNNER_ISOLATION=auto
ERMITE_RUNNER_CAPACITY=2
ERMITE_RUNNER_JOB_CPUS=4
ERMITE_RUNNER_JOB_MEMORY=8G
ERMITE_RUNNER_JOB_DISK=64G
ERMITE_RUNNER_LOG_FORMAT=json
ENV
# Register once, in the same environment as the service (the settings, the state
# directory), with the token on standard input so that it is not in any
# command line:
printf %s "$TOKEN" | sudo systemd-run --pipe --wait -p EnvironmentFile=/etc/ermite-runner/env \
  -p StateDirectory=ermite-runner -p StateDirectoryMode=0700 \
  /usr/local/bin/ermite-runner register --token-stdin
sudo systemctl enable --now ermite-runner
journalctl -u ermite-runner -f
```

`systemd-run` reads the same `EnvironmentFile` as the unit, so `register` and
`run` agree on the URL, the name, the labels, the isolation mode and the state
directory (`/var/lib/ermite-runner`, the default of `ERMITE_RUNNER_STATE_DIR`).

**Logs.** The runner writes to standard error, which journald collects. Set
`ERMITE_RUNNER_LOG_FORMAT=json` under journald: one JSON event per line, with
`job_id`, `run_id` and `attempt` on every line about a job, `warn` for what is
retried and `error` for what is refused or leaked; the colours of the `console`
format are noise in a journal. `ERMITE_RUNNER_LOG_LEVEL=debug` adds detail.
Tokens are never logged.

**Stopping.** `systemctl stop` sends SIGTERM: running jobs are cancelled with
`runner_shutdown`, reported to ermite, and their sandboxes removed before the
service exits. The budget, with the defaults, is **90 s**: 30 s for the executor
to stop its steps (SIGINT, SIGTERM after 7.5 s, SIGKILL 2.5 s later, then the
post-steps), 30 s to destroy the sandbox, 30 s to flush the log and report; jobs
do this in parallel, so it does not grow with `ERMITE_RUNNER_CAPACITY`. The unit
sets `TimeoutStopSec=120`, 30 s of margin (a test in `internal/host` fails if the
two drift apart). A job whose first heartbeat is still pending when the signal
arrives is not started: ermite gives it to another runner after 30 s. A crash or
a power cut leaves sandboxes behind; the next start reaps them. Only one runner
may live on a host (a host-wide lock enforces it), because reaping removes
everything labelled as the runner's own.

**Sandboxing of the unit.** The runner has to be root (TAP devices, nftables,
loop mounts, the jailer, Docker), so the unit only takes away what it does not
use, and only with options that **leave it in the host's mount namespace**:
`NoNewPrivileges`, `ProtectClock`, `RestrictSUIDSGID`, `LockPersonality`,
`RestrictRealtime`, `SystemCallArchitectures=native` and `RestrictAddressFamilies`
(UNIX, INET, INET6, NETLINK, VSOCK).

**Deliberately absent**, and why:

- **Every option that gives the unit a mount namespace of its own:** `PrivateTmp`,
  `ProtectHome`, `ProtectSystem`, `ProtectKernelModules`, `ProtectKernelLogs`,
  `ProtectKernelTunables`, `ProtectControlGroups`, `PrivateDevices`,
  `ReadOnlyPaths`, `ReadWritePaths`, `InaccessiblePaths`, `TemporaryFileSystem`,
  `BindPaths`. In container mode the runner loop-mounts the scratch disk of each
  job and the Docker daemon, which lives in the host's namespace, binds that
  directory into the container; a mount made inside the unit's namespace is
  invisible to it and the container would get an empty directory. (`ProtectKernelModules`
  and `ProtectKernelLogs` look harmless but are implemented with mounts too.) The
  micro-VM mode would tolerate them, but one unit serves both modes, and a second
  unit for VM-only hardening is not worth maintaining. A test in `internal/host`
  fails if the unit sets any of them.
- `Delegate=`: the runner manages no cgroup of its own (the jailer writes the
  VMs' cgroups as root, Docker has its own).
- `RestrictNamespaces=`, `PrivateNetwork=`, `CapabilityBoundingSet=`,
  `SystemCallFilter=`: network namespaces and devices (TAP, veth), mounts,
  `net.ipv4.ip_forward`, capabilities handed to the jailer and Docker.
- `MemoryDenyWriteExecute=`: Firecracker maps guest memory writable and executable.
- `UMask=`: the drivers create directories that the job's user must enter, with
  explicit modes; the state directory is made 0700 by the runner.

The boundary of a job is its micro-VM or container, not this unit. Checked on a
workstation (a user manager, no root): the micro-VM job test (`TestVMRunsAJob`:
user and network namespaces, Firecracker, TAP, vsock) passes under the options
that remain. Not checked, because they need root on a real host: the jailer, loop
mounts, nftables in the host namespace, Docker.

## The image

A job runs in the image named by `ERMITE_RUNNER_IMAGE` (container: an OCI
reference; vm: the directory with `vmlinux` and `base.ext4`, both built from
`images/ubuntu-24.04/Dockerfile`). The runner binary is attached to the sandbox
by the host and is never part of the image. What the executor needs from the
image:

- a user **`runner`** with uid and gid 1001 and the home `/home/runner`; the
  executor starts as root (init needs it to start `dockerd`) and runs every step
  as this user, with HOME, USER and LOGNAME set. Without such a user the steps
  run as root (a test image only);
- the directories `/home/runner/work` and `/opt/hostedtoolcache` owned by
  `runner` (`RUNNER_TOOL_CACHE`, which the executor reads from its environment
  like the official runner; each job gets `/home/runner/work/<repo>/<repo>` and
  `/home/runner/work/_temp`, cleaned at the start of the job);
- **`bash`** and **`sh`** (the default shell is `bash -e {0}` when bash exists,
  else `sh -e {0}`; like on hosted runners `shell: bash` adds
  `--noprofile --norc -eo pipefail`), `python3` and a `python` command for
  `shell: python`; `pwsh` is not part of the CI-1 image (a step that asks for it
  fails with `pwsh: command not found`);
- `dockerd` (and passwordless `sudo` for `runner`) for jobs that use Docker: the
  job gets its own daemon with its data root on the scratch disk;
- what workflows commonly use: git, curl, jq, build-essential, ca-certificates,
  tzdata, zip/unzip. Full parity with the hosted images (`actions/runner-images`)
  is a later phase.

## Job limits

Each job gets its own resources, set for the runner as a whole:

| Variable | Default | Effect |
|---|---|---|
| `ERMITE_RUNNER_CAPACITY` | `1` | jobs in parallel: one sandbox, one uid, one `/30` network and one disk per slot. The runner polls ermite only when a slot is free. |
| `ERMITE_RUNNER_JOB_CPUS` | `2` | vCPUs of a micro-VM; CPU quota of a container |
| `ERMITE_RUNNER_JOB_MEMORY` | `4G` | guest memory (`M`, `G`, `T`; at least `256M`); a jailed VMM gets this plus 256 MiB of cgroup headroom |
| `ERMITE_RUNNER_JOB_DISK` | `64G` | size of the sparse scratch disk (at least `1G`): the overlay upper layer of a VM and the Docker data root of a container. Deleted with the job |
| `ERMITE_RUNNER_JOB_PIDS` | `32768` | process limit of a container |
| `ERMITE_RUNNER_JOB_DISK_BANDWIDTH`, `_JOB_DISK_IOPS`, `_JOB_NET_BANDWIDTH` | `400`, `20000`, `1000` | VM rate limits (MiB/s, IOPS, Mbit/s); `0` disables |

The job timeout is not a runner setting: it is the workflow's `timeout-minutes`
(360 by default), which ermite sends with the job and the executor enforces from
the moment it starts.

## Configuration (sandbox part)

| Variable | Default | |
|---|---|---|
| `ERMITE_RUNNER_ISOLATION` | `auto` | `auto`, `vm` or `container` |
| `ERMITE_RUNNER_STATE_DIR` | `/var/lib/ermite-runner` | absolute path |
| `ERMITE_RUNNER_CAPACITY` | `1` | parallel jobs (slots) |
| `ERMITE_RUNNER_IMAGE` | | VM: image directory (`vmlinux`, `base.ext4`; default `<state dir>/image`); container: OCI reference (default `ermite-runner-ubuntu-24.04:latest`) |
| `ERMITE_RUNNER_JOB_CPUS` / `_JOB_MEMORY` / `_JOB_DISK` | `2` / `4G` / `64G` | per job; sizes accept `M`, `G`, `T`. A warning is logged at start when the state directory has less free space than capacity x disk (disks are sparse: it is only a warning) |
| `ERMITE_RUNNER_JOB_PIDS` | `32768` | container process limit |
| `ERMITE_RUNNER_DENY_PRIVILEGED` | `0` | `1` refuses container mode without sysbox |
| `ERMITE_RUNNER_LOCK_FILE` | `/run/ermite-runner.lock` | host-wide single-instance lock (tests only) |
| `ERMITE_RUNNER_JOB_DISK_BANDWIDTH`, `_JOB_DISK_IOPS`, `_JOB_NET_BANDWIDTH` | `400`, `20000`, `1000` | VM rate limits in MiB/s, IOPS and Mbit/s; 0 disables |
| `ERMITE_RUNNER_UID_BASE` | `900000` | jailer uid pool start |
| `ERMITE_RUNNER_NETWORK` | `10.200.0.0/16` | pool of /30 subnets, one per slot |
| `ERMITE_RUNNER_FIRECRACKER`, `_JAILER`, `_KERNEL` | from `PATH` | binaries and kernel overrides |
| `ERMITE_RUNNER_JAILER_MODE` | `on` | `off` only for development and tests |

## Images and kernel

```sh
scripts/build-kernel.sh tmp/image   # downloads Linux 6.18 LTS from kernel.org, builds vmlinux (needs bc, flex, bison, pahole, libelf, openssl headers)
scripts/build-image.sh tmp/image    # builds the OCI image with docker or podman, exports tmp/image/base.ext4 (no root needed)
```

`scripts/build-image.sh` tags the OCI image `ermite-runner-ubuntu-24.04:latest`, the default of `ERMITE_RUNNER_IMAGE` in container mode, and needs e2fsprogs >= 1.47.1 (the runner itself needs >= 1.43 and checks it at start).

The kernel configuration is `images/kernel/config-6.18` (+ per-architecture fragments), checked after `olddefconfig`: the build fails if a listed option did not survive. arm64 builds (`ARCH_NAME=arm64`) but is not tested in CI-1.

## Tests

```sh
GOWORK=off go vet ./... && GOWORK=off go test -race ./...
```

Unit tests need nothing special (the drivers are tested with a fake Firecracker and a fake Docker engine). Integration tests are opt-in and use the real stub executor: output and exit codes come back, and nothing survives 10 jobs, including a killed one and a runner crash.

**VM** (`ERMITE_RUNNER_IT_VM=1`): needs `/dev/kvm`, Firecracker, the kernel and base image from the scripts above.

```sh
# Unprivileged: inside a user + network namespace (TAP, nftables and DNS on the TAP address work there).
# Jailer off: it needs real root. Everything else is the production path.
FIRECRACKER=/path/to/firecracker IMAGE_DIR=$PWD/tmp/image scripts/it-vm-userns.sh

# Privileged, with the jailer (the production configuration):
sudo env GOWORK=off ERMITE_RUNNER_IT_VM=1 ERMITE_RUNNER_ISOLATION=vm \
  ERMITE_RUNNER_IMAGE=$PWD/tmp/image ERMITE_RUNNER_STATE_DIR=/var/lib/ermite-runner-it \
  "$(command -v go)" test -count=1 -v ./internal/sandbox/integration -run VM
```

**Container** (`ERMITE_RUNNER_IT_CONTAINER=1`): needs a rootful Docker daemon, root (loop mounts, `CAP_NET_ADMIN`) and the OCI image (`scripts/build-image.sh`).

```sh
sudo env GOWORK=off ERMITE_RUNNER_IT_CONTAINER=1 ERMITE_RUNNER_IMAGE=ermite-runner-ubuntu-24.04:latest \
  DOCKER_HOST=unix:///var/run/docker.sock "$(command -v go)" test -count=1 -v ./internal/sandbox/integration -run Container
```

**Jobs end to end** (`ERMITE_RUNNER_IT_E2E=1`, `internal/e2e`): the real executor in the real image, started through the container driver. They also run on a workstation with a *rootless* Docker daemon (the test-only option `UnsafeAllowRootlessForTests`; the nested `dockerd` of the image cannot start there, and the jobs do not need it). `mount` and `umount` are replaced by stand-ins, since loop mounts need root.

```sh
IMAGE_TAG=ermite-runner-ubuntu-24.04:dev scripts/build-image.sh tmp/image     # or: docker build -t ermite-runner-ubuntu-24.04:dev images/ubuntu-24.04
ERMITE_RUNNER_IT_E2E=1 ERMITE_RUNNER_TEST_IMAGE=ermite-runner-ubuntu-24.04:dev GOWORK=off go test -race -count=1 ./internal/e2e
```

- `TestContainerRunsAJobAsTheRunnerUser` and `TestContainerCancelStopsTheJobAndOrphans` drive a job over the stream: steps run as `runner` (1001), file ownership, `GITHUB_PATH` and `GITHUB_ENV`, `python`, cancellation by SIGINT, the `always()` step, and the orphan cleanup of "Complete job".
- `TestChainCorpus` runs the whole chain: a fake `RunnerService` (`internal/host/hosttest`, which checks the offsets, limits and tokens of `runner.proto`) hands out 22 jobs of the ermite-conformance corpus, the real host loop starts them in containers (two at a time), and what the fake stored (steps, conclusions, outputs, log lines) is compared with the recordings made on github.com. It reads `ERMITE_CONFORMANCE_DIR` (default: a sibling checkout of ermite-conformance) and never writes to it.
- `TestVMRunsAJob` (`internal/sandbox/integration`, run by `scripts/it-vm-userns.sh`) does the first of these in a micro-VM.

Not covered anywhere yet, because they need root on a real host: the jailer (uid drop, chroot, cgroups), a rootful Docker daemon or sysbox with the nested `dockerd`, and the firewall of container mode.

`ERMITE_RUNNER_IT_DOCKER_API=1 ERMITE_RUNNER_TEST_IMAGE=<image> go test -run DockerAPIWiring ./internal/sandbox/container` exercises the real Docker Engine calls (labels, networks, volumes, reaping) against any daemon, including a rootless one; it does not test isolation.

The firewall policy has a packet-level test (`ERMITE_RUNNER_IT_NET=1`, run by `scripts/it-vm-userns.sh`): two guests and an outside host on veth pairs in separate network namespaces, with a forged source and a forged "reply" between guests.

Protobuf: `go generate ./internal/execstream` (buf and protoc-gen-go are pinned as `tool` directives in go.mod); CI lints the proto and fails when the committed generated code differs.

`ermite-runner sandbox-check [script]` runs one stub job through the selected mode and prints its output: the smoke test of an installation.

## License

MIT. See [LICENSE](LICENSE).
