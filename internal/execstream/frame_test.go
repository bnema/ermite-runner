// SPDX-License-Identifier: MIT

package execstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
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
