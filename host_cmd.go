// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/host"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/drivers"
)

// loadHostConfig reads the sandbox settings and the host settings together, so
// that the state directory and the capacity are the same for both.
func loadHostConfig() (sandbox.Config, host.Config, error) {
	scfg, err := sandbox.LoadConfig(os.Getenv)
	if err != nil {
		return scfg, host.Config{}, err
	}
	name, _ := os.Hostname()
	hcfg, err := host.LoadConfig(os.Getenv, name, scfg.StateDir, scfg.Capacity, version)
	return scfg, hcfg, err
}

func buildDriver(ctx context.Context, scfg sandbox.Config) (sandbox.Driver, error) {
	return sandbox.Select(ctx, scfg, drivers.Factories(), sandbox.Prober{})
}

// registerCmd is `ermite-runner register [--token-stdin] [--token T] [--replace]`:
// it chooses the isolation mode like `run` will, registers the runner with it,
// and stores the runner token (mode 0600) in the state directory. The register
// token is read from standard input (--token-stdin) or from
// ERMITE_RUNNER_REGISTER_TOKEN; --token is accepted but the arguments of a
// process are visible to every user of the host.
func registerCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	token := fs.String("token", "", "token with the runner:register scope (visible in the process list: prefer $"+host.EnvRegisterToken+" or --token-stdin)")
	fromStdin := fs.Bool("token-stdin", false, "read the token from standard input")
	replace := fs.Bool("replace", false, "replace a runner of the same name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tok, err := host.ResolveRegisterToken(*token, *fromStdin, os.Stdin, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 2
	}
	*token = tok
	scfg, hcfg, err := loadHostConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 2
	}
	if hcfg.URL == "" {
		fmt.Fprintf(os.Stderr, "ermite-runner: %s is not set\n", host.EnvURL)
		return 2
	}
	drv, err := buildDriver(ctx, scfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 1
	}
	iso := drv.Isolation()
	_ = drv.Close()

	client := hcfg.Client(hcfg.URL, *token)
	st, err := host.Register(ctx, hcfg, client, iso, *replace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 1
	}
	fmt.Printf("registered runner %q (%s) with isolation %s; token stored in %s\n", st.Name, st.RunnerID, iso, host.StateFile(hcfg.StateDir))
	return 0
}

// runCmd is `ermite-runner run`: poll ermite for jobs and run each one in its
// own sandbox until SIGINT or SIGTERM, then cancel the running jobs with
// runner_shutdown and report them.
func runCmd(ctx context.Context) int {
	log := zerowrap.FromCtx(ctx)
	scfg, hcfg, err := loadHostConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 2
	}
	st, err := host.LoadState(hcfg.StateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 2
	}
	url := hcfg.URL
	if url == "" {
		url = st.URL
	}
	if url == "" {
		fmt.Fprintf(os.Stderr, "ermite-runner: %s is not set\n", host.EnvURL)
		return 2
	}
	if scfg.GuestForgeEnabled {
		// The sandboxes reach the forge the way the runner does: by the host name
		// and port of the URL the runner itself uses.
		gf, err := sandbox.ResolveGuestForge(ctx, url, hcfg.InsecureHTTP, net.DefaultResolver.LookupHost)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ermite-runner:", err)
			return 2
		}
		scfg.GuestForge = gf
		hcfg.GuestForgeOrigin = gf.Origin
	}
	drv, err := buildDriver(ctx, scfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 1
	}
	defer func() { _ = drv.Close() }()
	log.Info().Str("runner", st.Name).Str("isolation", string(drv.Isolation())).Int("capacity", scfg.Capacity).Msg("runner started")

	r := &host.Runner{
		Cfg:    hcfg,
		Driver: drv,
		Poll:   hcfg.Client(url, st.RunnerToken),
		Job: func(tok string) ermiteconnect.RunnerServiceClient {
			return hcfg.Client(url, tok)
		},
	}
	r.Cfg.URL = url
	if err := r.Run(ctx); err != nil {
		if errors.Is(err, host.ErrRemoved) {
			fmt.Fprintln(os.Stderr, "ermite-runner:", err)
			return 3
		}
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 1
	}
	log.Info().Msg("runner stopped")
	return 0
}
