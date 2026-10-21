# Service

`contrib/ermite-runner.service` is a systemd unit. For the commands to install it, see the [README](../README.md#register-and-run).

## Privileges

- As a host service, the runner runs as root. It needs TAP devices, nftables, loop mounts, the jailer and Docker.
- To run without root on the host, use the [unprivileged container](container.md) instead.
- Jobs never run as root on the host. They run as user `runner` in a microVM or a container.

## Registration

- Register in the same environment as the service, so `register` and `run` agree on the URL, name, labels, isolation and state directory.
- `systemd-run` with the same `EnvironmentFile` does this.
- `register` picks the isolation mode like `run` will. It sends the name, labels, version, isolation and architecture.

## Logs

- The runner writes to standard error, which journald collects.
- Set `ERMITE_RUNNER_LOG_FORMAT=json` under journald. Every line about a job has `job_id`, `run_id` and `attempt`.
- `warn` marks what is retried. `error` marks what is refused or leaked.
- `ERMITE_RUNNER_LOG_LEVEL=debug` adds detail.
- Tokens are never logged.

## Stopping

`systemctl stop` sends SIGTERM. The runner:

1. Cancels running jobs with `runner_shutdown`.
2. Reports them to the server.
3. Removes their sandboxes.

Budget with the defaults:

| Step | Time |
| --- | --- |
| Executor stops its steps and runs post-steps | 30 s |
| Destroy the sandbox | 30 s |
| Flush the log and report | 30 s |
| **Total** | **90 s** |

- Jobs stop in parallel. The total does not grow with `ERMITE_RUNNER_CAPACITY`.
- The unit sets `TimeoutStopSec=120`.
- A job whose first heartbeat is pending at SIGTERM is not started. The server gives it to another runner after 30 s.
- A crash or power cut leaves sandboxes behind. The next start reaps them.

## Unit hardening

The unit sets only options that keep it in the host's mount namespace:

- `NoNewPrivileges`
- `ProtectClock`
- `RestrictSUIDSGID`
- `LockPersonality`
- `RestrictRealtime`
- `SystemCallArchitectures=native`
- `RestrictAddressFamilies` (UNIX, INET, INET6, NETLINK, VSOCK)

Do not add:

- Options that give the unit its own mount namespace: `PrivateTmp`, `ProtectHome`, `ProtectSystem`, `ProtectKernelModules`, `ProtectKernelLogs`, `ProtectKernelTunables`, `ProtectControlGroups`, `PrivateDevices`, `ReadOnlyPaths`, `ReadWritePaths`, `InaccessiblePaths`, `TemporaryFileSystem`, `BindPaths`. In `container` mode the runner loop-mounts the job disk and the Docker daemon, in the host namespace, must see it. A test fails if the unit sets one of them.
- `PrivateNetwork`, `RestrictNamespaces`, `CapabilityBoundingSet`, `SystemCallFilter`: the runner creates network devices, mounts filesystems and hands capabilities to the jailer and Docker.
- `MemoryDenyWriteExecute`: Firecracker maps guest memory writable and executable.
- `Delegate` and `UMask`: not needed.

The security boundary of a job is its microVM or container, not the unit.

## Job timing

The `job finished` line carries integer milliseconds (all `*_ms` fields).
Phases that did not run are omitted, and the driver (`sb_*`) timings appear only
when the sandbox started. `resolve_ms` includes `archive_miss_ms`, and both are
part of `run_ms`.

| Fields | Meaning |
| --- | --- |
| `ack_ms` | Assignment received to successful first heartbeat acknowledgement |
| `sandbox_ms` | Host `startSandbox` call |
| `run_ms` | Executor session (`drive`) |
| `stop_ms` | Sandbox `Stop` call |
| `complete_ms` | Completion call, including retries |
| `total_ms` | Assignment received to this log line, including log flushing |
| `resolve_calls`, `resolve_ms` | Number and summed duration of action resolution requests |
| `archive_misses`, `archive_miss_ms`, `archive_miss_bytes` | Archive misses and their summed fetch duration and bytes |
| `sb_tap_ms`, `sb_net_ms` (VM) | TAP creation and the rest of network setup |
| `sb_vmm_ms`, `sb_files_ms`, `sb_scratch_ms` (VM) | VMM launch/API readiness, shared files excluding scratch, scratch creation |
| `sb_vsock_ms`, `sb_configure_ms`, `sb_connect_ms` (VM) | Executor/cache sockets, VM configuration/start, executor connection wait |
| `sb_socket_ms`, `sb_cache_ms`, `sb_scratch_ms` (container) | Executor socket, cache socket, scratch setup |
| `sb_net_ms`, `sb_container_ms`, `sb_connect_ms` (container) | Network setup, container creation/start, executor connection wait |
| `guest_kernel_ms`, `guest_setup_ms` (VM only) | Kernel uptime at init entry, VM filesystem/system setup |
| `guest_containerd_ms`, `guest_dockerd_ms` | Containerd start/readiness, dockerd spawn through readiness wait |

Guest boot overlaps the driver's executor connection wait: do not sum those
fields with `sandbox_ms`. Driver `container running` and `micro-VM running`
lines also carry unprefixed phase fields and `total_ms` for their boot sequence.
The `action archive cache` line carries `verify_ms` for integrity verification;
a miss also carries `fetch_ms` and `bytes`. Archive verification is never skipped.
