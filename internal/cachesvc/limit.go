// SPDX-License-Identifier: MIT

package cachesvc

import (
	"net"
	"sync"
)

// limitListener bounds the open connections of a listener (local) and of the
// service (global, shared by all listeners). A slot is taken only once a
// connection has been accepted, so an idle listener holds none; a connection
// that finds no slot is closed at once (never queued or waited for), which makes
// the client retry. Idle and stalled connections are cut by the server's
// timeouts and the progress deadlines of the handlers, which frees their slots.
type limitListener struct {
	net.Listener
	local  chan struct{}
	global chan struct{}
}

func newLimitListener(l net.Listener, perListener int, global chan struct{}) *limitListener {
	return &limitListener{Listener: l, local: make(chan struct{}, perListener), global: global}
}

// take takes a slot of c without waiting.
func take(c chan struct{}) bool {
	select {
	case c <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !take(l.local) {
			_ = c.Close()
			continue
		}
		if !take(l.global) {
			<-l.local
			_ = c.Close()
			continue
		}
		return &limitConn{Conn: c, release: func() { <-l.global; <-l.local }}, nil
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
