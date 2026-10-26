// SPDX-License-Identifier: MIT
package guest

import (
	"bnema.dev/ermite-runner/internal/deps"
	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/sandbox"
	"context"
	"io"
	"strings"
	"time"
)

// depsLog sends the command output as log lines with the clone credentials
// masked, including a secret split across two writes.
type depsLog struct {
	conn *execstream.Conn
	m    *streamMasker
}

func newDepsLog(c *execstream.Conn, tokens ...string) depsLog {
	l := depsLog{conn: c}
	var secrets []string
	for _, token := range tokens {
		secrets = append(secrets, deps.Secrets(token)...)
	}
	l.m = newStreamMasker(l.send, secrets...)
	return l
}

func (w depsLog) Write(p []byte) (int, error) { return w.m.Write(p) }
func (w depsLog) Close() error                { return w.m.Close() }

func (w depsLog) send(text string) error {
	for _, line := range strings.Split(text, "\n") {
		for _, piece := range execstream.SplitLine(line, execstream.MaxLogLine) {
			if err := w.conn.Send(&execpb.Frame{Body: &execpb.Frame_Log{Log: &execpb.Log{Step: 1, Text: piece, UnixNano: time.Now().UnixNano()}}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// depsEnv holds what runDeps calls out to, so tests can replace it.
type depsEnv struct {
	run   func(context.Context, *execpb.DepsRequest, io.Writer, string) (map[string][]byte, error)
	relay func(context.Context, *execpb.JobSpec) cacheRelay
}

func newDepsEnv(target Target) depsEnv {
	return depsEnv{run: deps.Run, relay: func(ctx context.Context, spec *execpb.JobSpec) cacheRelay {
		return startCacheRelay(ctx, target, spec, nil)
	}}
}

func runDeps(parent context.Context, c *execstream.Conn, r *execpb.DepsRequest, env depsEnv) int {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		for {
			f, err := c.Recv()
			if err != nil || f.GetCancel() != nil {
				cancel()
				return
			}
		}
	}()
	_ = c.Send(&execpb.Frame{Body: &execpb.Frame_StepStarted{StepStarted: &execpb.StepStarted{Number: 1, Name: "Update dependencies", UnixNano: time.Now().UnixNano()}}})
	log := newDepsLog(c, r.Token, r.CacheToken)
	// The same relay serves the registry mirror and the optional deps cache.
	relay := env.relay(ctx, &execpb.JobSpec{CachePort: r.RegistryPort, CacheToken: r.CacheToken, CacheMode: r.CacheMode})
	// Large temporary data goes to the scratch volume; /tmp is RAM in a VM.
	files, err := env.run(ctx, r, log, sandbox.GuestScratch)
	relay.close()
	_ = log.Close()
	result := &execpb.DepsResult{}
	if err != nil {
		result.Error = err.Error()
	} else {
		for p, b := range files {
			for offset := 0; ; {
				end := min(offset+deps.MaxChunk, len(b))
				if sendErr := c.Send(&execpb.Frame{Body: &execpb.Frame_DepsFileChunk{DepsFileChunk: &execpb.DepsFileChunk{Path: p, Offset: uint64(offset), Data: b[offset:end], Last: end == len(b)}}}); sendErr != nil {
					return 1
				}
				offset = end
				if offset == len(b) {
					break
				}
			}
		}
		result.FileCount = uint32(len(files))
	}
	_ = c.Send(&execpb.Frame{Body: &execpb.Frame_DepsResult{DepsResult: result}})
	if err != nil {
		return 1
	}
	return 0
}
