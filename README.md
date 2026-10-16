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
- Disks: the runner executable (read-only boot root, ext4 built with `mkfs.ext4 -d`, cached by the executable's hash), the shared read-only base image, a per-job sparse scratch disk (`ERMITE_RUNNER_JOB_DISK`) and, when there are externals (below), a fourth read-only disk with the host's Node.js builds, cached by their content. The guest init builds an overlayfs root (lower = base, upper = scratch), mounts the externals disk (`/dev/vdd`) at `/home/runner/externals` and keeps dockerd's data root on the scratch disk. The scratch disk and `eth0` are rate limited (defaults 400 MiB/s and 20000 IOPS, 1 Gbit/s each way; `ERMITE_RUNNER_JOB_DISK_BANDWIDTH` in MiB/s, `_JOB_DISK_IOPS`, `_JOB_NET_BANDWIDTH` in Mbit/s; 0 disables).
- The jailer runs each VM as uid/gid `ERMITE_RUNNER_UID_BASE` + slot in its own chroot under the state directory, in a new PID namespace and in a cgroup v2 with `memory.max` (guest memory + 256 MiB for the VMM) and `pids.max`. The kernel, the base image and the state directory must be on the same filesystem (files are hard-linked into the jail) and the kernel and base image must be world-readable.
- **The state directory and every parent of it must be owned by root and not writable by group or others.** The jailer runs as root and builds the chroots under `<state dir>/jail`; the driver checks that `jail` itself is a real directory, root-owned and not group/other-writable, but it does not walk the parents: a user who can rename or replace a parent directory can still redirect root's file operations. `/var/lib/ermite-runner` (the default, created by the unit's `StateDirectory`) is fine; do not point `ERMITE_RUNNER_STATE_DIR` into a home directory or `/tmp`.
- One TAP (`ertap<slot>`) per VM with a /30 from `ERMITE_RUNNER_NETWORK` (default `10.200.0.0/16`), vsock for the executor stream, IPv6 disabled in the guest.
- Needs root, or `CAP_NET_ADMIN` and the rights the jailer requires (the plan runs the host process as root under systemd).

### container

- Docker Engine API via the maintained client `github.com/moby/moby/client` (`DOCKER_HOST` honoured). Runtime `sysbox-runc` when the daemon lists it, else a **privileged** container, which is host-root equivalent. That is allowed by default (ADR 0025) but never silent: the runner logs a warning at start and at the start of every job (`privileged container: host-root equivalent, network policy best effort`), `Driver.Isolation()` reports `container_privileged` (or `container_sysbox`, `vm`) for the registration with ermite, and `ERMITE_RUNNER_DENY_PRIVILEGED=1` refuses to start without sysbox.
- A **rootless** Docker daemon is refused with a clear error (a nested dockerd needs a rootful daemon or sysbox).
- Limits per job: memory with swap equal to memory (no swap), `ERMITE_RUNNER_JOB_PIDS` processes (default 32768), CPUs.
- Disk: the job's writable space is enforced like in `vm`: a sparse ext4 file of `ERMITE_RUNNER_JOB_DISK`, **loop-mounted on the host** (so this mode needs root, like the rest of it) and bind-mounted at `/scratch`. The nested dockerd's data root, `/home/runner/work` and `/tmp` are directories of that disk; the container's own writable layer stays small. A job that fills its disk gets "no space left on device" instead of filling the host.
- Per job: a bridge network (`erbr<slot>`), the scratch disk, a Unix socket in a bind-mounted per-job directory of the state directory (the job sees only that directory, not the scratch file). Every Docker object carries the label `dev.bnema.ermite-runner.job`; leftovers are reaped by label at start. The state directory path must keep the socket path under 107 bytes.
- The Docker daemon must run on the same host as the runner (host paths are bind-mounted).

### Externals (Node.js)

The host provides Node.js to the jobs **outside the image**, like its own executable, so that a runner upgrade or a new Node.js version never needs an image rebuild. The builds are the ones of `actions/runner` (`Misc/externals.sh`), pinned in `internal/externals/table.go` with the SHA-256 of each archive:

| Name | Node.js | Source | Architectures |
| --- | --- | --- | --- |
| `node20` | 20.20.2 | `nodejs.org/dist/v20.20.2/node-v20.20.2-linux-<arch>.tar.gz` | x64, arm64 |
| `node24` | 24.21.0 | `nodejs.org/dist/v24.21.0/node-v24.21.0-linux-<arch>.tar.gz` | x64, arm64 |
| `node20_alpine` | 20.20.2 | `github.com/actions/alpine_nodejs` release `v20.20.2` | x64 |
| `node24_alpine` | 24.21.0 | `github.com/actions/alpine_nodejs` release `v24.21.0` | x64 |

- **Node versions of actions** follow github.com today (`RequireNode24` and `Node24ByDefault` of the executor's `ActionsConfig`, both set by the sandbox through `DefaultActionsConfig`): `using: node12`, `node16` and `node20` actions run on `node24`, whatever `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24` and `ACTIONS_ALLOW_USE_UNSECURE_NODE_VERSION` say, and `Complete job` ends with `##[warning]Node.js 20 is deprecated. The following actions target Node.js 20 but are being forced to run on Node.js 24: <actions, sorted ignoring case>. For more information see: …` (recordings `act-node-*`). Both flags off give the older phases of the runner.
- **Download.** `ermite-runner run` (and `sandbox-check`) fetch what is missing at start, into `$ERMITE_RUNNER_STATE_DIR/externals/<name>-<version>-<arch>/`, in the background; the start waits for them at most 2 minutes (a runner with a network normally takes its first job with everything in place, one without never waits long). The archive is the `.tar.gz` (the Go standard library has no xz); its SHA-256 is checked **before** anything is unpacked. Redirects must stay on https.
- **Extraction** keeps the top directory out and refuses absolute names, `..`, devices and fifos, symbolic links that are absolute or climb out of the tree, entries below a link, hard links to anything but an unpacked file, and archives over 200,000 entries or 2 GiB; writes go through `os.Root`. The tree is built in a temporary directory and renamed into place, so a directory under its final name is always complete.
- **Only pinned versions are kept**: at every start everything else in `externals/` (other versions, other architectures, interrupted downloads) is removed.
- **Layout in the sandbox** (GitHub's): `/home/runner/externals/node20/bin/node`, `/node24/bin/node`, and the alpine ones for job containers, read-only. In `container` mode they are read-only bind mounts of the host directories; in `vm` mode one ext4 disk (`mkfs.ext4 -d`) named after the content of the set (`cache/extdisk-<key>.ext4`), so a new Node.js builds a new disk and leaves the executable's disk and the base image alone. Superseded disks are removed an hour after the last build. The set is read at every job start: a download that finishes later is used by the next job.
- **When a build cannot be provided** (offline, HTTP error, wrong checksum, unsafe archive, no build for the architecture) the runner **still runs**: it logs why and attaches in its place a directory with the file `UNAVAILABLE` (the reason) and a `bin/node` script, `cat "$(dirname "$0")/../UNAVAILABLE" >&2; exit 1`, so a job that needs the tool fails with a message such as `node24 (Node.js 24.21.0) is not available on this runner: download …: SHA-256 mismatch …` rather than `No such file or directory`. These directories (`externals/.unavailable/<name>-<reason hash>`) are built once, complete, and renamed into place; a file is never rewritten in place, so a reader (the build of a VM's externals disk, a job running the stub) never sees a half-written one. A directory handed out to a sandbox in the last hour is kept by the pruning, since a running container may have it bind-mounted.
- **Retries.** A download that failed for a reason that may pass (network error, HTTP status) is retried every 5 minutes by a timer, whether jobs come or not, until the runner stops. A verdict (wrong checksum, unsafe archive, no build for the architecture) is not retried before the next start.
- **Archive layouts.** The nodejs.org archives have one top directory (`node-vX-linux-x64/`), which is dropped; the `actions/alpine_nodejs` archives start at `./bin/node` and are unpacked as they are. The alpine checksums are the digests of the GitHub release assets.
- `ERMITE_RUNNER_NODE=0` turns all of this off: nothing is downloaded or attached.

### One runner per host

Reaping removes every process, device, firewall rule and Docker object the runner owns, so two runners on one host would destroy each other's jobs. Both modes take an exclusive lock at a fixed host path, `/run/ermite-runner.lock` (`ERMITE_RUNNER_LOCK_FILE` overrides it, for tests), and refuse to start when it is held, whatever their state directories or isolation modes. The kernel drops the lock when the process dies. A runner in a container or in another mount namespace must share that path (or it is not protected): do not run it so.

### Network policy (both modes)

One nftables table, `inet ermite_runner`, replaced atomically (`nft -f -`) and deleted with the last sandbox; nothing else is touched. From a sandbox: to the host only DNS to its own gateway address (plus one TCP port when the opt-in guest forge access is on, below; the cache has no rule, see "Cache"), where the runner's DNS forwarder listens (UDP and TCP, forwarding to the resolvers of `/etc/resolv.conf`; port 53 of a gateway is closed to every other interface); no traffic to RFC 1918, `100.64.0.0/10`, link-local (including `169.254.169.254`), loopback, the documentation, benchmarking and IETF-assigned ranges, multicast or reserved ranges; no IPv6; no traffic between sandboxes; the rest is NATed. A sandbox's own checks (source address, destination ranges) run before any "established" accept, so a guest cannot forge another's flow. The golden rulesets are in `internal/netpolicy/testdata`. The DNS forwarder limits each listener to 32 in-flight queries (UDP beyond that is dropped), 16 TCP connections, and 30 s per TCP connection.

**Side effect on the host:** the runner turns `net.ipv4.ip_forward` on (NAT needs it) and leaves it on when it exits. Forwarding is host-wide, not scoped to the runner's interfaces: if the machine must not route for other networks, restrict it with its own firewall rules (the runner's table only governs traffic of its sandboxes).

### Guest forge access (opt-in, staging only)

Off by default. `ERMITE_RUNNER_GUEST_FORGE=1` lets sandboxes reach the forge **the way the runner does**, for a staging host where ermite is only reachable through an overlay (NetBird) and the runner itself uses a local route, e.g. `/etc/hosts` `ermite.bnema.dev -> 127.0.0.1` and a reverse proxy (Gordon) on `*:8088` with `ERMITE_RUNNER_URL=http://ermite.bnema.dev:8088`. Jobs otherwise cannot `git clone ${{ github.server_url }}/...`: the public URL (`https://ermite.bnema.dev`) is TLS-terminated off-host, and the private ranges are denied.

The host name and port come from `ERMITE_RUNNER_URL` (port defaulted from the scheme: 443 or 80). The runner **refuses to start** when the URL host is an IP literal (sandboxes are pointed at the host by DNS), when the URL has a path, when the name does not resolve on the host, or when the URL is plain `http://` without `ERMITE_RUNNER_INSECURE_HTTP=1` (same rule as for the runner itself: tokens then travel in clear). Then, for every sandbox in both modes:

- the DNS forwarder answers `A` queries for exactly that host name (case-insensitive, trailing dot ignored) with the sandbox's **gateway address**, and `AAAA` with an empty `NOERROR` answer; every other name is forwarded as before;
- the nftables input chain accepts TCP from the sandbox's own address to its own gateway address on that one port, before the sandbox's catch-all drop (one extra line per sandbox; see `internal/netpolicy/testdata/guest_forge.nft`). Nothing else on the host opens; the forward chain (private-range denies, source check, no sandbox-to-sandbox) is unchanged;
- the `github.server_url`, `github.api_url` and `github.graphql_url` of the job's contexts, and so `GITHUB_SERVER_URL`, `GITHUB_API_URL` and `GITHUB_GRAPHQL_URL`, are rewritten to the origin of `ERMITE_RUNNER_URL` when their origin is the server's public one (`github.server_url`); paths are kept (`/-/github/api`). Other values, and the URLs inside `github.event`, stay as the server recorded them;
- `guest forge access: sandboxes may reach <origin> on the host` is logged at start and at the start of every job, like the privileged-container warning.

**The runner does not proxy.** The traffic reaches the host's own listener on that port, so a service must listen on it on an address the gateway reaches (`*`, as Gordon does; a listener bound to `127.0.0.1` receives nothing). A shared reverse proxy on that port routes by `Host` header, so **a job can reach every app that proxy serves on that port**, not only the forge, by sending another `Host` header: use this on staging hosts only, with a proxy whose other apps you accept to expose to job code. That traffic comes from a private address (`10.200.x.x`), so anything behind the proxy that trusts private sources (admin pages, metrics, rate-limit exemptions) trusts job code too. Jobs also see the forge over plain HTTP in that setup.

### Cache (GitHub Actions cache, API v1)

The runner serves the cache of `actions/cache`, `setup-go`/`setup-node` with `cache:` and everything else built on `@actions/cache`. The toolkit uses **cache API v1** for any `GITHUB_SERVER_URL` that is not `github.com`, `*.ghe.com` or `*.localhost` (`toolkit/packages/cache/src/internal/config.ts:1-20`), so that is what is served: `GET _apis/artifactcache/cache?keys=&version=`, `POST caches` (reserve), `PATCH caches/{id}` (chunks with `Content-Range`, in any order), `POST caches/{id}` (commit), and the archive by a plain `GET` of the `archiveLocation`. `ACTIONS_CACHE_SERVICE_V2` is never set.

- **Where.** One service per runner, reachable **only from inside the sandbox**: on its loopback, `127.0.0.1:9089`, and on the address of the sandbox's own Docker bridge (`docker0`, typically `172.17.0.1:9089`) for the steps that run in a container; it listens on no network of the host and on no host port, and the host firewall has no rule for it (nothing to open on the host). A small relay in the sandbox (`internal/guest/relay.go`, started with the job and ended with it) listens on those addresses and pipes each TCP connection, in both directions with half-close, to a new connection on the host channel the executor already uses, and the runner serves HTTP on the other end:
  - **VM:** the vsock port 9089 (the relay connects to the host, CID 2). Firecracker hands the connection to the VM's own Unix socket `<vm dir>/v.sock_9089`, next to the one for the executor (`_1024`); with the jailer both are in the VM's chroot.
  - **Container:** `cache.sock` next to `exec.sock` in the per-job directory that is bind-mounted as `/run/ermite`. That directory exists for that container only.

  Each sandbox has its own listener and handler on the runner, and the runner accepts on it only the token made for that sandbox's job, so one sandbox can neither reach another's channel nor use a token on it that was not made for its job. The connection caps apply to the channel (32 per sandbox, 128 in all; each relay allows 32 open connections). Node actions on the sandbox get `ACTIONS_CACHE_URL=http://127.0.0.1:9089/` (the toolkit appends `_apis/…`), `ACTIONS_RUNTIME_URL` (same value), `ACTIONS_RUNTIME_TOKEN` and `ACTIONS_CACHE_MODE`; `run:` steps get none, as on the official runner. The token is masked. **Containers:** a job container and a docker action have a network namespace of their own, whose loopback is not the sandbox's, so node steps in a job container and docker actions get the same variables with the bridge address instead (`http://<docker0>:9089/`, read when the job starts: the nested daemon picks it). A container on any bridge network of that daemon reaches a local address of the sandbox through `INPUT`, which Docker does not filter: no `--add-host`, no name, no host rule. Without a Docker bridge those steps get no runtime variables. A relay that cannot start (port taken) leaves that job without a cache; the job runs.
- **Storage.** `<state dir>/cache/data/<id>` plus `index.json` (mode 0700/0600), written atomically (temporary file, fsync, rename, directory fsync). Total size is capped by `ERMITE_RUNNER_CACHE_MAX` (default 10G; `500M`, `20G`…): the least recently used entries are evicted first, both on commit and when a reservation needs room. A reservation is dropped after an hour without activity, and in any case at the expiry of the token that made it or an hour after it was made, whichever comes first (one with a chunk in flight is reclaimed when that chunk ends; every transfer's deadline is capped at the token's expiry). A restart keeps the entries and drops unfinished uploads.
- **Scopes.** Entries belong to (repository, ref, key, version). A job writes to its write ref only and reads its read refs, in priority order; keys are tried in the order the toolkit sends them in each ref (the primary key exactly, restore keys as prefixes, newest first). Another repository's entries are never visible. A second reservation of the same (repository, ref, key, version) while one exists or is in flight is a `409`.
- **Tokens.** `ACTIONS_RUNTIME_TOKEN` is a JWT (three base64url segments, HS256) whose claims are the job id, repository, write ref, read refs, expiry (job timeout plus 15 min), `iat`, `exp` and `scp` (`Actions.GenericRead:…`, plus the job's `Actions.Results:<run>:<job>` when it has the results service, see Artifacts). The key is derived for tokens from a random key made at start and kept in memory only (a restart ends every job anyway), so no URL signature can pass for a token. The comparison is constant time; everything wrong (no token, bad signature, expired, ids of another job or ref, a token without a cache scope) is a bare `403`. The token is not an ermite credential and ermite refuses it. The archive URL carries its own HMAC (entry, repository, issuing job, expiry) because the toolkit downloads without the bearer token; a sandbox's channel serves only URLs issued to its own job, so a URL taken out of one sandbox is refused on every other's.
- **Off.** `ERMITE_RUNNER_CACHE=0`.
- **Action archives.** The same channel serves the tarballs of actions. When the server resolves an action to a commit, the host downloads the archive once into `<state dir>/actions-cache/` (keyed by forge, repository and commit sha, never by ref) and points the sandbox at a signed URL on the cache channel (bound to the job, the key and the expiry); the sandbox extracts it as it would a download, and "Download action repository" is printed on a hit as on a miss. Concurrent jobs share one download; each entry stores its sha256, which is checked on every read (a corrupt entry is dropped and fetched again); least recently used entries are evicted above `ERMITE_RUNNER_ACTIONS_CACHE_MAX` (default `2G`, `0` turns it off), and an entry just handed out is kept for 30 minutes, and downloads in progress are charged against the cap as they are written (an archive that does not fit is not cached and the sandbox downloads it). The sandbox never writes to this cache. Whatever cannot be cached (no cache channel for the job, no sha, a failed host download) is downloaded by the sandbox as before.

**Scopes from the server.** The server sends `Job.cache {repository, write_ref, read_refs}` (feature `ci.cache`). The runner checks it (`internal/host/cache.go`): the repository must be the job's own, and the scope always carries the job's canonical `owner/name`; refs are full `refs/…` names, empty ones are dropped, at most 8 read refs; an empty `write_ref` makes the job read-only (reserve answers `403 cache write denied:`). A scope that fails the checks disables the cache for that job with a log line; the job runs. **Old server fallback:** with no `Job.cache` the scope is `github.repository` and `Job.ref`, for reading and writing, with no base or default branch fallback; a job without either gets no cache.

**Limits.** Every entry and reservation is charged at least 4 KiB, so tiny entries cannot pile up; uploads that give no `cacheSize` are charged for the bytes they write, before the bytes are accepted. At most 256 uploads in progress (32 per job); an entry being downloaded is never evicted (its bytes stay charged), and a reservation that cannot be given room answers `503`. At most 32 connections per sandbox and 128 in all; an upload or download that makes no progress for 60 s is cut. **Bandwidth:** a VM's cache traffic does not cross `eth0`, so the runner limits it itself: one limit per sandbox, shared by all its connections and both directions (request bodies and replies or archives), at the sandbox's `ERMITE_RUNNER_JOB_NET_BANDWIDTH` (Mbit/s, same default; `0` is unlimited). A transfer over the limit is slowed, not cut. Containers have no per-job network rate limit, so their cache channel is unlimited.

### Artifacts (artifacts v4)

`actions/upload-artifact@v4+` and `actions/download-artifact@v4+` (and anything else bundling `@actions/artifact` v2) work when the server has the feature `ci.artifacts`. **ermite stores the artifacts** (cross-runner downloads, retention, quota, the run page and `ermite run download`); the runner keeps no byte of them on disk. It serves GitHub's results service on the same channel and port as the cache (`internal/resultssvc`), and translates it to ermite's `JobArtifactService` with the job token, which never enters the sandbox:

- **Protocol.** Twirp JSON, `POST /twirp/github.actions.results.api.v1.ArtifactService/{CreateArtifact,FinalizeArtifact,ListArtifacts,GetSignedArtifactURL,DeleteArtifact}`, authenticated by `ACTIONS_RUNTIME_TOKEN` (the job's token with its `Actions.Results:<run id>:<job>` scope, which the toolkit decodes for its ids). The bytes go through Azure block blob requests on signed URLs: `PUT ?comp=block` (each 8 MiB block streamed to ermite as it arrives, answered `201` once ermite has stored it), `PUT ?comp=blocklist` (the commit), single-shot `PUT`, and `GET` for downloads (streamed from ermite; a stream that breaks is resumed at the byte reached, up to 3 times). Signed URLs (`/_apis/results/blob/…?se=&sig=`) are bound to the operation, the artifact, the job and the token's expiry, and use the host the step used, so they work from the sandbox and from containers alike.
- **Isolation.** Run and job ids come from the job the channel serves, never from request bodies; ermite checks the run again. A job reaches the artifacts of its own run only.
- **Errors.** ermite's message reaches the step log. Quota and size limits are `412 failed_precondition` (`413` on a block), not retried; ermite unavailable is `503`, which the toolkit retries.
- **Variables.** Node actions and docker actions get `ACTIONS_RESULTS_URL` (the same origin as `ACTIONS_CACHE_URL`, the bridge address in containers), as `NodeScriptActionHandler.cs`/`ContainerActionHandler.cs` set it. A job with no cache scope still gets the channel for artifacts, without `ACTIONS_CACHE_URL`.
- **`GITHUB_SERVER_URL` of the artifact steps.** This is the one place where the runner changes an action's environment. `@actions/artifact` refuses to run on GitHub Enterprise Server, which it recognises **only** by `GITHUB_SERVER_URL` (`isGhes()`, `toolkit/packages/artifact/src/internal/shared/config.ts`): any host but `github.com`, `*.ghe.com` and `*.localhost` fails with "not currently supported on GHES". So in the process of a **node** step (main, pre, post, also inside composites) of `actions/upload-artifact` or `actions/download-artifact` (any subpath, such as `actions/upload-artifact/merge`; `upload-pages-artifact` uses it), or of an action whose script contains that toolkit message, `GITHUB_SERVER_URL` is `https://ermite.ghe.localhost`. Nothing resolves or connects to that host: the toolkit only reads it. The `github` context, `${{ github.server_url }}` and every other step keep the real URL. Changing it globally would break git, curl and checkout; forking the actions would break SHA pins.
- **Not covered.** Artifact actions pinned below `upload-artifact@v4.3.1` / `download-artifact@v4.1.2` (their toolkit treats `*.localhost` as GHES), artifacts v3 (`@actions/artifact` v1), and the `artifact-url` output, which points at the fake host (the run page in ermite lists the artifacts). Downloads of another run (`run-id` with `github-token`) need ermite's REST endpoints.
- **Off.** `ERMITE_RUNNER_ARTIFACTS=0`, or a server without `ci.artifacts`: no `ACTIONS_RESULTS_URL`, no override, and the artifact actions fail with the toolkit's GHES message, as before. The service needs the cache channel: `ERMITE_RUNNER_CACHE=0` turns it off too.

### Cancellation

The host sends a `Cancel{reason}` frame (`execstream.Drive` takes a cancel channel, not a context). The executor signals the script's process group like the official runner: SIGINT at once, SIGTERM after 7.5 s, SIGKILL 2.5 s later, with the timers stopped as soon as the shell exits and nothing signalled afterwards (the group id could be reused). Background children that keep the output pipes open are killed when the shell exits, after a short drain. Init forwards SIGTERM and SIGINT to the executor.

### Annotations, summaries and masks

The executor ports the issue handling of the official runner: `::notice`, `::warning` and `::error` (with `title`, `file`, `line`, `endLine`, `col`, `endColumn`), problem matchers (`add-matcher`, `remove-matcher`, .NET regular expressions through `regexp2`, 1 s timeout, `GITHUB_ACTIONS_RUNNER_ISSUE_MATCHER_TIMEOUT`, removed after 3 timeouts) and every `##[error]` the runner itself writes become annotations, at most 10 per level per step, messages cut at 4096 characters; `GITHUB_STEP_SUMMARY` is read after each step (1 MiB). `::add-mask::` adds to the masker at run time, with the encodings of `ValueEncoders.cs`. The executor sends them to the host as `StepAnnotations` and `StepSummary` frames; the host checks the limits again, sends the annotations with the `UpdateJob` that completes the step (the list replaces the step's stored one) and the leftovers with `CompleteJob`, and the summaries with `UploadSummary`. Both are sent only when the server lists `ci.annotations` and `ci.summaries` in `GetVersion` (read once per job). Step `timeout-minutes` is enforced like `StepsRunner` does. Attribution for the ported code and tests: [THIRD_PARTY.md](THIRD_PARTY.md).

### Cleanup

Everything a job creates (process, TAP or bridge, nftables rules, files, loop mounts, containers, networks, cgroups) is removed by `Stop`, which also runs with a cancelled context, aborts a `Start` in progress, and by `Reap` at start for what a crash left behind. A VM leaves a state record (`<state dir>/vms/<id>.json`) before its first resource. **The network policy and the TAP or bridge are never removed while the VMM or the container may still be running**: when a process cannot be killed or a container cannot be removed, `Stop` returns an error wrapping `sandbox.ErrLeaked`, the sandbox's slot stays taken, and `Driver.Reap` (which returns `sandbox.ErrBusy` while a sandbox is running) finishes the job and frees the slot. The host should stop taking jobs on a leaked slot until then. `Sandbox.ID()` names a sandbox's files and Docker objects.

## Running jobs

```
printf %s "$TOKEN" | ermite-runner register --token-stdin   # once
ermite-runner run                                           # as a service
```

`register` picks the isolation mode like `run` will, registers the runner with
ermite (name, labels, version, isolation, architecture) and stores the runner
token in `$ERMITE_RUNNER_STATE_DIR/runner.json` (mode 0600, in a directory made
0700; a file readable by others is refused). `--replace` removes a runner of the
same name first.

**The token of `register`** (scope `runner:register`: it reads the code and the
secrets of every repository, see `runner.proto`) is looked up in this order:
standard input with `--token-stdin`, then `ERMITE_RUNNER_REGISTER_TOKEN`, then
`--token`. Prefer the first two: **the arguments of a process are visible to
every user of the host** (`ps`, `/proc/<pid>/cmdline`), the environment and
standard input are not.

**The server URL** must be `https://`. Plain `http://` is accepted for
`localhost`, `127.0.0.0/8` and `::1` only; `ERMITE_RUNNER_INSECURE_HTTP=1` allows
any other `http://` URL (tests, a trusted network) and sends every token in
clear. The client never follows a redirect, so a token cannot be carried to
another host.

`run` reaps what a previous run left, then polls `FetchJob` whenever a slot is
free (`ERMITE_RUNNER_CAPACITY`), with a `poll_id` that is kept when a response
is lost and obeying `retry_after`. For each job it sends the acknowledging
heartbeat first, then one every 30 s (a failed one is retried every 5 s), opens
`WatchJob` (a cancel request reaches the executor, which interrupts, terminates
and kills like the official runner), forwards the log with `AppendLog`
(batches of at most 1,000 lines and 64 KiB per step, numbered offsets, retried
with the same lines), sends step snapshots with `UpdateJob` and ends with an
idempotent `CompleteJob`. When ermite says the job ended (`JobEnded`) or revokes
the assignment (`unauthenticated`) the sandbox is destroyed and nothing more is
sent. On SIGINT or SIGTERM the running jobs are cancelled with
`runner_shutdown`, reported, and the sandboxes removed. The job token stays in
the host process; the sandbox receives the workflow and its contexts only.

| Variable | Meaning |
| --- | --- |
| `ERMITE_RUNNER_URL` | base URL of ermite, as in a browser (the runner adds `/-/api` itself) |
| `ERMITE_RUNNER_NAME` | runner name (default: the host name) |
| `ERMITE_RUNNER_LABELS` | extra labels, comma separated (`ubuntu-latest,ubuntu-24.04`) |
| `ERMITE_RUNNER_ENVIRONMENT` | `github-hosted` (default) or `self-hosted`: the `runner.environment` value the jobs see. ermite's runners stand in for GitHub-hosted ones, so workflows written for github.com (`runner.environment == 'github-hosted'`) behave the same; `self-hosted` also makes "Set up job" print the runner and machine name, as GitHub does for self-hosted runners |
| `ERMITE_RUNNER_REGISTER_TOKEN` | token for `register` (see above) |
| `ERMITE_RUNNER_INSECURE_HTTP` | `1` allows a non-loopback `http://` URL (tests only); also required for `ERMITE_RUNNER_GUEST_FORGE` with an `http://` URL |
| `ERMITE_RUNNER_GUEST_FORGE` | `1` lets sandboxes reach the forge as the runner does (staging only; see "Guest forge access") |
| `ERMITE_RUNNER_LOG_LEVEL` | `debug`, `info` (default), `warn`, `error` |
| `ERMITE_RUNNER_LOG_FORMAT` | `console` (default, colours) or `json` |

## Installing the service

`contrib/ermite-runner.service` is a systemd unit. The runner runs as **root**
on the host (TAP devices, nftables, loop mounts, the jailer, containers); the
jobs never do: they run as user `runner` in a micro-VM or a container.

```sh
sudo install -m 0755 ermite-runner /usr/local/bin/ermite-runner     # a static build: CGO_ENABLED=0
sudo install -m 0644 contrib/ermite-runner.service /etc/systemd/system/
sudo install -d -m 0700 /etc/ermite-runner
sudo install -m 0600 /dev/stdin /etc/ermite-runner/env <<'ENV'
ERMITE_RUNNER_URL=https://ermite.example.org
ERMITE_RUNNER_NAME=igor
ERMITE_RUNNER_LABELS=ubuntu-latest,ubuntu-24.04
ERMITE_RUNNER_ISOLATION=auto
ERMITE_RUNNER_CAPACITY=2
ERMITE_RUNNER_JOB_CPUS=4
ERMITE_RUNNER_JOB_MEMORY=8G
ERMITE_RUNNER_JOB_DISK=64G
ERMITE_RUNNER_LOG_FORMAT=json
ENV
# Register once, in the same environment as the service (the settings, the state
# directory), with the token on standard input so that it is not in any
# command line:
printf %s "$TOKEN" | sudo systemd-run --pipe --wait -p EnvironmentFile=/etc/ermite-runner/env \
  -p StateDirectory=ermite-runner -p StateDirectoryMode=0700 \
  /usr/local/bin/ermite-runner register --token-stdin
sudo systemctl enable --now ermite-runner
journalctl -u ermite-runner -f
```

`systemd-run` reads the same `EnvironmentFile` as the unit, so `register` and
`run` agree on the URL, the name, the labels, the isolation mode and the state
directory (`/var/lib/ermite-runner`, the default of `ERMITE_RUNNER_STATE_DIR`).

**Logs.** The runner writes to standard error, which journald collects. Set
`ERMITE_RUNNER_LOG_FORMAT=json` under journald: one JSON event per line, with
`job_id`, `run_id` and `attempt` on every line about a job, `warn` for what is
retried and `error` for what is refused or leaked; the colours of the `console`
format are noise in a journal. `ERMITE_RUNNER_LOG_LEVEL=debug` adds detail.
Tokens are never logged.

**Stopping.** `systemctl stop` sends SIGTERM: running jobs are cancelled with
`runner_shutdown`, reported to ermite, and their sandboxes removed before the
service exits. The budget, with the defaults, is **90 s**: 30 s for the executor
to stop its steps (SIGINT, SIGTERM after 7.5 s, SIGKILL 2.5 s later, then the
post-steps), 30 s to destroy the sandbox, 30 s to flush the log and report; jobs
do this in parallel, so it does not grow with `ERMITE_RUNNER_CAPACITY`. The unit
sets `TimeoutStopSec=120`, 30 s of margin (a test in `internal/host` fails if the
two drift apart). A job whose first heartbeat is still pending when the signal
arrives is not started: ermite gives it to another runner after 30 s. A crash or
a power cut leaves sandboxes behind; the next start reaps them. Only one runner
may live on a host (a host-wide lock enforces it), because reaping removes
everything labelled as the runner's own.

**Sandboxing of the unit.** The runner has to be root (TAP devices, nftables,
loop mounts, the jailer, Docker), so the unit only takes away what it does not
use, and only with options that **leave it in the host's mount namespace**:
`NoNewPrivileges`, `ProtectClock`, `RestrictSUIDSGID`, `LockPersonality`,
`RestrictRealtime`, `SystemCallArchitectures=native` and `RestrictAddressFamilies`
(UNIX, INET, INET6, NETLINK, VSOCK).

**Deliberately absent**, and why:

- **Every option that gives the unit a mount namespace of its own:** `PrivateTmp`,
  `ProtectHome`, `ProtectSystem`, `ProtectKernelModules`, `ProtectKernelLogs`,
  `ProtectKernelTunables`, `ProtectControlGroups`, `PrivateDevices`,
  `ReadOnlyPaths`, `ReadWritePaths`, `InaccessiblePaths`, `TemporaryFileSystem`,
  `BindPaths`. In container mode the runner loop-mounts the scratch disk of each
  job and the Docker daemon, which lives in the host's namespace, binds that
  directory into the container; a mount made inside the unit's namespace is
  invisible to it and the container would get an empty directory. (`ProtectKernelModules`
  and `ProtectKernelLogs` look harmless but are implemented with mounts too.) The
  micro-VM mode would tolerate them, but one unit serves both modes, and a second
  unit for VM-only hardening is not worth maintaining. A test in `internal/host`
  fails if the unit sets any of them.
- `Delegate=`: the runner manages no cgroup of its own (the jailer writes the
  VMs' cgroups as root, Docker has its own).
- `RestrictNamespaces=`, `PrivateNetwork=`, `CapabilityBoundingSet=`,
  `SystemCallFilter=`: network namespaces and devices (TAP, veth), mounts,
  `net.ipv4.ip_forward`, capabilities handed to the jailer and Docker.
- `MemoryDenyWriteExecute=`: Firecracker maps guest memory writable and executable.
- `UMask=`: the drivers create directories that the job's user must enter, with
  explicit modes; the state directory is made 0700 by the runner.

The boundary of a job is its micro-VM or container, not this unit. Checked on a
workstation (a user manager, no root): the micro-VM job test (`TestVMRunsAJob`:
user and network namespaces, Firecracker, TAP, vsock) passes under the options
that remain. Not checked, because they need root on a real host: the jailer, loop
mounts, nftables in the host namespace, Docker.

## The image

A job runs in the image named by `ERMITE_RUNNER_IMAGE` (container: an OCI
reference; vm: the directory with `vmlinux` and `base.ext4`, both built from
`images/ubuntu-24.04/Dockerfile`). The runner binary and Node.js (see "Externals") are attached to the sandbox
by the host and are never part of the image. What the executor needs from the
image:

- a user **`runner`** with uid and gid 1001 and the home `/home/runner`; the
  executor starts as root (init needs it to start `dockerd`) and runs every step
  as this user, with HOME, USER and LOGNAME set. Without such a user the steps
  run as root (a test image only);
- the directories `/home/runner/work` and `/opt/hostedtoolcache` owned by
  `runner` (`RUNNER_TOOL_CACHE`, which the executor reads from its environment
  like the official runner; each job gets `/home/runner/work/<repo>/<repo>` and
  `/home/runner/work/_temp`, cleaned at the start of the job);
- **`bash`** and **`sh`** (the default shell is `bash -e {0}` when bash exists,
  else `sh -e {0}`; like on hosted runners `shell: bash` adds
  `--noprofile --norc -eo pipefail`), `python3` and a `python` command for
  `shell: python`; `pwsh` is not part of the CI-1 image (a step that asks for it
  fails with `pwsh: command not found`);
- `dockerd` (and passwordless `sudo` for `runner`) for jobs that use Docker: the
  job gets its own daemon with its data root on the scratch disk;
- what workflows commonly use: git, curl, jq, build-essential, ca-certificates,
  tzdata, zip/unzip. Full parity with the hosted images (`actions/runner-images`)
  is a later phase.

## Job containers and service containers

`container:` and `services:` work as on GitHub-hosted Linux runners
(`internal/executor/containers_*.go`, a port of `ContainerOperationProvider`,
`ContainerInfo`, `DockerCommandManager` and `ContainerStepHost` of
actions/runner, with their line numbers in the comments). The containers are
created in the **sandbox's own `dockerd`**, so nothing reaches the host: in `vm`
mode (the mode of hosts without Docker Engine) the daemon runs inside the
micro-VM, in `container` mode it is the nested daemon of the job.

- **Initialize containers** (a step after "Set up job"): docker version check
  (API ≥ 1.35), cleanup of labelled leftovers, network `github_network_<32 hex>`,
  for each container `docker pull` (3 tries, random 1–10 s back off),
  `docker create`/`start`, `docker port` and the health checks of the services
  (exponential back off 2–32 s, "`<alias>` service is healthy."). Registry
  `credentials:` go to a private `docker --config` directory under RUNNER_TEMP
  (password through stdin, masked in every log, directory removed after the pull).
- **Stop containers** (post step, `always()`): service logs, `docker rm --force`,
  `docker network rm`. It runs after a failure, a cancellation, a step timeout
  and the job timeout.
- **Steps in the job container**: `run:` steps and node actions go through
  `docker exec -i --workdir … -e NAME… <id> …` (values are passed in the
  environment of the docker process, translated to `/__w`, `/__t`, `/__e`,
  `/github/home`, `/github/workflow`, never on the command line). The shell is
  looked up by the container: `sh -e {0}` unless the step says otherwise. Node
  actions use the externals mounted read-only at `/__e`, `node<N>_alpine` when
  `/etc/*release` says alpine. Docker actions run as siblings on the job network.
- Contexts: `job.container.{id,network}`, `job.services.<alias>.{id,network,ports}`
  (host ports from `docker port`).
- **Untrusted input.** `options:` is split into arguments like .NET splits a
  command line and passed to `docker create` as an argument vector, never through
  a shell, so a workflow cannot inject a second command; it can still pass any
  `docker create` flag (`--privileged`, `--pid host`, `--device`, …) and bind
  volumes (`volumes: /:/host`), exactly as on a self-hosted GitHub runner. That
  is contained by the sandbox (its own daemon, its own disk), not by the runner:
  **host paths in `volumes:` and `options:` are paths of the sandbox**. A job
  that mounts `/` exposes the sandbox's filesystem to its own container only.
  The runner warns, as GitHub does, about a volume that mounts `/`.
- Not ported: container hooks (`ACTIONS_RUNNER_CONTAINER_HOOKS`), the
  "already running in a container" refusal (the sandbox's daemon is local to the
  executor).

## Job limits

Each job gets its own resources, set for the runner as a whole:

| Variable | Default | Effect |
|---|---|---|
| `ERMITE_RUNNER_CAPACITY` | `1` | jobs in parallel: one sandbox, one uid, one `/30` network and one disk per slot. The runner polls ermite only when a slot is free. |
| `ERMITE_RUNNER_JOB_CPUS` | `2` | vCPUs of a micro-VM; CPU quota of a container |
| `ERMITE_RUNNER_JOB_MEMORY` | `4G` | guest memory (`M`, `G`, `T`; at least `256M`); a jailed VMM gets this plus 256 MiB of cgroup headroom |
| `ERMITE_RUNNER_JOB_DISK` | `64G` | size of the sparse scratch disk (at least `1G`): the overlay upper layer of a VM and the Docker data root of a container. Deleted with the job |
| `ERMITE_RUNNER_JOB_PIDS` | `32768` | process limit of a container |
| `ERMITE_RUNNER_JOB_DISK_BANDWIDTH`, `_JOB_DISK_IOPS`, `_JOB_NET_BANDWIDTH` | `400`, `20000`, `1000` | VM rate limits (MiB/s, IOPS, Mbit/s); `0` disables |

The job timeout is not a runner setting: it is the workflow's `timeout-minutes`
(360 by default), which ermite sends with the job and the executor enforces from
the moment it starts.

## Configuration (sandbox part)

| Variable | Default | |
|---|---|---|
| `ERMITE_RUNNER_ISOLATION` | `auto` | `auto`, `vm` or `container` |
| `ERMITE_RUNNER_STATE_DIR` | `/var/lib/ermite-runner` | absolute path |
| `ERMITE_RUNNER_CAPACITY` | `1` | parallel jobs (slots) |
| `ERMITE_RUNNER_IMAGE` | | VM: image directory (`vmlinux`, `base.ext4`; default `<state dir>/image`); container: OCI reference (default `ermite-runner-ubuntu-24.04:latest`) |
| `ERMITE_RUNNER_JOB_CPUS` / `_JOB_MEMORY` / `_JOB_DISK` | `2` / `4G` / `64G` | per job; sizes accept `M`, `G`, `T`. A warning is logged at start when the state directory has less free space than capacity x disk (disks are sparse: it is only a warning) |
| `ERMITE_RUNNER_JOB_PIDS` | `32768` | container process limit |
| `ERMITE_RUNNER_DENY_PRIVILEGED` | `0` | `1` refuses container mode without sysbox |
| `ERMITE_RUNNER_GUEST_FORGE` | `0` | `1` lets sandboxes reach the host and port of `ERMITE_RUNNER_URL` on the host (staging only, see "Guest forge access") |
| `ERMITE_RUNNER_CACHE` | `1` | `0` turns the cache service off (see "Cache") |
| `ERMITE_RUNNER_CACHE_MAX` | `10G` | size cap of the cache, LRU eviction |
| `ERMITE_RUNNER_ACTIONS_CACHE_MAX` | `2G` | size cap of the action archive cache; `0` turns it off |
| `ERMITE_RUNNER_ARTIFACTS` | `1` | `0` turns the results service (artifacts v4) off (see "Artifacts") |
| `ERMITE_RUNNER_NODE` | `1` | `0` turns the Node.js externals off (see "Externals") |
| `ERMITE_RUNNER_LOCK_FILE` | `/run/ermite-runner.lock` | host-wide single-instance lock (tests only) |
| `ERMITE_RUNNER_JOB_DISK_BANDWIDTH`, `_JOB_DISK_IOPS`, `_JOB_NET_BANDWIDTH` | `400`, `20000`, `1000` | VM rate limits in MiB/s, IOPS and Mbit/s; 0 disables |
| `ERMITE_RUNNER_UID_BASE` | `900000` | jailer uid pool start |
| `ERMITE_RUNNER_NETWORK` | `10.200.0.0/16` | pool of /30 subnets, one per slot |
| `ERMITE_RUNNER_FIRECRACKER`, `_JAILER`, `_KERNEL` | from `PATH` | binaries and kernel overrides |
| `ERMITE_RUNNER_JAILER_MODE` | `on` | `off` only for development and tests |

## Images and kernel

```sh
scripts/build-kernel.sh tmp/image   # downloads Linux 6.18 LTS from kernel.org, builds vmlinux (needs bc, flex, bison, pahole, libelf, openssl headers)
scripts/build-image.sh tmp/image    # builds the OCI image with docker or podman, exports tmp/image/base.ext4 (no root needed)
```

`scripts/build-image.sh` tags the OCI image `ermite-runner-ubuntu-24.04:latest`, the default of `ERMITE_RUNNER_IMAGE` in container mode, and needs e2fsprogs >= 1.47.1 (the runner itself needs >= 1.43 and checks it at start).

The kernel configuration is `images/kernel/config-6.18` (+ per-architecture fragments), checked after `olddefconfig`: the build fails if a listed option did not survive. arm64 builds (`ARCH_NAME=arm64`) but is not tested in CI-1.

`CONFIG_BINFMT_MISC` is built in (the guest has no modules) and the guest init mounts it at `/proc/sys/fs/binfmt_misc`, as systemd does on GitHub's image, so `docker/setup-qemu-action` (`tonistiigi/binfmt --install`) can register QEMU handlers and run foreign-architecture containers. The handlers belong to the job's VM and disappear with it. With a kernel built before this option, the init logs the failed mount and jobs run without it.

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

**Container** (`ERMITE_RUNNER_IT_CONTAINER=1`): needs a rootful Docker daemon, root (loop mounts, `CAP_NET_ADMIN`) and the OCI image (`scripts/build-image.sh`).

```sh
sudo env GOWORK=off ERMITE_RUNNER_IT_CONTAINER=1 ERMITE_RUNNER_IMAGE=ermite-runner-ubuntu-24.04:latest \
  DOCKER_HOST=unix:///var/run/docker.sock "$(command -v go)" test -count=1 -v ./internal/sandbox/integration -run Container
```

**Jobs end to end** (`ERMITE_RUNNER_IT_E2E=1`, `internal/e2e`): the real executor in the real image, started through the container driver. They also run on a workstation with a *rootless* Docker daemon (the test-only option `UnsafeAllowRootlessForTests`; the nested `dockerd` of the image cannot start there, and the jobs do not need it). `mount` and `umount` are replaced by stand-ins, since loop mounts need root.

```sh
IMAGE_TAG=ermite-runner-ubuntu-24.04:dev scripts/build-image.sh tmp/image     # or: docker build -t ermite-runner-ubuntu-24.04:dev images/ubuntu-24.04
ERMITE_RUNNER_IT_E2E=1 ERMITE_RUNNER_TEST_IMAGE=ermite-runner-ubuntu-24.04:dev GOWORK=off go test -race -count=1 ./internal/e2e
```

- `TestContainerRunsAJobAsTheRunnerUser` and `TestContainerCancelStopsTheJobAndOrphans` drive a job over the stream: steps run as `runner` (1001), file ownership, `GITHUB_PATH` and `GITHUB_ENV`, `python`, cancellation by SIGINT, the `always()` step, and the orphan cleanup of "Complete job".
- `TestChainCorpus` runs the whole chain: a fake `RunnerService` (`internal/host/hosttest`, which checks the offsets, limits and tokens of `runner.proto`) hands out 22 jobs of the ermite-conformance corpus, the real host loop starts them in containers (two at a time), and what the fake stored (steps, conclusions, outputs, log lines) is compared with the recordings made on github.com. It reads `ERMITE_CONFORMANCE_DIR` (default: a sibling checkout of ermite-conformance) and never writes to it.
- `TestVMRunsAJob` (`internal/sandbox/integration`, run by `scripts/it-vm-userns.sh`) does the first of these in a micro-VM.
- `TestVMNodeExternals` (same place) downloads the pinned `node24` (or takes it from `ERMITE_RUNNER_IT_NODE_DIR`, a directory with `node-v24.21.0-linux-x64.tar.gz`, for an offline host; the checksum and the extraction are the real ones), boots a micro-VM and runs `/home/runner/externals/node24/bin/node -e 'console.log(process.version)'` in it, checking that the mount is read-only.

Checked as root on a real KVM host (Arch Linux, kernel 6.18 LTS, Firecracker and jailer 1.17, rootful podman as the Docker API): the jailer (uid drop, chroot, `memory.max` and `pids.max` cgroups, PID namespace), the VM and container integration scenarios, the firewall policy and NAT, the nested `dockerd` on the loop-mounted scratch disk (full disk gives `ENOSPC`), and `internal/e2e` including `TestChainCorpus`. Not checked: sysbox, and a real Docker Engine (podman's compatibility API is not a drop-in, see below).

**Jailer notes** (found on that host): Firecracker 1.17's jailer needs `--chroot-base-dir` to exist (the driver creates `<state dir>/jail`), starts Firecracker as `/firecracker --id <id> --start-time-us ...` (the `/proc` scan that finds a VMM by id knows that form), and leaves `<state dir>/jail/<exec name>` and the cgroup parent `/sys/fs/cgroup/firecracker` behind (only the per-VM directories are the runner's). The jailed API socket path (`<state dir>/jail/firecracker/<id>/root/run/firecracker.socket`) must fit in 107 bytes; the driver checks it at start.

**Host firewalls.** The runner's table has its own `input` hook at priority `filter`; another table with a later hook (firewalld at `filter + 10`) still sees a guest's packet to the gateway after the runner accepted it. A firewalld zone that rejects `udp/tcp 53` (the `public` zone with a DNS reject rule, for instance) therefore breaks the guests' DNS (the symptom is `dns silent` in `TestVMNetworkPolicy`) while NAT keeps working. On such a host, allow DNS from `ertap*` and `erbr*` in that firewall, or run the policy test in a private network namespace (as `scripts/it-vm-userns.sh` does).

**Podman as the Docker API.** `DOCKER_HOST` pointing at rootful podman's compatibility socket gets as far as creating the network, then fails: the compatibility `POST /networks/create` cannot disable netavark's DNS (`dns_enabled` is always true) and `HostConfig.Dns` equal to the network gateway is refused when the container starts (`netavark: error while applying dns entries: DNS server 10.200.0.1 is the gateway of network ...; using it would cause aardvark-dns to forward queries to itself`); the option `com.docker.network.bridge.enable_ip_masquerade` is ignored with a warning, and netavark (`firewall_driver = "nftables"` by default) adds its own rules unless it is configured with `firewall_driver = "none"`. The integration tests passed with a small test-only proxy that rewrites that one request to the libpod endpoint with `dns_enabled=false`, and a private `CONTAINERS_CONF`. Production container mode supports Docker Engine (or sysbox) only.

`ERMITE_RUNNER_IT_DOCKER_API=1 ERMITE_RUNNER_TEST_IMAGE=<image> go test -run DockerAPIWiring ./internal/sandbox/container` exercises the real Docker Engine calls (labels, networks, volumes, reaping) against any daemon, including a rootless one; it does not test isolation.

The firewall policy has a packet-level test (`ERMITE_RUNNER_IT_NET=1`, run by `scripts/it-vm-userns.sh`): two guests and an outside host on veth pairs in separate network namespaces, with a forged source and a forged "reply" between guests.

Protobuf: `go generate ./internal/execstream` (buf and protoc-gen-go are pinned as `tool` directives in go.mod); CI lints the proto and fails when the committed generated code differs.

`ermite-runner sandbox-check [script]` runs one stub job through the selected mode and prints its output: the smoke test of an installation.

## License

MIT. See [LICENSE](LICENSE).
