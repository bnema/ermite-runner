# Sandboxes

Each job runs in its own sandbox. Both modes attach the runner's own executable read-only to the sandbox. It must be a static build (`CGO_ENABLED=0`). The runner refuses to start otherwise.

Inside the sandbox, `ermite-runner init` is the entrypoint (PID 1 in a VM). `ermite-runner exec` runs the job and talks to the host over a framed protobuf stream: vsock in a VM, a Unix socket in a container.

## vm

- Firecracker runs the job in a microVM. The VM is destroyed when the job ends.
- Needs root, or `CAP_NET_ADMIN` and the rights the jailer requires.
- Disks:
  - The runner executable is the read-only boot root.
  - The shared base image is read-only.
  - A sparse scratch disk per job (`ERMITE_RUNNER_JOB_DISK`) holds the writable layer and the Docker data root.
  - A read-only disk holds the Node.js externals, when there are any.
- The scratch disk and `eth0` are rate limited. See [job limits](configuration.md#job-limits).
- Network: one TAP device (`ertap<slot>`) per VM with a `/30` from `ERMITE_RUNNER_NETWORK`. IPv6 is disabled in the guest.
- The jailer runs each VM:
  - as uid and gid `ERMITE_RUNNER_UID_BASE` plus the slot;
  - in its own chroot under the state directory;
  - in a new PID namespace;
  - in a cgroup v2 with `memory.max` (guest memory plus 256 MiB) and `pids.max`.
- The kernel, the base image and the state directory must be on the same filesystem. The kernel and the base image must be world-readable.
- The jailed API socket path must fit in 107 bytes. The driver checks it at start.

## container

- Uses the Docker Engine API (`DOCKER_HOST` is honoured). The Docker daemon must run on the same host as the runner.
- Runtime: `sysbox-runc` when the daemon lists it. Otherwise a **privileged** container.
- A rootless Docker daemon is refused. A nested `dockerd` needs a rootful daemon or sysbox.
- Limits per job: memory (no swap), CPUs and `ERMITE_RUNNER_JOB_PIDS` processes.
- Disk:
  - A sparse ext4 file of `ERMITE_RUNNER_JOB_DISK`, loop-mounted on the host and bind-mounted at `/scratch`. This is why the mode needs root.
  - The nested `dockerd` data root, `/home/runner/work` and `/tmp` live on it.
  - A job that fills its disk gets "no space left on device".
- Per job: a bridge network (`erbr<slot>`) and a Unix socket in a per-job directory of the state directory. The job sees only that directory.
- Every Docker object carries the label `dev.bnema.ermite-runner.job`. Leftovers are reaped by label at start.
- The state directory path must keep the socket path under 107 bytes.

### Privileged container warning

- A privileged container is equivalent to host root.
- It is allowed by default. The runner logs a warning at start and at the start of every job.
- The isolation reported at registration is `container_privileged`, `container_sysbox` or `vm`.
- `ERMITE_RUNNER_DENY_PRIVILEGED=1` refuses to start without sysbox.
- Network policy is best effort in this mode.

## State directory

- The state directory and every parent must be owned by root and not writable by group or others.
- The jailer runs as root and builds chroots under `<state dir>/jail`. The driver checks `jail` itself. It does not check the parents.
- A user who can rename or replace a parent can redirect root's file operations.
- The default `/var/lib/ermite-runner` is safe. The unit creates it with `StateDirectory`.
- Do not point `ERMITE_RUNNER_STATE_DIR` into a home directory or `/tmp`.

## One runner per host

- Cleanup removes every process, device, firewall rule and Docker object the runner owns. Two runners on one host would destroy each other's jobs.
- The runner takes an exclusive lock on `/run/ermite-runner.lock` and refuses to start when it is held. `ERMITE_RUNNER_LOCK_FILE` overrides the path for tests.
- The kernel drops the lock when the process dies.
- A runner in a container or another mount namespace must share that path. Do not run it that way.

## Network policy

Both modes use one nftables table, `inet ermite_runner`. It is replaced atomically and deleted with the last sandbox. Nothing else is touched.

From a sandbox:

| Traffic | Result |
| --- | --- |
| DNS to its own gateway address | Allowed. The runner's DNS forwarder answers, using the resolvers of `/etc/resolv.conf` |
| One TCP port to the gateway, with guest forge access on | Allowed |
| Other traffic to the host | Denied |
| RFC 1918, `100.64.0.0/10`, link-local (including `169.254.169.254`), loopback, documentation, benchmarking, IETF-assigned, multicast and reserved ranges | Denied |
| IPv6 | Denied |
| Traffic between sandboxes | Denied |
| The rest | NATed |

- Source address and destination checks run before any "established" accept. A guest cannot forge another's flow.
- The DNS forwarder allows 32 in-flight queries per listener (extra UDP is dropped), 16 TCP connections, and 30 s per TCP connection.
- Golden rulesets: `internal/netpolicy/testdata`.

Host side effects:

- The runner turns `net.ipv4.ip_forward` on and leaves it on when it exits.
- Forwarding is host-wide. If the machine must not route for other networks, restrict it with your own firewall rules.
- Another firewall with a later hook (for example firewalld) can still block guest DNS. Allow DNS from `ertap*` and `erbr*` there.

## Guest forge access

Off by default. Use it on staging hosts only.

`ERMITE_RUNNER_GUEST_FORGE=1` lets sandboxes reach the server the way the runner does. It is for hosts where the server is reachable only through a local route, such as an `/etc/hosts` entry plus a reverse proxy on a local port. Jobs cannot otherwise run `git clone` against `github.server_url`.

The host name and port come from `ERMITE_RUNNER_URL`. The runner refuses to start when:

- the URL host is an IP literal;
- the URL has a path;
- the name does not resolve on the host;
- the URL is `http://` and `ERMITE_RUNNER_INSECURE_HTTP=1` is not set.

For every sandbox:

- The DNS forwarder answers `A` queries for that host name with the sandbox's gateway address. `AAAA` gets an empty answer.
- The firewall accepts TCP from the sandbox to its gateway on that one port.
- `github.server_url`, `github.api_url` and `github.graphql_url` (and the matching `GITHUB_*` variables) are rewritten to the origin of `ERMITE_RUNNER_URL`.
- The runner logs `guest forge access: sandboxes may reach <origin> on the host` at start and at every job start.

Security notes:

- The runner does not proxy. A service must listen on that port on an address the gateway reaches (`*`, not `127.0.0.1`).
- A shared reverse proxy routes by `Host` header. A job can reach every app that proxy serves on that port.
- Traffic comes from a private address. Anything behind the proxy that trusts private sources trusts job code too.
- Jobs see the server over plain HTTP in this setup.

## Cleanup

- `Stop` removes everything a job creates: processes, TAP or bridge, firewall rules, files, loop mounts, containers, networks and cgroups. It also runs with a cancelled context.
- At start, the runner reaps what a crash left behind.
- The network policy and the TAP or bridge are never removed while the VMM or container may still run.
- If a process cannot be killed or a container cannot be removed, the slot stays taken until the next reap. The host stops taking jobs on that slot.
