package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

const (
	connectTimeout = 5 * time.Second
	keepalive      = 15 * time.Second
)

// dial opens the one SSH connection every executor call shares, without
// client authentication or host-key pinning: the bridge accepts only
// unauthenticated clients on the run's private network.
func dial(ctx context.Context, address string) (*gossh.Client, func() error, error) {
	connection, err := (&net.Dialer{Timeout: connectTimeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", address, err)
	}
	// The handshake takes no context; closing the socket aborts it.
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	_ = connection.SetDeadline(time.Now().Add(connectTimeout))
	sshConnection, channels, requests, err := gossh.NewClientConn(connection, address, &gossh.ClientConfig{
		User: lockedUsername, HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: connectTimeout,
	})
	if !stopCancellation() && err == nil {
		_ = sshConnection.Close()
		err = ctx.Err()
	}
	if err != nil {
		_ = connection.Close()
		return nil, nil, fmt.Errorf("establish SSH connection: %w", err)
	}
	_ = connection.SetDeadline(time.Time{})
	client := gossh.NewClient(sshConnection, channels, requests)
	stop := startKeepalive(client, connection)
	var once sync.Once
	return client, func() error { once.Do(stop); return nil }, nil
}

func startKeepalive(client *gossh.Client, connection net.Conn) func() {
	done := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(keepalive)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// Bound an unanswered request; SendRequest itself has no timeout.
				_ = connection.SetDeadline(time.Now().Add(3 * keepalive))
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					_ = client.Close()
					return
				}
				_ = connection.SetDeadline(time.Time{})
			}
		}
	}()
	return func() { close(done); _ = client.Close(); <-joined }
}

// OpenStdin reopens inherited stdin so Close can interrupt a pending Read.
// Inherited os.Stdin is often a blocking, unpollable descriptor whose Close
// waits forever for a pending Read while Codex keeps its input pipe open. A
// separate file description lets Go cancel reads through its poller without
// changing the parent's descriptor flags. The helper runs in a Linux container
// with /proc mounted.
func OpenStdin() (io.ReadCloser, error) {
	file, err := os.OpenFile("/proc/self/fd/0", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENXIO) {
		// A socket cannot be reopened through procfs; FileConn duplicates the
		// owned endpoint into Go's network poller so Close interrupts Read.
		return net.FileConn(os.Stdin)
	}
	return file, err
}
