// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"net"
	"testing"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// The resolver of the executor and the Resolve handler of the host meet on the
// stream: requests are answered by id, in any order, and an error of the host
// reaches the caller.
func TestResolverRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	guestSide, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	hostSide, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hostSide.Close() }()
	defer func() { _ = guestSide.Close() }()

	gconn := execstream.NewConn(guestSide)
	res := newResolver(gconn)
	go func() { // the guest's frame reader
		for {
			f, err := gconn.Recv()
			if err != nil {
				return
			}
			if r := f.GetResolveResponse(); r != nil {
				res.deliver(r)
			}
		}
	}()
	// The executor says Hello first, as it does at startup.
	if err := gconn.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{}}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = execstream.DriveJob(hostSide, &execpb.Run{}, nil, execstream.JobHandler{
			Resolve: func(r *execpb.ResolveRequest) *execpb.ResolveResponse {
				if len(r.Uses) == 1 && r.Uses[0] == "bad@v1" {
					return &execpb.ResolveResponse{Error: "no"}
				}
				out := &execpb.ResolveResponse{}
				for _, u := range r.Uses {
					out.Actions = append(out.Actions, &execpb.ResolvedAction{Ref: u, Token: "t-" + u, ResolvedSha: "sha"})
				}
				return out
			},
		})
	}()
	// DriveJob sends Run after Hello; the guest ignores it.
	got, err := res.resolve(context.Background(), []string{"a/b@v1", "c/d@v2"})
	if err != nil || len(got) != 2 || got[1].Ref != "c/d@v2" || got[1].Token != "t-c/d@v2" || got[0].ResolvedSHA != "sha" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := res.resolve(context.Background(), []string{"bad@v1"}); err == nil || err.Error() != "no" {
		t.Fatalf("error not passed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = guestSide.Close()
	if _, err := res.resolve(ctx, []string{"x/y@z"}); err == nil {
		t.Error("a closed stream must fail")
	}
	<-done
}
