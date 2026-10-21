# Configuration

The runner reads environment variables only. Under systemd they come from `/etc/ermite-runner/env`.

Sizes accept `M`, `G` and `T`.

## Server and identity

| Variable | Default | Meaning |
| --- | --- | --- |
| `ERMITE_RUNNER_URL` | | Base URL of the server, as in a browser. The runner adds `/-/api` itself |
| `ERMITE_RUNNER_NAME` | host name | Runner name: 1 to 64 letters, digits, `.`, `_` and `-` |
| `ERMITE_RUNNER_LABELS` | | Extra labels, comma separated, same characters as the name |
| `ERMITE_RUNNER_ENVIRONMENT` | `github-hosted` | `github-hosted` or `self-hosted`. Sets the `runner.environment` value that jobs see. `self-hosted` also makes "Set up job" print the runner and machine name |
| `ERMITE_RUNNER_REGISTER_TOKEN` | | Token for `register`, and for the first start of `run` when no registration exists |
| `ERMITE_RUNNER_REGISTER_TOKEN_FILE` | | File holding the register token. Wins over `ERMITE_RUNNER_REGISTER_TOKEN` for the first start of `run`. See [container](container.md#first-start) |
| `ERMITE_RUNNER_INSECURE_HTTP` | `0` | `1` allows a non-loopback `http://` URL. Every token then travels in clear. For tests and trusted networks |

### Server URL

- The URL must be `https://`.
- Plain `http://` is accepted for `localhost`, `127.0.0.0/8` and `::1`.
- The client never follows a redirect, so a token cannot be carried to another host.

### Registration token

`register` reads the token from the first source that is set:

1. Standard input, with `--token-stdin`.
2. `ERMITE_RUNNER_REGISTER_TOKEN`.
3. `--token`.

Prefer the first two. The arguments of a process are visible to every user of the host.

The token has the scope `runner:register`. It can read the code and the secrets of every repository.

`register` stores the runner token in `$ERMITE_RUNNER_STATE_DIR/runner.json`. The file is mode 0600 in a directory made 0700. A file readable by others is refused. `--replace` removes a runner of the same name first.

## Sandboxes

| Variable | Default | Meaning |
| --- | --- | --- |
| `ERMITE_RUNNER_ISOLATION` | `auto` | `auto`, `vm` or `container` |
| `ERMITE_RUNNER_STATE_DIR` | `/var/lib/ermite-runner` | Absolute path. See the ownership rules in [sandboxes](sandboxes.md#state-directory) |
| `ERMITE_RUNNER_CAPACITY` | `1` | Jobs in parallel. One sandbox, one uid, one `/30` network and one disk per slot. The runner polls the server only when a slot is free |
| `ERMITE_RUNNER_IMAGE` | | `vm`: image directory with the kernel and `base.ext4` (default `<state dir>/image`). `container`: OCI reference (default `ermite-runner-ubuntu-26.04:latest`) |
| `ERMITE_RUNNER_DENY_PRIVILEGED` | `0` | `1` refuses `container` mode without sysbox |
| `ERMITE_RUNNER_NETWORK` | `10.200.0.0/16` | Pool of `/30` subnets, one per slot |
| `ERMITE_RUNNER_UID_BASE` | `900000` | First uid of the jailer pool. A VM runs as this value plus its slot |
| `ERMITE_RUNNER_FIRECRACKER` | from `PATH` | Firecracker binary |
| `ERMITE_RUNNER_JAILER` | from `PATH` | Jailer binary |
| `ERMITE_RUNNER_KERNEL` | `<image dir>/vmlinux` (`Image` on arm64) | Guest kernel |
| `ERMITE_RUNNER_JAILER_MODE` | `on` | `off` removes the chroot, uid and namespace confinement of the VMM. Use it only when an outer boundary replaces it, as in the [unprivileged container](container.md), or for development and tests |
| `ERMITE_RUNNER_LOCK_FILE` | `/run/ermite-runner.lock` | Single-instance lock. Change it for tests only |

`DOCKER_HOST` is honoured in `container` mode.

## Job limits

| Variable | Default | Meaning |
| --- | --- | --- |
| `ERMITE_RUNNER_JOB_CPUS` | `2` | vCPUs of a VM. CPU quota of a container |
| `ERMITE_RUNNER_JOB_MEMORY` | `4G` | Guest memory, at least `256M`. A jailed VMM gets this plus 256 MiB |
| `ERMITE_RUNNER_JOB_DISK` | `64G` | Sparse scratch disk, at least `1G`. Deleted with the job |
| `ERMITE_RUNNER_JOB_PIDS` | `32768` | Process limit of a container |
| `ERMITE_RUNNER_JOB_DISK_BANDWIDTH` | `400` | VM disk limit in MiB/s. `0` disables |
| `ERMITE_RUNNER_JOB_DISK_IOPS` | `20000` | VM disk limit in IOPS. `0` disables |
| `ERMITE_RUNNER_JOB_NET_BANDWIDTH` | `1000` | VM network limit in Mbit/s, each way. `0` disables |

- The runner logs a warning at start when the state directory has less free space than capacity times disk size. Disks are sparse, so it is only a warning.
- The job timeout is the workflow's `timeout-minutes` (360 by default).

## Services

| Variable | Default | Meaning |
| --- | --- | --- |
| `ERMITE_RUNNER_CACHE` | `1` | `0` turns the cache channel off, including the Actions cache and Docker Hub proxy. See [cache and artifacts](cache-and-artifacts.md) |
| `ERMITE_RUNNER_CACHE_MAX` | `10G` | Size cap of the cache. Least recently used entries are evicted |
| `ERMITE_RUNNER_ACTIONS_CACHE_MAX` | `2G` | Size cap of the action archive cache. `0` turns it off |
| `ERMITE_RUNNER_REGISTRY_CACHE_MAX` | `10G` | Docker Hub blob and manifest cache cap, independent of `ERMITE_RUNNER_CACHE_MAX`. A zero size (`0`, `0M`, `0G`) turns only the proxy off; `ERMITE_RUNNER_CACHE=0` also disables it |
| `ERMITE_RUNNER_ARTIFACTS` | `1` | `0` turns artifacts v4 off. Artifacts need the cache service |
| `ERMITE_RUNNER_NODE` | `1` | `0` turns the Node.js externals off. See [externals](externals.md) |
| `ERMITE_RUNNER_GUEST_FORGE` | `0` | `1` lets sandboxes reach the server host and port. Read the security notes first. See [sandboxes](sandboxes.md#guest-forge-access) |
| `ERMITE_RUNNER_GUEST_FORGE_TARGET` | | `host:port` the runner relays guest forge traffic to. Empty: no relay, a service must listen on the gateway port |

## Logging

| Variable | Default | Meaning |
| --- | --- | --- |
| `ERMITE_RUNNER_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `ERMITE_RUNNER_LOG_FORMAT` | `console` | `console` (colours) or `json` |

Tokens are never logged.

## Test-only

| Variable | Meaning |
| --- | --- |
| `ERMITE_RUNNER_ALLOW_ROOT_STEPS` | `1` runs steps as root in an image without the `runner` user. A runner never sets it |

Integration test variables are listed in [development](development.md).
