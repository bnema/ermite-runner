// SPDX-License-Identifier: MIT

package guest

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// newFDConn wraps a connected stream socket (net.FileConn does not know
// AF_VSOCK) as a ReadWriteCloser registered with the runtime poller.
func newFDConn(fd int) (io.ReadWriteCloser, error) {
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "vsock"), nil
}
