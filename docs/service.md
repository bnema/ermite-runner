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
