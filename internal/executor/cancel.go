// SPDX-License-Identifier: MIT

package executor

import "sync"

// CancelReason says why a job is stopped.
type CancelReason int

// Cancel reasons.
const (
	// CancelUser is a cancellation by the owner or by fail-fast: the job ends
	// cancelled.
	CancelUser CancelReason = iota + 1
	// CancelTimeout is the job timeout: the job ends timed_out.
	CancelTimeout
	// CancelShutdown is the runner stopping: the job ends cancelled with
	// RunnerShutdown set, and the steps that did not start are skipped.
	CancelShutdown
)

// Canceller stops a job. The first Cancel wins; later calls are ignored (the
// server only sends the first reason too).
type Canceller struct {
	once   sync.Once
	ch     chan struct{}
	mu     sync.Mutex
	reason CancelReason
}

// NewCanceller returns a Canceller that has not fired.
func NewCanceller() *Canceller { return &Canceller{ch: make(chan struct{})} }

// Cancel stops the job for reason.
func (c *Canceller) Cancel(reason CancelReason) {
	c.once.Do(func() {
		c.mu.Lock()
		c.reason = reason
		c.mu.Unlock()
		close(c.ch)
	})
}

// Done is closed when the job is cancelled.
func (c *Canceller) Done() <-chan struct{} { return c.ch }

// Reason returns why the job was cancelled; zero before Done is closed.
func (c *Canceller) Reason() CancelReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}
