// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/drivers"
)

// sandboxCheck runs one stub job in the sandbox chosen by
// ERMITE_RUNNER_ISOLATION: reap leftovers, start, stream the output to stdout
// and stderr, stop. Its exit code is the script's. It is the manual counterpart
// of the integration tests and the smoke test of an installation.
func sandboxCheck(ctx context.Context, args []string) int {
	log := zerowrap.FromCtx(ctx)
	cfg, err := sandbox.LoadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 2
	}
	drv, err := sandbox.Select(ctx, cfg, drivers.Factories(), sandbox.Prober{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 1
	}
	defer func() { _ = drv.Close() }()
	if err := drv.Reap(ctx); err != nil {
		log.Warn().Err(err).Msg("reaping leftovers of a previous run")
	}
	sb, err := drv.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 1
	}
	// Stop runs even when the job context was cancelled.
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if err := sb.Stop(sctx); err != nil {
			fmt.Fprintln(os.Stderr, "ermite-runner: cleanup:", err)
			if errors.Is(err, sandbox.ErrLeaked) {
				fmt.Fprintln(os.Stderr, "ermite-runner: something leaked: run again (the start reaps) or check the state directory")
			}
		}
	}()
	conn, err := sb.Start(ctx, sandbox.JobSpec{JobID: "sandbox-check"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 1
	}
	// Ctrl-C / SIGTERM cancel the job: the executor escalates by itself.
	cancel := make(chan *execpb.Cancel, 1)
	stop := context.AfterFunc(ctx, func() { cancel <- &execpb.Cancel{Reason: "interrupted"} })
	defer stop()
	_, exit, err := execstream.Drive(conn, &execpb.Run{Script: strings.Join(args, " ")}, cancel, func(o *execpb.Output) {
		f := os.Stdout
		if o.Stream == execpb.StreamKind_STREAM_KIND_STDERR {
			f = os.Stderr
		}
		_, _ = f.Write(o.Data)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ermite-runner:", err)
		return 1
	}
	if exit.Error != "" {
		fmt.Fprintln(os.Stderr, "ermite-runner: executor:", exit.Error)
	}
	return int(exit.Code)
}
