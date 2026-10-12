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
- Disks: the runner executable (read-only boot root, ext4 built with `mkfs.ext4 -d`, cached by the executable's hash), the shared read-only base image, and a per-job sparse scratch disk (`ERMITE_RUNNER_JOB_DISK`). The guest init builds an overlayfs root (lower = base, upper = scratch) and keeps dockerd's data root on the scratch disk.
- The jailer runs each VM as uid/gid `ERMITE_RUNNER_UID_BASE` + slot in its own chroot under the state directory. The kernel, the base image and the state directory must be on the same filesystem (files are hard-linked into the jail) and the kernel and base image must be world-readable.
- One TAP (`ertap<slot>`) per VM with a /30 from `ERMITE_RUNNER_NETWORK` (default `10.200.0.0/16`), vsock for the executor stream, IPv6 disabled in the guest.
- Needs root, or `CAP_NET_ADMIN` and the rights the jailer requires (the plan runs the host process as root under systemd).

### container

- Docker Engine API via the maintained client `github.com/moby/moby/client` (`DOCKER_HOST` honoured). Runtime `sysbox-runc` when the daemon lists it, else a **privileged** container, which is host-root equivalent: it is logged at start and its network policy is best effort.
- A **rootless** Docker daemon is refused with a clear error (a nested dockerd needs a rootful daemon or sysbox).
- Per job: a bridge network (`erbr<slot>`), a scratch volume, a Unix socket in a bind-mounted per-job directory of the state directory. Every Docker object carries the label `dev.bnema.ermite-runner.job`; leftovers are reaped by label at start.
- The Docker daemon must run on the same host as the runner (host paths are bind-mounted).

### Network policy (both modes)

One nftables table, `inet ermite_runner`, replaced atomically (`nft -f -`) and deleted with the last sandbox; nothing else is touched. From a sandbox: to the host only DNS to its own gateway address, where the runner's DNS forwarder listens (UDP and TCP, forwarding to the resolvers of `/etc/resolv.conf`); no traffic to RFC 1918, `100.64.0.0/10`, link-local (including `169.254.169.254`), loopback, multicast or reserved ranges; no IPv6; no traffic between sandboxes; the rest is NATed. The golden rulesets are in `internal/netpolicy/testdata`.

### Cleanup

Everything a job creates (process, TAP or bridge, nftables rules, files, containers, networks, volumes) is removed by `Stop`, which also runs with a cancelled context, and by `Reap` at start for what a crash left behind. A VM leaves a state record (`<state dir>/vms/<id>.json`) before its first resource; one runner instance per state directory (flock).

## Configuration (sandbox part)

| Variable | Default | |
|---|---|---|
| `ERMITE_RUNNER_ISOLATION` | `auto` | `auto`, `vm` or `container` |
| `ERMITE_RUNNER_STATE_DIR` | `/var/lib/ermite-runner` | absolute path |
| `ERMITE_RUNNER_CAPACITY` | `1` | parallel jobs (slots) |
| `ERMITE_RUNNER_IMAGE` | | VM: image directory (`vmlinux`, `base.ext4`; default `<state dir>/image`); container: OCI reference (default `ermite-runner-ubuntu-24.04:latest`) |
| `ERMITE_RUNNER_JOB_CPUS` / `_JOB_MEMORY` / `_JOB_DISK` | `2` / `4G` / `64G` | per job; sizes accept `M`, `G`, `T` |
| `ERMITE_RUNNER_UID_BASE` | `900000` | jailer uid pool start |
| `ERMITE_RUNNER_NETWORK` | `10.200.0.0/16` | pool of /30 subnets, one per slot |
| `ERMITE_RUNNER_FIRECRACKER`, `_JAILER`, `_KERNEL` | from `PATH` | binaries and kernel overrides |
| `ERMITE_RUNNER_JAILER_MODE` | `on` | `off` only for development and tests |

## Images and kernel

```sh
scripts/build-kernel.sh tmp/image   # downloads Linux 6.18 LTS from kernel.org, builds vmlinux (needs bc, flex, bison, pahole, libelf, openssl headers)
scripts/build-image.sh tmp/image    # builds the OCI image with docker or podman, exports tmp/image/base.ext4 (no root needed)
```

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

**Container** (`ERMITE_RUNNER_IT_CONTAINER=1`): needs a rootful Docker daemon, `CAP_NET_ADMIN` and the OCI image (`scripts/build-image.sh`).

```sh
sudo env GOWORK=off ERMITE_RUNNER_IT_CONTAINER=1 ERMITE_RUNNER_IMAGE=ermite-runner-ubuntu-24.04:dev \
  DOCKER_HOST=unix:///var/run/docker.sock "$(command -v go)" test -count=1 -v ./internal/sandbox/integration -run Container
```

`ERMITE_RUNNER_IT_DOCKER_API=1 ERMITE_RUNNER_TEST_IMAGE=<image> go test -run DockerAPIWiring ./internal/sandbox/container` exercises the real Docker Engine calls (labels, networks, volumes, reaping) against any daemon, including a rootless one; it does not test isolation.

`ermite-runner sandbox-check [script]` runs one stub job through the selected mode and prints its output: the smoke test of an installation.

## License

MIT. See [LICENSE](LICENSE).
