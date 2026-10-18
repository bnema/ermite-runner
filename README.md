<h1 align="center">ermite-runner</h1>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue?style=flat-square" alt="License: MIT"></a>
  <a href="https://github.com/bnema/ermite-runner/commits/main"><img src="https://badgen.net/github/last-commit/bnema/ermite-runner/main?icon=github" alt="Last commit"></a>
  <a href="https://github.com/bnema/ermite-runner/stargazers"><img src="https://badgen.net/github/stars/bnema/ermite-runner?icon=github" alt="GitHub stars"></a>
</p>

<p align="center">The CI runner of <a href="https://github.com/bnema/ermite">ermite</a>: runs GitHub Actions workflows, one microVM per job.</p>

> Read-only mirror. Development happens at git.bnema.dev; issues and pull requests are closed here.

---

ermite-runner fetches jobs from an ermite server and runs workflows written for GitHub Actions (`.github/workflows/*.yml`). Each job runs in its own sandbox, destroyed when the job ends.

## Isolation modes

| Mode | Sandbox | Needs |
| --- | --- | --- |
| `vm` | A Firecracker microVM per job | A usable `/dev/kvm` |
| `container` | A container per job with its own Docker daemon | A rootful Docker Engine |

`ERMITE_RUNNER_ISOLATION=auto` is the default. It picks `vm` when `/dev/kvm` is usable and Firecracker, the guest kernel and the base image exist. Otherwise it picks `container`. The chosen mode and the reasons are logged at start.

Details: [docs/sandboxes.md](docs/sandboxes.md).

## Requirements

- A Linux host. The runner runs as root.
- `ip`, `nft` and `mkfs.ext4` (e2fsprogs 1.43 or newer).
- Go, to build the runner.
- For `vm`: `/dev/kvm`, `firecracker` and `jailer` in `PATH`, a guest kernel and a base image.
- For `container`: a rootful Docker Engine and the job image. [sysbox](https://github.com/nestybox/sysbox) is optional: without it the runner uses a privileged container.

[docs/images.md](docs/images.md) explains how to build the kernel and the images.

## Build

The binary must be static:

```sh
CGO_ENABLED=0 go build -trimpath -o ermite-runner .
```

## Register and run

1. On the ermite server, create a registration token:

   ```sh
   ermite token create my-runner --preset runner
   ```

2. Install the binary, the unit and the settings:

   ```sh
   sudo install -m 0755 ermite-runner /usr/local/bin/ermite-runner
   sudo install -m 0644 contrib/ermite-runner.service /etc/systemd/system/
   sudo install -d -m 0700 /etc/ermite-runner
   sudo install -m 0600 /dev/stdin /etc/ermite-runner/env <<'ENV'
   ERMITE_RUNNER_URL=https://ermite.example.com
   ERMITE_RUNNER_NAME=runner-1
   ERMITE_RUNNER_LABELS=ubuntu-latest,ubuntu-24.04
   ERMITE_RUNNER_LOG_FORMAT=json
   ENV
   ```

3. Register once. The token goes through standard input, so it is in no command line:

   ```sh
   printf %s "$TOKEN" | sudo systemd-run --pipe --wait \
     -p EnvironmentFile=/etc/ermite-runner/env \
     -p StateDirectory=ermite-runner -p StateDirectoryMode=0700 \
     /usr/local/bin/ermite-runner register --token-stdin
   ```

4. Start the service:

   ```sh
   sudo systemctl enable --now ermite-runner
   journalctl -u ermite-runner -f
   ```

`ermite-runner run` polls the server for jobs and runs each one in a sandbox, up to `ERMITE_RUNNER_CAPACITY` jobs at a time. On SIGTERM it cancels the running jobs, reports them and removes their sandboxes.

To check an installation without a server, run `ermite-runner sandbox-check`. It runs one stub script in the selected mode.

More on the service, logs and shutdown: [docs/service.md](docs/service.md).

## Commands

| Command | Purpose |
| --- | --- |
| `ermite-runner register [--token-stdin] [--token T] [--replace]` | Register with the server and store the runner token |
| `ermite-runner run` | Fetch and run jobs |
| `ermite-runner sandbox-check [script]` | Run one stub script in a sandbox |
| `ermite-runner version` | Print the version |

## Key settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `ERMITE_RUNNER_URL` | | Base URL of the ermite server. Must be `https://` |
| `ERMITE_RUNNER_NAME` | host name | Runner name |
| `ERMITE_RUNNER_LABELS` | | Extra labels, comma separated |
| `ERMITE_RUNNER_ISOLATION` | `auto` | `auto`, `vm` or `container` |
| `ERMITE_RUNNER_CAPACITY` | `1` | Jobs in parallel |
| `ERMITE_RUNNER_JOB_CPUS` | `2` | CPUs per job |
| `ERMITE_RUNNER_JOB_MEMORY` | `4G` | Memory per job |
| `ERMITE_RUNNER_JOB_DISK` | `64G` | Scratch disk per job |
| `ERMITE_RUNNER_STATE_DIR` | `/var/lib/ermite-runner` | State directory |

All settings: [docs/configuration.md](docs/configuration.md).

## Security notes

- Run only one runner per host.
- The registration token can read the code and secrets of every repository. Pass it through standard input.
- Without sysbox, `container` mode uses a privileged container, which is equivalent to host root. Set `ERMITE_RUNNER_DENY_PRIVILEGED=1` to refuse it.
- The state directory and all its parents must be owned by root and not writable by others.

## Documentation

- [Configuration](docs/configuration.md): every environment variable
- [Sandboxes](docs/sandboxes.md): `vm` and `container` modes, network policy, cleanup
- [Service](docs/service.md): systemd unit, logs, shutdown
- [Images](docs/images.md): job image, guest kernel
- [Workflows](docs/workflows.md): job and service containers, cancellation, annotations
- [Cache and artifacts](docs/cache-and-artifacts.md): Actions cache and artifacts v4
- [Node.js externals](docs/externals.md): Node.js for JavaScript actions
- [Development](docs/development.md): build, tests

## License

MIT. See [LICENSE](LICENSE). Third-party attributions: [THIRD_PARTY.md](THIRD_PARTY.md).
