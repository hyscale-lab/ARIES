package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh/client"
)

func main() {
	stdin, err := openStdin()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "aries-ssh-client: open owned stdin: %v\n", err)
		os.Exit(255)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := client.Main(ctx, os.Args[1:], stdin, os.Stdout, os.Stderr)
	stop()
	_ = stdin.Close()
	os.Exit(code)
}

// Inherited os.Stdin is often a blocking, unpollable descriptor. Its Close can
// wait forever for a pending Read when the harness keeps its input pipe open.
// Reopen it with a separate file description so Go can cancel reads through its
// poller without changing the parent's descriptor flags. The injected client
// runs in Linux containers with /proc mounted.
func openStdin() (io.ReadCloser, error) {
	file, err := os.OpenFile("/proc/self/fd/0", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENXIO) {
		// Node implements child-process pipes with Unix socket pairs. Sockets
		// cannot be reopened through procfs; FileConn duplicates the owned
		// endpoint into Go's network poller so Close interrupts Read.
		return net.FileConn(os.Stdin)
	}
	return file, err
}
