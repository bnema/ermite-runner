# Images and kernel

A job runs in the image named by `ERMITE_RUNNER_IMAGE`.

| Mode | Value | Contents |
| --- | --- | --- |
| `container` | OCI reference | Default `ermite-runner-ubuntu-26.04:latest` |
| `vm` | Directory | `vmlinux` (`Image` on arm64) and `base.ext4`. Default `<state dir>/image` |

Both come from `images/ubuntu-26.04/Dockerfile` (Ubuntu 26.04 LTS). The runner executable and action runtime Node.js are attached by the host. The image also includes system Node.js and the cached toolchains listed below.

## Build

```sh
scripts/build-kernel.sh tmp/image   # vm only: builds the guest kernel
scripts/build-image.sh tmp/image    # builds the OCI image and tmp/image/base.ext4
```

- `build-kernel.sh` downloads Linux 6.18 LTS from kernel.org and checks the tarball against the published sha256 sums. It needs `bc`, `flex`, `bison`, `pahole`, libelf and openssl headers.
- `build-image.sh` needs Docker or Podman (rootless is fine) and e2fsprogs 1.47.1 or newer with libarchive, which `mkfs.ext4` loads to read the rootfs tarball (`libarchive13t64` on Ubuntu). No root is needed.
- The OCI image is tagged `ermite-runner-ubuntu-26.04:latest`. Set `IMAGE_TAG` to change it.
- The kernel config is `images/kernel/config-6.18` plus per-architecture fragments. The build fails if a listed option does not survive `olddefconfig`.
- arm64 builds with `ARCH_NAME=arm64`.
- `CONFIG_BINFMT_MISC` is built in and mounted by the guest init, so `docker/setup-qemu-action` can run foreign-architecture containers.

## What the image must contain

- User `runner`, uid and gid 1001, home `/home/runner`. Steps run as this user. Without it, steps run as root (test images only).
- `/home/runner/work` and `/opt/hostedtoolcache`, owned by `runner`. The tool cache holds the toolchains listed in `images/ubuntu-26.04/toolcache.lock` for the image architecture.
- `bash` and `sh`. `python3` and a `python` command for `shell: python`.
- `dockerd` and passwordless `sudo` for `runner`, for jobs that use Docker.
- No `registry-mirrors` in `/etc/docker/daemon.json`. The runner starts dockerd with `--registry-mirror` (its Docker Hub mirror), and dockerd refuses to start when an option is set both as a flag and in that file.
- Common tools: git, curl, jq, build-essential, ca-certificates, tzdata, zip and unzip.

`pwsh` is not in the image. A step that asks for it fails with `pwsh: command not found`.

## Toolchains

`images/ubuntu-26.04/toolcache.lock` pins Go and Node.js archives by version,
action architecture (`x64` or `arm64`), official SHA-256 and download URL.
To add a version, add a line for each supported architecture with the checksum
from go.dev's download JSON or nodejs.org's `SHASUMS256.txt`, then rebuild with
`scripts/build-image.sh tmp/image`. A checksum mismatch fails the build.

The installer uses the `@actions/tool-cache` layout, including the sibling
`<arch>.complete` marker. `actions/setup-go` with an exact version, including
one read from `go.mod`, uses the cache only for an exact match. A range such as
`1.27` selects the highest matching cached version. Versions not in the cache
still download normally (and `check-latest: true` may check upstream).
