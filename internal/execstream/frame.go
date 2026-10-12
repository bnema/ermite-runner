// SPDX-License-Identifier: MIT

// Package execstream is the framed protobuf stream between the host process of
// ermite-runner and the executor running inside a sandbox.
//
// Wire format: each frame is a 4-byte big-endian length followed by that many
// bytes of a serialized execpb.Frame (see exec.proto). A frame is at most
// MaxFrame bytes; longer output is split by the sender. The stream is
// transport-agnostic: vsock in a micro-VM, a Unix socket in a container.
package execstream

//go:generate go tool buf generate ../.. --template ../../buf.gen.yaml

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// MaxFrame is the largest accepted serialized frame.
// It leaves room for the largest job result: outputs of 1 MiB in total (the
// limit of ermite) plus the overhead of the encoding.
const MaxFrame = 2 << 20

// ChunkSize is the largest output payload placed in one Output frame.
const ChunkSize = 32 * 1024

// ErrFrameTooLarge is returned for a frame above MaxFrame, on either side.
var ErrFrameTooLarge = errors.New("execstream: frame too large")

// Conn reads and writes frames on a byte stream. Send is safe for concurrent
// use; Recv must be called from one goroutine.
type Conn struct {
	rw  io.ReadWriter
	br  *bufio.Reader
	wmu sync.Mutex
}

// NewConn wraps a byte stream.
func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{rw: rw, br: bufio.NewReaderSize(rw, 64*1024)}
}

// Send writes one frame.
func (c *Conn) Send(f *execpb.Frame) error {
	b, err := proto.Marshal(f)
	if err != nil {
		return fmt.Errorf("execstream: marshal: %w", err)
	}
	if len(b) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rw.Write(buf)
	return err
}

// Recv reads one frame. It returns io.EOF only on a clean end of stream between
// frames; a stream cut inside a frame yields io.ErrUnexpectedEOF.
func (c *Conn) Recv() (*execpb.Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return nil, ErrFrameTooLarge
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.br, b); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	f := &execpb.Frame{}
	if err := proto.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("execstream: unmarshal: %w", err)
	}
	return f, nil
}

// Drive is the host side of a session: it waits for the executor's Hello, sends
// run, passes every Output frame to onOutput, and returns the Exit frame.
//
// cancel, when non-nil, asks the executor to stop the script: the first value
// received is sent as one Cancel frame (the executor escalates SIGINT, SIGTERM,
// SIGKILL by itself); closing the channel without a value sends nothing. It is
// deliberately not a context: cancelling the session's context would also abort
// the reads that deliver the executor's Exit. Drive keeps waiting for Exit after
// a Cancel; the caller bounds that wait by stopping the sandbox, which closes
// the stream and makes Drive return an error.
func Drive(rw io.ReadWriter, run *execpb.Run, cancel <-chan *execpb.Cancel, onOutput func(*execpb.Output)) (*execpb.Hello, *execpb.Exit, error) {
	return DriveJob(rw, run, cancel, JobHandler{Output: onOutput})
}

// JobHandler receives the frames of a session. Nil functions ignore their
// frames. The functions are called from the goroutine of DriveJob, in the
// order of the stream.
type JobHandler struct {
	Hello         func(*execpb.Hello)
	Output        func(*execpb.Output)
	StepStarted   func(*execpb.StepStarted)
	StepCompleted func(*execpb.StepCompleted)
	Log           func(*execpb.Log)
	JobResult     func(*execpb.JobResult)
}

// DriveJob is Drive for a session that carries a job: besides Output frames it
// delivers the step, log and result frames to h.
func DriveJob(rw io.ReadWriter, run *execpb.Run, cancel <-chan *execpb.Cancel, h JobHandler) (*execpb.Hello, *execpb.Exit, error) {
	c := NewConn(rw)
	first, err := c.Recv()
	if err != nil {
		return nil, nil, fmt.Errorf("execstream: waiting for hello: %w", err)
	}
	hello := first.GetHello()
	if hello == nil {
		return nil, nil, fmt.Errorf("execstream: first frame is not hello")
	}
	if h.Hello != nil {
		h.Hello(hello)
	}
	if err := c.Send(&execpb.Frame{Body: &execpb.Frame_Run{Run: run}}); err != nil {
		return hello, nil, fmt.Errorf("execstream: sending run: %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	if cancel != nil {
		go func() {
			select {
			case cn, ok := <-cancel:
				if ok {
					if cn == nil {
						cn = &execpb.Cancel{}
					}
					_ = c.Send(&execpb.Frame{Body: &execpb.Frame_Cancel{Cancel: cn}})
				}
			case <-done:
			}
		}()
	}

	for {
		f, err := c.Recv()
		if err != nil {
			return hello, nil, fmt.Errorf("execstream: reading: %w", err)
		}
		switch b := f.Body.(type) {
		case *execpb.Frame_Output:
			if h.Output != nil {
				h.Output(b.Output)
			}
		case *execpb.Frame_StepStarted:
			if h.StepStarted != nil {
				h.StepStarted(b.StepStarted)
			}
		case *execpb.Frame_StepCompleted:
			if h.StepCompleted != nil {
				h.StepCompleted(b.StepCompleted)
			}
		case *execpb.Frame_Log:
			if h.Log != nil {
				h.Log(b.Log)
			}
		case *execpb.Frame_JobResult:
			if h.JobResult != nil {
				h.JobResult(b.JobResult)
			}
		case *execpb.Frame_Exit:
			return hello, b.Exit, nil
		}
	}
}
