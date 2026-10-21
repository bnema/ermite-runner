# Cache and artifacts

The runner serves the GitHub Actions cache, artifacts v4 and a Docker Hub mirror to jobs. All three use one channel per sandbox.

## Channel

- The services are reachable only from inside the sandbox.
- They listen on the sandbox loopback (`127.0.0.1:9089`) and on the address of its own Docker bridge (`docker0`, typically `172.17.0.1:9089`).
- Nothing listens on a host network or port. The host firewall needs no rule.
- A relay in the sandbox pipes each connection to the host over a channel that belongs to that sandbox:
  - `vm`: vsock port 9089.
  - `container`: `cache.sock` in the per-job directory.
- A sandbox cannot reach another sandbox's channel.
- Limits: 32 connections per sandbox, 128 in all. A transfer with no progress for 60 s is cut.

### Variables

- Node actions get `ACTIONS_CACHE_URL`, `ACTIONS_RUNTIME_URL`, `ACTIONS_RUNTIME_TOKEN`, `ACTIONS_CACHE_MODE` and `ACTIONS_RESULTS_URL`. `run:` steps get none, as on the official runner.
- In job containers and docker actions the same variables point to the Docker bridge address.
- The token is masked.
- If the relay cannot start, the job runs without a cache.

## Cache (API v1)

Works with `actions/cache`, `setup-go` and `setup-node` with `cache:`, and anything built on `@actions/cache`. The toolkit uses cache API v1 for any server that is not `github.com`, `*.ghe.com` or `*.localhost`. `ACTIONS_CACHE_SERVICE_V2` is never set.

| Setting | Value |
| --- | --- |
| Storage | `<state dir>/cache`, mode 0700/0600, written atomically |
| Size cap | `ERMITE_RUNNER_CACHE_MAX`, default `10G`. Least recently used entries are evicted |
| Off | `ERMITE_RUNNER_CACHE=0` |
| Restart | Entries are kept. Unfinished uploads are dropped |

- **Scopes.** Entries belong to (repository, ref, key, version). A job writes to its write ref and reads its read refs in priority order. Another repository's entries are never visible.
- **Server scope.** The server sends the repository, write ref and read refs (feature `ci.cache`). The runner checks them: the repository must be the job's own, refs are full `refs/...` names, at most 8 read refs. An empty write ref makes the job read-only. A scope that fails the checks disables the cache for that job.
- **Old servers.** Without a scope from the server, the scope is `github.repository` and the job ref, for reading and writing.
- **Duplicates.** A second reservation of the same entry while one exists is a `409`.
- **Reservations.** Dropped after an hour without activity, or at the expiry of the token that made them.
- **Limits.** Each entry is charged at least 4 KiB. At most 256 uploads in progress (32 per job). An entry being downloaded is never evicted. A reservation that cannot get room answers `503`.
- **Bandwidth.** A VM's cache traffic does not cross `eth0`. The runner limits it per sandbox at `ERMITE_RUNNER_JOB_NET_BANDWIDTH`. Containers have no limit.

### Tokens

- `ACTIONS_RUNTIME_TOKEN` is a JWT (HS256) bound to the job, repository, refs and an expiry (job timeout plus 15 min).
- The key is random, made at start and kept in memory only.
- Any invalid token gets a bare `403`.
- The token is not an ermite credential. The server refuses it.
- Archive URLs carry their own HMAC and are served only to the job they were issued to.

### Action archives

- The host downloads an action archive once into `<state dir>/actions-cache/`, keyed by forge, repository and commit sha.
- The sandbox gets a signed URL on the cache channel and extracts the archive.
- The SHA-256 is checked on every read. A corrupt entry is dropped and fetched again.
- Size cap: `ERMITE_RUNNER_ACTIONS_CACHE_MAX`, default `2G`. `0` turns it off.
- The sandbox never writes to this cache. Anything that cannot be cached is downloaded by the sandbox.

## Artifacts (v4)

`actions/upload-artifact@v4` and later, `actions/download-artifact@v4` and later, and anything bundling `@actions/artifact` v2 work when the server has the feature `ci.artifacts`.

- **ermite stores the artifacts.** The runner keeps nothing on disk.
- The runner serves GitHub's results service (Twirp JSON plus Azure block blob requests on signed URLs) and translates it to the server with the job token.
- The job token never enters the sandbox.
- A job reaches only the artifacts of its own run.
- Quota and size limits answer `412` (`413` on a block) and are not retried. Server unavailable answers `503`, which the toolkit retries.
- Off: `ERMITE_RUNNER_ARTIFACTS=0` or a server without `ci.artifacts`. Artifacts need the cache service, so `ERMITE_RUNNER_CACHE=0` turns them off too.

### `GITHUB_SERVER_URL` in artifact steps

- `@actions/artifact` refuses to run on GitHub Enterprise Server, which it detects only from `GITHUB_SERVER_URL`.
- In node steps of `actions/upload-artifact` and `actions/download-artifact` (any subpath), and of actions that contain the toolkit's GHES message, `GITHUB_SERVER_URL` is `https://ermite.ghe.localhost`.
- Nothing resolves or connects to that host.
- The `github` context, `${{ github.server_url }}` and all other steps keep the real URL.

### Not covered

- `upload-artifact` below v4.3.1 and `download-artifact` below v4.1.2.
- Artifacts v3.
- The `artifact-url` output. It points at the fake host. The run page in ermite lists the artifacts.
- Downloads from another run (`run-id` with `github-token`).

## Docker Hub images

The runner provides an anonymous, pull-only Docker Hub mirror on the same private
channel. Each job's dockerd uses `http://127.0.0.1:9089` as a registry mirror.
Other registries are not cached. There is no HTTPS interception, and the mirror
needs no network policy rule. Guest credentials are never forwarded upstream. Private pulls
fall back to Docker Hub directly, where dockerd can use the job's credentials.

Verified sha256 blobs and manifests live under `<state dir>/registry/`. Tags are
revalidated by an upstream HEAD on each request, with the client's Accept headers
reduced to the known manifest media types. If Docker Hub is unreachable, times
out, returns a server error or rate-limits (429), a previously cached tag can be
served stale while its manifest remains stored. Unknown tags return an error so
dockerd can fall back.

A cold blob is downloaded once, whoever asks for it, and is not tied to any one
client: every client waits for the digest check and then reads the stored file.
A corrupt download returns 502 and leaves no file. A client that disconnects
does not stop the download while another client waits for it; once no client
has waited for 5 s, the download is cancelled and its temporary file removed.
Each job may have started at most 3 cold blob downloads at once (dockerd's
default concurrency), and at most 16 run at once across all jobs. A download
counts against the job that started it until it ends; a job that asks for a
layer another job is already downloading waits for it and uses none of its
quota. A request over either limit waits up to 30 s for a slot, its connection
kept alive, and then gets 503. A refused layer makes dockerd retry for about
50 s before it pulls that layer from Docker Hub directly. Each sandbox may also
have at most 8 registry requests in progress; one more gets 503 at once.

Downloads have no total time limit, but they fail after 30 s without progress.
Auth, manifest and HEAD requests are limited to 10 s. Auth and manifest requests
never follow redirects; blob requests, HEAD included, follow the CDN redirect
without the bearer token. Stored blobs support range requests. Cold blob HEAD
requests fetch only upstream metadata, not the layer body.

`ERMITE_RUNNER_REGISTRY_CACHE_MAX` defaults to `10G`. Blobs, manifests and tag
files share one least-recently-used eviction; each is charged at least 4 KiB, so
the number of files stays below the cap divided by 4 KiB. An entry that is being served is
never evicted. This cap is independent of
`ERMITE_RUNNER_CACHE_MAX`. A zero size in any unit (`0`, `0M`, `0G`) disables only
the proxy. `ERMITE_RUNNER_CACHE=0` disables the whole channel and therefore also
the proxy (logged at startup). Dockerd retains its mirror setting and falls back
to Docker Hub if the mirror is unavailable. To clear the cache, stop the runner
and remove `<state dir>/registry/`.

Jobs with neither cache permissions nor artifacts get the loopback channel for
dockerd when the proxy is on, but no `ACTIONS_CACHE_URL`, `ACTIONS_RESULTS_URL`, or
`ACTIONS_RUNTIME_TOKEN` is exposed to their steps. Registry routes need no token:
only public content is served over a channel exclusive to that sandbox.

The host logs one `registry manifest` line per request with `job_id`, `repo`,
`ref`, `hit` and `ms`. Blob misses log `registry blob` at debug level with
`job_id`, `digest`, `bytes` and `ms`; `job_id` is the job that started the
download.
