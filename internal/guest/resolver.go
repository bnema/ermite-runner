// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"errors"
	"sync"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"bnema.dev/ermite-runner/internal/executor"
)

// resolver sends ResolveRequest frames to the host and matches the
// ResolveResponse frames that come back (the job token stays in the host).
type resolver struct {
	conn *execstream.Conn
	mu   sync.Mutex
	next int64
	wait map[int64]chan *execpb.ResolveResponse
}

func newResolver(conn *execstream.Conn) *resolver {
	return &resolver{conn: conn, wait: map[int64]chan *execpb.ResolveResponse{}}
}

func (r *resolver) deliver(resp *execpb.ResolveResponse) {
	r.mu.Lock()
	ch := r.wait[resp.Id]
	delete(r.wait, resp.Id)
	r.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

// resolve is executor.Config.Resolver.
func (r *resolver) resolve(ctx context.Context, uses []string) ([]executor.ResolvedAction, error) {
	ch := make(chan *execpb.ResolveResponse, 1)
	r.mu.Lock()
	r.next++
	id := r.next
	r.wait[id] = ch
	r.mu.Unlock()
	forget := func() { r.mu.Lock(); delete(r.wait, id); r.mu.Unlock() }
	req := &execpb.ResolveRequest{Id: id, Uses: uses}
	if err := r.conn.Send(&execpb.Frame{Body: &execpb.Frame_ResolveRequest{ResolveRequest: req}}); err != nil {
		forget()
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		out := make([]executor.ResolvedAction, len(resp.Actions))
		for i, a := range resp.Actions {
			out[i] = executor.ResolvedAction{
				Ref: a.Ref, NameWithOwner: a.NameWithOwner, Path: a.Path, ResolvedSHA: a.ResolvedSha,
				TarballURL: a.TarballUrl, Token: a.Token, Error: a.Error,
			}
		}
		return out, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}
