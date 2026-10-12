// SPDX-License-Identifier: MIT

// Command ermite-runner runs CI jobs for an ermite server. Each job runs in
// its own Firecracker micro-VM, or in its own container when the host has no
// KVM, and executes workflows compatible with GitHub Actions.
//
// Subcommands: version; register stores a runner token obtained from ermite;
// run fetches jobs from ermite and runs each in a sandbox; init and exec run
// inside a sandbox (guest PID 1, and the executor); sandbox-check runs one stub
// script through the selected isolation mode and prints its output.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/guest"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	log := zerowrap.New(zerowrap.Config{Level: envOr("ERMITE_RUNNER_LOG_LEVEL", "info"), Format: envOr("ERMITE_RUNNER_LOG_FORMAT", "console")})
	ctx := zerowrap.WithCtx(context.Background(), log)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "version":
		fmt.Println("ermite-runner", version)
		return 0
	case "init":
		return guest.Init(ctx, guest.InitConfig{Version: version})
	case "exec":
		return execCmd(ctx)
	case "sandbox-check":
		return sandboxCheck(ctx, args[1:])
	case "register":
		return registerCmd(ctx, args[1:])
	case "run":
		return runCmd(ctx)
	}
	usage()
	return 2
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ermite-runner version | register [--token T] [--replace] | run | sandbox-check [script]\n       (inside a sandbox: ermite-runner init | exec)")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// execCmd is `ermite-runner exec`, the executor of a sandbox.
func execCmd(ctx context.Context) int {
	log := zerowrap.FromCtx(ctx)
	target, err := guest.ParseTarget(os.Getenv("ERMITE_CONNECT"))
	if err != nil {
		log.Error().Err(err).Msg("exec")
		return 1
	}
	code, err := guest.Exec(ctx, guest.ExecConfig{
		Target:  target,
		Sandbox: envOr("ERMITE_SANDBOX", "container"),
		Version: envOr("ERMITE_VERSION", version),
	})
	if err != nil {
		log.Error().Err(err).Msg("exec")
	}
	return code
}
