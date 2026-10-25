# Images and kernel

A job runs in the image named by `ERMITE_RUNNER_IMAGE`.

| Mode | Value | Contents |
| --- | --- | --- |
| `container` | OCI reference | Default `ermite-runner-ubuntu-26.04:latest` |
| `vm` | Directory | `vmlinux` (`Image` on arm64) and `base.ext4`. Default `<state dir>/image` |

Both come from `images/ubuntu-26.04/Dockerfile` (Ubuntu 26.04 LTS). The runner executable and action runtime Node.js are attached by the host. The image also includes system Node.js and the cached toolchains listed below.

## Build

```sh
scripts/fetch-guest.sh "$(cat images/GUEST_ID)" tmp/image   # vm: the published guest

scripts/build-kernel.sh tmp/image   # vm only: builds the guest kernel
scripts/build-image.sh tmp/image    # builds the OCI image and tmp/image/base.ext4
```

Fetch the published guest unless you change its inputs.

- `build-kernel.sh` downloads Linux 6.18 LTS from kernel.org and checks the tarball against the published sha256 sums. It needs `bc`, `flex`, `bison`, `pahole`, libelf and openssl headers.
- `build-image.sh` needs Docker or Podman (rootless is fine) and e2fsprogs 1.47.1 or newer with libarchive, which `mkfs.ext4` loads to read the rootfs tarball (`libarchive13t64` on Ubuntu). No root is needed.
- The OCI image is tagged `ermite-runner-ubuntu-26.04:latest`. Set `IMAGE_TAG` to change it.
- The kernel config is `images/kernel/config-6.18` plus per-architecture fragments. The build fails if a listed option does not survive `olddefconfig`.
- arm64 builds with `ARCH_NAME=arm64`.
- `CONFIG_BINFMT_MISC` is built in and mounted by the guest init, so `docker/setup-qemu-action` can run foreign-architecture containers.

## Guest id and publication

The guest is the kernel (`vmlinux`) and `base.ext4`. Its inputs are `images/`, `scripts/build-kernel.sh` and `scripts/build-image.sh`; the runner executable is never part of it. `scripts/guest-id.sh` hashes those inputs as Git tracks them (modes, blob ids and paths, not `images/GUEST_ID` itself) and prints the first 12 hex digits of the sha256.

- `images/GUEST_ID` holds the id of the committed inputs. `scripts/guest-id.sh --write` updates it; `--check` fails when it is stale, which CI runs on every change. A modified input is refused unless `--allow-dirty`, which hashes the git index (staged content), not unstaged edits. `--check` and `--write` cannot be combined.
- Builds are not reproducible (apt, the `ubuntu:26.04` tag), so the id names the inputs, not the bytes. `SHA256SUMS` of the release pins the bytes.
- The guest is stored as the prerelease `guest-<id>` of ermite-runner, with the assets `vmlinux`, `base.ext4.zst` (zstd -3), `kernel.config` and `SHA256SUMS` (vmlinux, base.ext4, base.ext4.zst, kernel.config).
- `.github/workflows/guest.yml` builds and publishes it when the inputs change on `main`, or on demand (`force` publishes again). It skips a release that already has its assets. It installs `libarchive13t64` and sets `LC_ALL=C.UTF-8` (libarchive fails on other locales), and builds the kernel with `JOBS=3`.
- `scripts/publish-guest.sh [OUT_DIR]` does the same from a workstation with the `ermite` CLI (`ERMITE_URL`, `ERMITE_TOKEN`). It reuses an `OUT_DIR` whose `GUEST_ID` file matches. `ASSUME_BUILT=1` adopts an existing build that has no such file (first publication only), `FORCE=1` uploads every asset again over the existing release (`--clobber`, `SHA256SUMS` last), a commit `TARGET` missing from the forge is refused, and `DRY_RUN=1` packages without calling `ermite`.
- The workflow needs a trusted run: an owner push or merge, or a workflow dispatch by the owner. A run started by a branch-limited token gets a read-only token and cannot create the release.
- A squash merge leaves the `guest-<id>` tag on a commit outside `main`. That is fine: the id hashes the inputs, not the commit.
- `scripts/fetch-guest.sh <id> <out_dir>` downloads the release from `ERMITE_RELEASES_URL` (default `$GITHUB_SERVER_URL`, then `https://git.bnema.dev`; `GITHUB_TOKEN` is sent for that server), checks `SHA256SUMS` and unpacks `base.ext4` sparse. It needs `curl` and `zstd`, and skips a directory that already holds the verified guest.
- Delete old releases with `ermite release delete ermite-runner guest-<id>`. Each holds about 650 MiB against the repository's release quota.

The container build reads `images/GUEST_ID` and records it as the image label `dev.bnema.ermite-runner.guest-id`.

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
