# Node.js externals

The host provides Node.js to jobs outside the image. A runner upgrade or a new Node.js version needs no image rebuild.

## Builds

The builds are the ones of `actions/runner`. Each archive is pinned with its SHA-256 in `internal/externals/table.go`.

| Name | Node.js | Source | Architectures |
| --- | --- | --- | --- |
| `node20` | 20.20.2 | `nodejs.org/dist/v20.20.2/node-v20.20.2-linux-<arch>.tar.gz` | x64, arm64 |
| `node24` | 24.21.0 | `nodejs.org/dist/v24.21.0/node-v24.21.0-linux-<arch>.tar.gz` | x64, arm64 |
| `node20_alpine` | 20.20.2 | `github.com/actions/alpine_nodejs` release `v20.20.2` | x64 |
| `node24_alpine` | 24.21.0 | `github.com/actions/alpine_nodejs` release `v24.21.0` | x64 |

## Which Node.js an action uses

- As on github.com, actions with `using: node12`, `node16` or `node20` run on `node24`.
- `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24` and `ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION` have no effect.
- "Complete job" prints a warning that lists the actions forced from Node.js 20 to 24.

## Download

- `ermite-runner run` and `sandbox-check` fetch missing builds at start, in the background, into `$ERMITE_RUNNER_STATE_DIR/externals/<name>-<version>-<arch>/`.
- Start waits for them for at most 2 minutes.
- The SHA-256 of the `.tar.gz` is checked **before** anything is unpacked. Redirects must stay on https.
- Extraction refuses:
  - absolute names and `..`;
  - devices and fifos;
  - symbolic links that are absolute or leave the tree;
  - entries below a link;
  - hard links to anything but an unpacked file;
  - archives over 200,000 entries or 2 GiB.
- Each build is unpacked in a temporary directory and renamed into place. A directory under its final name is complete.
- At every start, everything in `externals/` that is not a pinned build is removed.

## In the sandbox

Read-only, as on GitHub: `/home/runner/externals/node20/bin/node`, `node24/bin/node` and the alpine ones.

| Mode | How |
| --- | --- |
| `container` | Read-only bind mounts of the host directories |
| `vm` | One read-only ext4 disk, built from the set (`cache/extdisk-<key>.ext4`). Old disks are removed an hour after the last build |

The set is read at every job start. A download that finishes later is used by the next job.

## When a build is unavailable

Causes: offline, HTTP error, wrong checksum, unsafe archive, or no build for the architecture.

- The runner still runs and logs why.
- The sandbox gets a stub `bin/node` and a file `UNAVAILABLE` with the reason.
- A job that needs the tool fails with a message such as `node24 (Node.js 24.21.0) is not available on this runner: ...`.
- Failures that may pass (network error, HTTP status) are retried every 5 minutes until the runner stops.
- Other failures (wrong checksum, unsafe archive, no build for the architecture) are not retried before the next start.

## Turn off

`ERMITE_RUNNER_NODE=0` turns the externals off. Nothing is downloaded or attached.
