# ermite-runner

The CI runner of [ermite](https://git.bnema.dev/bnema/ermite). It runs workflows compatible with GitHub Actions (`.github/workflows/*.yml`), one job per sandbox:

- **`vm`**: a Firecracker micro-VM per job, destroyed at the end. Needs `/dev/kvm`.
- **`container`**: a container per job with its own Docker daemon, for hosts without KVM. Uses the sysbox runtime when available, else a privileged container.

`ERMITE_RUNNER_ISOLATION=auto` (default) picks `vm` when KVM is usable, else `container`.

Status: early development.

## License

MIT. See [LICENSE](LICENSE).
