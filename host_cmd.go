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

	"bnema.dev/ermite-runner/internal/actionscache"
	"bnema.dev/ermite-runner/internal/cachesvc"
	"bnema.dev/ermite-runner/internal/externals"
	"bnema.dev/ermite-runner/internal/host"
	"bnema.dev/ermite-runner/internal/resultssvc"
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
func setupCache(ctx context.Context, scfg *sandbox.Config) (*cachesvc.Service, *actionscache.Cache, error) {
	log := zerowrap.FromCtx(ctx)
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envCache))) {
	case "", "1", "true", "yes":
	case "0", "false", "no":
		log.Info().Msg("the cache service is off (" + envCache + "=0)")
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("%s: %q is not 0 or 1", envCache, os.Getenv(envCache))
	}
	max, err := cachesvc.ParseMax(os.Getenv(cachesvc.EnvMax))
	if err != nil {
		return nil, nil, err
	}
	archives, err := setupArchives(ctx, scfg.StateDir)
	if err != nil {
		return nil, nil, err
	}
	svc, err := cachesvc.New(cachesvc.Config{Dir: filepath.Join(scfg.StateDir, "cache"), Max: max, Log: log, Archives: archives})
	if err != nil {
		return nil, nil, err
	}
	scfg.Cache = svc
	log.Info().Int64("max_bytes", max).Msg("cache service on (guest loopback only, no host port)")
	return svc, archives, nil
}

// setupArchives opens the cache of action archives (ERMITE_RUNNER_ACTIONS_CACHE_MAX,
// default 2 GiB, 0 turns it off). It is served on the channel of the cache
// service, so it is only used when that is on.
func setupArchives(ctx context.Context, stateDir string) (*actionscache.Cache, error) {
	log := zerowrap.FromCtx(ctx)
	max, err := actionscache.ParseMax(os.Getenv(actionscache.EnvMax))
	if err != nil {
		return nil, err
	}
	if max == 0 {
		log.Info().Msg("the action archive cache is off (" + actionscache.EnvMax + "=0)")
		return nil, nil
	}
	c, err := actionscache.New(actionscache.Config{Dir: filepath.Join(stateDir, "actions-cache"), Max: max, Log: log})
	if err != nil {
		return nil, err
	}
	log.Info().Int64("max_bytes", max).Msg("action archive cache on")
	return c, nil
}

// envCache=0 turns the cache service off.
const envCache = "ERMITE_RUNNER_CACHE"

// envArtifacts=0 turns the results service (artifacts v4) off.
const envArtifacts = "ERMITE_RUNNER_ARTIFACTS"

// setupResults makes the results service of the runner (README, "Artifacts"):
// on by default, off with ERMITE_RUNNER_ARTIFACTS=0. It is served on the cache
// channel of each sandbox and checks the cache's runtime tokens, so it needs
// the cache service; a job gets it only when the server has ci.artifacts.
func setupResults(ctx context.Context, cache *cachesvc.Service) (*resultssvc.Service, error) {
	log := zerowrap.FromCtx(ctx)
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envArtifacts))) {
	case "", "1", "true", "yes":
	case "0", "false", "no":
		log.Info().Msg("the results service is off (" + envArtifacts + "=0): artifact actions fail")
		return nil, nil
	default:
		return nil, fmt.Errorf("%s: %q is not 0 or 1", envArtifacts, os.Getenv(envArtifacts))
	}
	if cache == nil {
		log.Warn().Msg("the results service needs the cache service (" + envCache + "=0): artifact actions fail")
		return nil, nil
	}
	log.Info().Msg("results service on (artifacts v4, on the cache channel; stored by ermite)")
	return resultssvc.New(resultssvc.Config{Auth: cache, Log: log}), nil
}

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

// registerOnFirstRun registers the runner when `run` finds no registration and
// a register token is given in ERMITE_RUNNER_REGISTER_TOKEN or in the file
// named by ERMITE_RUNNER_REGISTER_TOKEN_FILE. Without a token it returns
// host.ErrNotRegistered, as before.
func registerOnFirstRun(ctx context.Context, scfg sandbox.Config, hcfg host.Config) (host.State, error) {
	tok, err := firstRunToken(os.Getenv)
	if err != nil || tok == "" {
		return host.State{}, errors.Join(host.ErrNotRegistered, err)
	}
	if hcfg.URL == "" {
		return host.State{}, fmt.Errorf("%s is not set", host.EnvURL)
	}
	drv, err := buildDriver(ctx, scfg)
	if err != nil {
		return host.State{}, err
	}
	iso := drv.Isolation()
	_ = drv.Close()
	st, err := host.Register(ctx, hcfg, hcfg.Client(hcfg.URL, tok), iso, false)
	if err != nil {
		return host.State{}, fmt.Errorf("first-run registration: %w", err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("runner", st.Name).Str("isolation", string(iso)).Msg("registered on first run; the register token is no longer needed")
	return st, nil
}

// envRegisterTokenFile names a file holding the register token (a container
// secret delivered as a file).
const envRegisterTokenFile = "ERMITE_RUNNER_REGISTER_TOKEN_FILE"

// firstRunToken reads the register token of a first run: the file of
// envRegisterTokenFile when set, else host.EnvRegisterToken. "" means none.
func firstRunToken(getenv func(string) string) (string, error) {
	if p := strings.TrimSpace(getenv(envRegisterTokenFile)); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("%s: %w", envRegisterTokenFile, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(getenv(host.EnvRegisterToken)), nil
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
	if errors.Is(err, host.ErrNotRegistered) {
		// A container service has no separate install step: it registers on its
		// first start when it is given a register token.
		st, err = registerOnFirstRun(ctx, scfg, hcfg)
	}
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
		gf, err := sandbox.ResolveGuestForge(ctx, url, os.Getenv(sandbox.EnvGuestForgeTarget), hcfg.InsecureHTTP, net.DefaultResolver.LookupHost)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ermite-runner:", err)
			return 2
		}
		scfg.GuestForge = gf
		hcfg.GuestForgeOrigin = gf.Origin
	}
	setupExternals(ctx, &scfg)
	cache, archives, err := setupCache(ctx, &scfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 2
	}
	if cache != nil {
		defer func() { _ = cache.CloseAll() }()
	}
	results, err := setupResults(ctx, cache)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 2
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
		Cache:    cache,
		Archives: archives,
		Results:  results,
		Artifacts: func(tok string) ermiteconnect.JobArtifactServiceClient {
			return hcfg.ArtifactClient(url, tok)
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
