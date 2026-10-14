// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"

	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/externals"
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

// setupCache starts the cache service of the runner (README, "Cache") and hands
// it to the sandbox drivers through scfg. ERMITE_RUNNER_CACHE=0 turns it off;
// ERMITE_RUNNER_CACHE_MAX is the size cap (default 10 GiB).
func setupCache(ctx context.Context, scfg *sandbox.Config) (*cachesvc.Service, error) {
	log := zerowrap.FromCtx(ctx)
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envCache))) {
	case "", "1", "true", "yes":
	case "0", "false", "no":
		log.Info().Msg("the cache service is off (" + envCache + "=0)")
		return nil, nil
	default:
		return nil, fmt.Errorf("%s: %q is not 0 or 1", envCache, os.Getenv(envCache))
	}
	max, err := cachesvc.ParseMax(os.Getenv(cachesvc.EnvMax))
	if err != nil {
		return nil, err
	}
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(scfg.StateDir, "cache"), Max: max, Log: log})
	if err != nil {
		return nil, err
	}
	scfg.Cache, scfg.CachePort = svc, cachesvc.DefaultPort
	log.Info().Int64("max_bytes", max).Uint16("port", scfg.CachePort).Msg("cache service on")
	return svc, nil
}

// envCache=0 turns the cache service off.
const envCache = "ERMITE_RUNNER_CACHE"

// externalsWait is how long the start of the runner waits for the Node.js
// downloads. A runner with a working network is then ready for its first job; one
// without goes on after the first failure (nothing waits for a dead network), and
// the downloads that are still running when the time is up finish in the
// background.
const externalsWait = 2 * time.Minute

// setupExternals downloads the Node.js externals (README, "Externals") and hands
// them to the sandbox drivers through scfg. It never fails the start: whatever
// cannot be provided is reported to the jobs that need it.
func setupExternals(ctx context.Context, scfg *sandbox.Config) {
	log := zerowrap.FromCtx(ctx)
	if !scfg.NodeExternals {
		log.Info().Msg("Node.js externals are off (" + sandbox.EnvNode + "=0)")
		return
	}
	m, ok := externals.New(scfg.StateDir)
	if !ok {
		log.Warn().Msg("no Node.js builds for this architecture: jobs get no externals")
		return
	}
	m.Start(ctx, externalsWait)
	scfg.Externals = m
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
	setupExternals(ctx, &scfg)
	cache, err := setupCache(ctx, &scfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 2
	}
	if cache != nil {
		defer func() { _ = cache.CloseAll() }()
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
		Features: hcfg.ServerFeaturesFunc(url),
		Cache:    cache, CachePort: scfg.CachePort,
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
