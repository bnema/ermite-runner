// SPDX-License-Identifier: MIT

package execstream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf)
	in := []*execpb.Frame{
		{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{Version: "v", Sandbox: "vm"}}},
		{Body: &execpb.Frame_Output{Output: &execpb.Output{Stream: execpb.StreamKind_STREAM_KIND_STDERR, Data: []byte("x\x00y")}}},
		{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{Code: 3}}},
	}
	for _, f := range in {
		if err := c.Send(f); err != nil {
			t.Fatal(err)
		}
	}
	// One byte at a time: framing must not depend on read sizes.
	r := NewConn(struct {
		io.Reader
		io.Writer
	}{iotest.OneByteReader(&buf), io.Discard})
	for i := range in {
		out, err := r.Recv()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if out.String() != in[i].String() {
			t.Fatalf("frame %d: got %v want %v", i, out, in[i])
		}
	}
	if _, err := r.Recv(); err != io.EOF {
		t.Fatalf("want io.EOF at end, got %v", err)
	}
}

func TestRecvErrors(t *testing.T) {
	huge := make([]byte, 4)
	binary.BigEndian.PutUint32(huge, MaxFrame+1)
	if _, err := NewConn(bytes.NewBuffer(huge)).Recv(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	trunc := []byte{0, 0, 0, 9, 1, 2}
	if _, err := NewConn(bytes.NewBuffer(trunc)).Recv(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body: %v", err)
	}
	if _, err := NewConn(bytes.NewBuffer([]byte{0, 0})).Recv(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated header: %v", err)
	}
	garbage := []byte{0, 0, 0, 3, 0xff, 0xff, 0xff}
	if _, err := NewConn(bytes.NewBuffer(garbage)).Recv(); err == nil {
		t.Fatal("garbage payload accepted")
	}
}

func TestSendTooLarge(t *testing.T) {
	c := NewConn(struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(nil), io.Discard})
	err := c.Send(&execpb.Frame{Body: &execpb.Frame_Output{Output: &execpb.Output{Data: make([]byte, MaxFrame)}}})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestDrive(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	go func() {
		defer func() { _ = guest.Close() }()
		g := NewConn(guest)
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{Sandbox: "container"}}})
		f, err := g.Recv()
		if err != nil || f.GetRun() == nil || f.GetRun().Script != "echo hi" {
			return
		}
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Output{Output: &execpb.Output{Data: []byte("hi\n")}}})
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{Code: 7}}})
	}()
	var got []byte
	hello, exit, err := Drive(host, &execpb.Run{Script: "echo hi"}, nil, func(o *execpb.Output) { got = append(got, o.Data...) })
	if err != nil {
		t.Fatal(err)
	}
	if hello.Sandbox != "container" || exit.Code != 7 || string(got) != "hi\n" {
		t.Fatalf("hello=%v exit=%v out=%q", hello, exit, got)
	}
}

func TestDriveCancelSendsCancel(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	gotCancel := make(chan bool, 1)
	go func() {
		defer func() { _ = guest.Close() }()
		g := NewConn(guest)
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{}}})
		_, _ = g.Recv() // run
		f, err := g.Recv()
		gotCancel <- err == nil && f.GetCancel() != nil && f.GetCancel().Reason == "cancelled by the user"
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{Code: 130}}})
	}()
	cancel := make(chan *execpb.Cancel, 1)
	time.AfterFunc(50*time.Millisecond, func() { cancel <- &execpb.Cancel{Reason: "cancelled by the user"} })
	_, exit, err := Drive(host, &execpb.Run{}, cancel, nil)
	if err != nil || exit.Code != 130 {
		t.Fatalf("exit=%v err=%v", exit, err)
	}
	if !<-gotCancel {
		t.Fatal("guest did not receive a Cancel frame")
	}
}

func TestDriveHelloMissing(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	go func() {
		defer func() { _ = guest.Close() }()
		_ = NewConn(guest).Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
	}()
	if _, _, err := Drive(host, &execpb.Run{}, nil, nil); err == nil {
		t.Fatal("want error when the first frame is not hello")
	}
}

func TestDriveJobDeliversJobFrames(t *testing.T) {
	host, guest := net.Pipe()
	go func() {
		g := NewConn(guest)
		_ = g.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{Version: "v"}}})
		r, _ := g.Recv()
		if r.GetRun().GetJob().GetJobKey() != "build" {
			t.Errorf("run: %v", r)
		}
		for _, b := range []*execpb.Frame{
			{Body: &execpb.Frame_StepStarted{StepStarted: &execpb.StepStarted{Number: 1, Name: "Set up job"}}},
			{Body: &execpb.Frame_Log{Log: &execpb.Log{Step: 1, Text: "hello", UnixNano: 5}}},
			{Body: &execpb.Frame_StepCompleted{StepCompleted: &execpb.StepCompleted{Number: 1, Conclusion: execpb.StepConclusion_STEP_CONCLUSION_SUCCESS}}},
			{Body: &execpb.Frame_JobResult{JobResult: &execpb.JobResult{Conclusion: execpb.StepConclusion_STEP_CONCLUSION_SUCCESS, Outputs: map[string]string{"a": "b"}}}},
			{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}},
		} {
			_ = g.Send(b)
		}
	}()
	var got []string
	_, exit, err := DriveJob(host, &execpb.Run{Job: &execpb.JobSpec{JobKey: "build"}}, nil, JobHandler{
		StepStarted:   func(s *execpb.StepStarted) { got = append(got, "start:"+s.Name) },
		Log:           func(l *execpb.Log) { got = append(got, "log:"+l.Text) },
		StepCompleted: func(s *execpb.StepCompleted) { got = append(got, "done:"+s.Conclusion.String()) },
		JobResult:     func(r *execpb.JobResult) { got = append(got, "result:"+r.Outputs["a"]) },
	})
	if err != nil || exit == nil {
		t.Fatal(err)
	}
	want := "start:Set up job log:hello done:STEP_CONCLUSION_SUCCESS result:b"
	if strings.Join(got, " ") != want {
		t.Fatalf("%v", got)
	}
}

// GO-003: a flood of ResolveRequest frames is answered by one worker, the ones
// beyond the queue with an error and no call, and DriveJob does not return
// before the worker is done (a call in flight is cancelled).
func TestDriveJobResolveBurst(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	g, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	h, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	gc := NewConn(g)
	if err := gc.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{}}}); err != nil {
		t.Fatal(err)
	}
	var running, maxRunning, started atomic.Int32
	release := make(chan struct{})
	var exited atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = DriveJob(h, &execpb.Run{}, nil, JobHandler{Resolve: func(ctx context.Context, r *execpb.ResolveRequest) *execpb.ResolveResponse {
			n := running.Add(1)
			defer running.Add(-1)
			for {
				m := maxRunning.Load()
				if n <= m || maxRunning.CompareAndSwap(m, n) {
					break
				}
			}
			started.Add(1)
			select {
			case <-release:
			case <-ctx.Done():
				if exited.Load() {
					t.Error("the worker outlived DriveJob")
				}
			}
			return &execpb.ResolveResponse{}
		}})
		exited.Store(true)
	}()
	// 50 requests at once while the first call is held.
	for i := int64(1); i <= 50; i++ {
		if err := gc.Send(&execpb.Frame{Body: &execpb.Frame_ResolveRequest{ResolveRequest: &execpb.ResolveRequest{Id: i, Uses: []string{"a/b@v1"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Collect the refusals, which are immediate: until the stream is quiet. The
	// worker may or may not have taken the first request before the queue filled,
	// so the queue and the running call hold 2 or 3 of the 50.
	refused := 0
	for {
		_ = g.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		f, err := gc.Recv()
		if err != nil {
			break
		}
		if r := f.GetResolveResponse(); r != nil && r.Error != "" {
			refused++
		}
	}
	_ = g.SetReadDeadline(time.Time{})
	if refused < 50-1-resolveQueue || refused > 50-resolveQueue {
		t.Fatalf("%d refused of 50, want %d or %d", refused, 50-1-resolveQueue, 50-resolveQueue)
	}
	if maxRunning.Load() != 1 || started.Load() != 1 {
		t.Errorf("running %d, started %d: resolutions are serial", maxRunning.Load(), started.Load())
	}
	// End the session with a call still in flight: it is cancelled and joined.
	_ = g.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DriveJob did not return")
	}
	close(release)
}

// GO-009: the resolution worker is in a Send that cannot finish because the
// guest stopped reading, and the guest ends the session with Exit. DriveJob
// must return, not wait for the write for ever.
func TestDriveJobReturnsWhenTheResolverWriteIsStuck(t *testing.T) {
	old := resolveJoinGrace
	resolveJoinGrace = 50 * time.Millisecond
	defer func() { resolveJoinGrace = old }()
	hostSide, guestSide := net.Pipe() // synchronous: a write needs a reader
	defer func() { _ = guestSide.Close() }()
	gc := NewConn(guestSide)
	go func() {
		_ = gc.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{}}})
		if _, err := gc.Recv(); err != nil { // the Run frame
			return
		}
		_ = gc.Send(&execpb.Frame{Body: &execpb.Frame_ResolveRequest{ResolveRequest: &execpb.ResolveRequest{Id: 1, Uses: []string{"a/b@v1"}}}})
		time.Sleep(100 * time.Millisecond) // the answer is being written: nobody reads it
		_ = gc.Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
	}()
	done := make(chan error, 1)
	go func() {
		_, exit, err := DriveJob(hostSide, &execpb.Run{}, nil, JobHandler{Resolve: func(context.Context, *execpb.ResolveRequest) *execpb.ResolveResponse {
			return &execpb.ResolveResponse{}
		}})
		if err == nil && exit == nil {
			err = errors.New("no exit")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DriveJob is blocked by a write nobody reads")
	}
}

// And a worker that has an answer when the session ends does not write it.
func TestDriveJobDoesNotSendAfterTheSession(t *testing.T) {
	hostSide, guestSide := net.Pipe()
	defer func() { _ = guestSide.Close() }()
	gc := NewConn(guestSide)
	release := make(chan struct{})
	go func() {
		_ = gc.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{}}})
		_, _ = gc.Recv()
		_ = gc.Send(&execpb.Frame{Body: &execpb.Frame_ResolveRequest{ResolveRequest: &execpb.ResolveRequest{Id: 1, Uses: []string{"a/b@v1"}}}})
		_ = gc.Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
		close(release)
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = DriveJob(hostSide, &execpb.Run{}, nil, JobHandler{Resolve: func(ctx context.Context, _ *execpb.ResolveRequest) *execpb.ResolveResponse {
			<-ctx.Done() // returns only when the session ends
			return &execpb.ResolveResponse{}
		}})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DriveJob did not return")
	}
	<-release
}
