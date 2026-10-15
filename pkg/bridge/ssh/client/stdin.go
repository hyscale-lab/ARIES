package client

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

// OpenStdin reopens inherited stdin so Close can interrupt a pending Read.
// Inherited os.Stdin is often a blocking, unpollable descriptor whose Close
// waits forever for a pending Read when the harness keeps its input pipe open.
// A separate file description lets Go cancel reads through its poller without
// changing the parent's descriptor flags. The helpers run in Linux containers
// with /proc mounted.
func OpenStdin() (io.ReadCloser, error) {
	file, err := os.OpenFile("/proc/self/fd/0", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENXIO) {
		// Node implements child-process pipes with Unix socket pairs. Sockets
		// cannot be reopened through procfs; FileConn duplicates the owned
		// endpoint into Go's network poller so Close interrupts Read.
		return net.FileConn(os.Stdin)
	}
	return file, err
}
