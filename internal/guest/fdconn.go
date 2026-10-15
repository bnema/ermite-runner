// SPDX-License-Identifier: MIT

package guest

import (
	"os"

	"golang.org/x/sys/unix"
)

// fdConn is a connected stream socket as a ReadWriteCloser registered with the
// runtime poller (net.FileConn does not know AF_VSOCK). It can shut down its
// write side, which the cache relay passes on from the client.
type fdConn struct{ *os.File }

func newFDConn(fd int) (*fdConn, error) {
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &fdConn{os.NewFile(uintptr(fd), "vsock")}, nil
}

// CloseWrite sends the end of the stream (shutdown(SHUT_WR)) and keeps reading.
func (c *fdConn) CloseWrite() error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}
