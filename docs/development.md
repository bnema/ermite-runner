# Development

## Build and check

```sh
CGO_ENABLED=0 go build -trimpath -o ermite-runner .
GOWORK=off go vet ./... && GOWORK=off go test -race ./...
```

- The binary must be static: the sandbox has no libc of ours.
- Unit tests need nothing special. The drivers are tested with a fake Firecracker and a fake Docker engine.
- Protobuf: `go generate ./internal/execstream`. `buf` and `protoc-gen-go` are pinned as `tool` directives in `go.mod`. CI lints the proto and fails when the committed generated code differs.
- `ermite-runner sandbox-check [script]` runs one stub job in the selected mode. It is the smoke test of an installation.

## Integration tests

They are opt-in. They run the real stub executor. Output and exit codes come back, and nothing survives 10 jobs, including a killed one and a runner crash.

| Variable | Enables |
| --- | --- |
| `ERMITE_RUNNER_IT_VM=1` | VM tests. Need `/dev/kvm`, Firecracker, the kernel and the base image |
| `ERMITE_RUNNER_IT_CONTAINER=1` | Container tests. Need a rootful Docker daemon, root and the OCI image |
| `ERMITE_RUNNER_IT_E2E=1` | End-to-end job tests in `internal/e2e` |
| `ERMITE_RUNNER_IT_NET=1` | Packet-level firewall test |
| `ERMITE_RUNNER_IT_DOCKER_API=1` | Docker Engine calls against any daemon, including rootless |
| `ERMITE_RUNNER_IT_NODE_DIR` | Directory with `node-v24.21.0-linux-x64.tar.gz`, for offline hosts |
| `ERMITE_RUNNER_TEST_IMAGE` | Image used by the container and end-to-end tests |

Build the kernel and images first: see [images](images.md).

### VM

```sh
# Unprivileged, in a user and network namespace. The jailer is off.
FIRECRACKER=/path/to/firecracker IMAGE_DIR=$PWD/tmp/image scripts/it-vm-userns.sh

# Privileged, with the jailer (the production configuration)
sudo env GOWORK=off ERMITE_RUNNER_IT_VM=1 ERMITE_RUNNER_ISOLATION=vm \
  ERMITE_RUNNER_IMAGE=$PWD/tmp/image ERMITE_RUNNER_STATE_DIR=/var/lib/ermite-runner-it \
  "$(command -v go)" test -count=1 -v ./internal/sandbox/integration -run VM
```

### Container

```sh
sudo env GOWORK=off ERMITE_RUNNER_IT_CONTAINER=1 ERMITE_RUNNER_IMAGE=ermite-runner-ubuntu-26.04:latest \
  DOCKER_HOST=unix:///var/run/docker.sock "$(command -v go)" test -count=1 -v ./internal/sandbox/integration -run Container
```

### End to end

The real executor runs in the real image, started by the container driver. It also works with a rootless Docker daemon, where `mount` and `umount` are replaced by stand-ins.

```sh
IMAGE_TAG=ermite-runner-ubuntu-26.04:dev scripts/build-image.sh tmp/image
ERMITE_RUNNER_IT_E2E=1 ERMITE_RUNNER_TEST_IMAGE=ermite-runner-ubuntu-26.04:dev GOWORK=off go test -race -count=1 ./internal/e2e
```

- `TestChainCorpus` runs jobs of the ermite-conformance corpus through a fake server and compares the result with recordings made on github.com. It reads `ERMITE_CONFORMANCE_DIR` (default: a sibling checkout of ermite-conformance) and never writes to it.

## Notes

- **Docker API.** Production `container` mode supports Docker Engine and sysbox. Podman's compatibility API is not a drop-in: it cannot disable DNS on the network that the runner creates.
- **Jailer.** The jailer needs `--chroot-base-dir` to exist. The driver creates `<state dir>/jail`. It leaves `<state dir>/jail/<exec name>` and `/sys/fs/cgroup/firecracker` behind.
- **Host firewalls.** A firewall that rejects `53` on `ertap*` or `erbr*` breaks guest DNS. The symptom is `dns silent` in `TestVMNetworkPolicy`.
