# Unprivileged container

`contrib/container/Containerfile` packages the runner for a rootless container host (Podman or Docker). Jobs run in Firecracker microVMs (`vm` isolation) inside the container. Nothing runs as root on the host.

## What the image holds

- `ermite-runner`, Firecracker and `tini`.
- The guest kernel (`vmlinux`) and the base disk (`base.ext4`), from [images](images.md).
- Defaults: `ERMITE_RUNNER_ISOLATION=vm`, `ERMITE_RUNNER_JAILER_MODE=off`, `ERMITE_RUNNER_STATE_DIR=/var/lib/ermite-runner`, JSON logs.

## Build

```sh
scripts/fetch-guest.sh "$(cat images/GUEST_ID)" tmp/image
docker build -f contrib/container/Containerfile \
  --build-arg VERSION=<tag> --build-arg GUEST_ID="$(cat images/GUEST_ID)" \
  -t ermite-runner:<tag> .
```

`GUEST_ID` is required and becomes the label `dev.bnema.ermite-runner.guest-id`, so `docker inspect` tells which guest an image ships. `tmp/image` can also come from `scripts/build-kernel.sh` and `scripts/build-image.sh` (see [images](images.md)).

## What the container needs

| Grant | Why |
| --- | --- |
| `/dev/kvm` | Firecracker |
| `/dev/net/tun` | One TAP device per job |
| `CAP_NET_ADMIN` | TAP devices and nftables rules, in the container's own network namespace only |
| A volume on `/var/lib/ermite-runner` | Registration, caches and job disks |

No `--privileged`, no host network, no other capability. With rootless Podman, a CDI spec can expose the two device nodes.

The jailer is off: it needs root to chroot and switch uid. The container and the user namespace are the outer boundary instead, and each job still runs in its own microVM.

## First start

With no registration in the state directory, the runner registers itself when a register token is set:

- `ERMITE_RUNNER_REGISTER_TOKEN_FILE`: path of a file holding the token (a container secret), or
- `ERMITE_RUNNER_REGISTER_TOKEN`.

The token is read only on that first start. Revoke it afterwards. Labels are fixed at registration: to change them, remove the runner on the server, delete `runner.json` from the state directory and restart with a fresh token.

## Reaching the server

When the server runs on the same container network, set `ERMITE_RUNNER_URL` to its internal address and turn on [guest forge access](sandboxes.md#guest-forge-access) with a relay:

```sh
ERMITE_RUNNER_URL=http://forge:8080
ERMITE_RUNNER_INSECURE_HTTP=1
ERMITE_RUNNER_GUEST_FORGE=1
ERMITE_RUNNER_GUEST_FORGE_TARGET=forge:8080
```

Jobs then clone from `http://forge:8080`, which the runner relays to the server.
