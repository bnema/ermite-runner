// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	ermite "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// stubClient answers the calls of the final report. CompleteJob records the
// deadline of its context, then waits for the context to end.
type stubClient struct {
	mu       sync.Mutex
	deadline time.Time
	hang     bool // AppendLog waits for its context to end
}

func (c *stubClient) UpdateJob(context.Context, *connect.Request[ermite.UpdateJobRequest]) (*connect.Response[ermite.UpdateJobResponse], error) {
	return connect.NewResponse(&ermite.UpdateJobResponse{}), nil
}

func (c *stubClient) UploadSummary(context.Context, *connect.Request[ermite.UploadSummaryRequest]) (*connect.Response[ermite.UploadSummaryResponse], error) {
	return connect.NewResponse(&ermite.UploadSummaryResponse{}), nil
}

func (c *stubClient) AppendLog(ctx context.Context, r *connect.Request[ermite.AppendLogRequest]) (*connect.Response[ermite.AppendLogResponse], error) {
	c.mu.Lock()
	hang := c.hang
	c.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return connect.NewResponse(&ermite.AppendLogResponse{NextOffset: r.Msg.Offset + int64(len(r.Msg.Lines))}), nil
}

func (c *stubClient) Heartbeat(context.Context, *connect.Request[ermite.HeartbeatRequest]) (*connect.Response[ermite.HeartbeatResponse], error) {
	return connect.NewResponse(&ermite.HeartbeatResponse{}), nil
}

func (c *stubClient) WatchJob(context.Context, *connect.Request[ermite.WatchJobRequest]) (*connect.ServerStreamForClient[ermite.WatchJobResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (c *stubClient) ResolveActions(context.Context, *connect.Request[ermite.ResolveActionsRequest]) (*connect.Response[ermite.ResolveActionsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (c *stubClient) CompleteJob(ctx context.Context, _ *connect.Request[ermite.CompleteJobRequest]) (*connect.Response[ermite.CompleteJobResponse], error) {
	c.mu.Lock()
	c.deadline, _ = ctx.Deadline()
	c.mu.Unlock()
	return connect.NewResponse(&ermite.CompleteJobResponse{}), nil
}

// The report of a job gets ShutdownReport once the runner's shutdown channel is
// closed, even when the stop timers did not mark the job as stopping (GO-004),
// and CompleteDeadline otherwise.
func TestReportBudgetFollowsTheShutdownChannel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shutdown bool
		want     time.Duration
	}{
		{"running", false, time.Hour},
		{"shutdown", true, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := Timing{
				CallTimeout: 24 * time.Hour, CompleteDeadline: time.Hour, ShutdownReport: 30 * time.Second,
				BackoffMin: time.Millisecond, BackoffMax: time.Millisecond,
			}
			cl := &stubClient{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			j := &jobRun{client: cl, t: tm, ctx: ctx, job: &ermite.Job{JobId: "j1"}, steps: map[int32]*ermite.StepUpdate{},
				cancelCh: make(chan *execpb.Cancel, 1), log: zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &logBuf{}})}
			j.outbox = newOutbox(cl, "j1", tm, func(error) {}, j.log)
			j.outbox.start(ctx)
			shutdown := make(chan struct{})
			if tc.shutdown {
				close(shutdown)
			}
			if j.isStopping() {
				t.Fatal("the job must not be marked as stopping: only the channel is closed")
			}
			start := time.Now()
			j.report(ctx, outcome{conclusion: ermite.CiConclusion_CI_CONCLUSION_SUCCESS}, shutdown)
			cl.mu.Lock()
			defer cl.mu.Unlock()
			if cl.deadline.IsZero() {
				t.Fatal("CompleteJob was not called")
			}
			if got := cl.deadline.Sub(start); got > tc.want+time.Second || got < tc.want-time.Second {
				t.Fatalf("budget %v, want about %v", got, tc.want)
			}
		})
	}
}
