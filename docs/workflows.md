# Workflow features

## Job containers and service containers

`container:` and `services:` work as on GitHub-hosted Linux runners. The containers are created in the **sandbox's own `dockerd`**, so nothing reaches the host.

- **Initialize containers** runs after "Set up job":
  - checks the Docker API version (1.35 or newer);
  - removes labelled leftovers;
  - creates the network `github_network_<32 hex>`;
  - pulls (3 tries) and starts each container;
  - waits for service health checks.
- Registry `credentials:` go to a private `docker --config` directory under `RUNNER_TEMP`. The password goes through stdin and is masked. The directory is removed after the pull.
- **Stop containers** is a post step (`always()`). It prints service logs and removes containers and the network. It runs after a failure, a cancellation and a timeout.
- `run:` steps and node actions in the job container go through `docker exec`. Values are passed in the environment of the docker process, never on the command line.
- Node actions in a job container use the externals mounted at `/__e`. `node<N>_alpine` is used when `/etc/*release` says alpine.
- Docker actions run as siblings on the job network.
- Contexts `job.container.{id,network}` and `job.services.<alias>.{id,network,ports}` are set.
- Not supported: container hooks (`ACTIONS_RUNNER_CONTAINER_HOOKS`) and the "already running in a container" refusal.

Security notes:

- `options:` is split into arguments and passed to `docker create` as an argument vector, never through a shell. A workflow cannot inject a second command.
- A workflow can still pass any `docker create` flag (`--privileged`, `--pid host`, `--device`) and bind volumes, as on a self-hosted GitHub runner.
- The sandbox contains this, not the runner. **Host paths in `volumes:` and `options:` are paths of the sandbox.**
- The runner warns about a volume that mounts `/`.

## Cancellation

- The host sends a cancel request to the executor.
- The executor signals the script's process group like the official runner: SIGINT at once, SIGTERM after 7.5 s, SIGKILL 2.5 s later.
- Background children that keep the output pipes open are killed when the shell exits.
- Init forwards SIGTERM and SIGINT to the executor.
- Step `timeout-minutes` is enforced.

## Annotations, summaries and masks

- `::notice`, `::warning` and `::error` become annotations, with `title`, `file`, `line`, `endLine`, `col` and `endColumn`.
- Problem matchers are supported (`add-matcher`, `remove-matcher`). Each has a 1 s timeout (`GITHUB_ACTIONS_RUNNER_ISSUE_MATCHER_TIMEOUT`) and is removed after 3 timeouts.
- Every `##[error]` the runner writes becomes an annotation.
- Limits: 10 annotations per level per step, messages cut at 4096 characters.
- `GITHUB_STEP_SUMMARY` is read after each step, up to 1 MiB.
- `::add-mask::` masks values at run time.
- Annotations and summaries are sent only when the server lists `ci.annotations` and `ci.summaries`.

Attribution for the ported code: [THIRD_PARTY.md](../THIRD_PARTY.md).
