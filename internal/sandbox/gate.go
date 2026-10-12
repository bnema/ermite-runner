// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"errors"
	"sync"
)

// BootGate lets Stop abort a Start in progress. Start holds the sandbox's own
// mutex for the whole boot (up to the boot timeout), so Stop could otherwise
// only wait for it; with a gate, Stop first calls Abort, which cancels the
// context Start runs its boot with, and then takes the mutex.
type BootGate struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	aborted bool
}

// ErrAborted is the cause of a boot cancelled by Stop.
var ErrAborted = errors.New("sandbox: start aborted by Stop")

// Begin returns the context to boot with: parent, cancelled by Abort. It fails
// when Abort was already called (Stop before Start).
func (g *BootGate) Begin(parent context.Context) (context.Context, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.aborted {
		return nil, ErrAborted
	}
	ctx, cancel := context.WithCancelCause(parent)
	g.cancel = func() { cancel(ErrAborted) }
	return ctx, nil
}

// End must be called when the boot finished, successfully or not.
func (g *BootGate) End() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		g.cancel()
		g.cancel = nil
	}
}

// Abort cancels a boot in progress and forbids a later Begin. It never blocks.
func (g *BootGate) Abort() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.aborted = true
	if g.cancel != nil {
		g.cancel()
	}
}
