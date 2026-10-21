// Package client is the SSH transport shared by the ARIES client helpers
// staged into harness containers. It forwards one remote command and the
// caller's streams unchanged; invocation and configuration policy belong to
// each harness-specific adapter.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

const (
	connectTimeout = 5 * time.Second
	keepalive      = 15 * time.Second
)

// Config supplies an already validated destination.
// Invocation/config-file policy belongs to the adapter, not the SSH transport.
type Config struct {
	Address string
	User    string
}

// Run forwards remote verbatim; it never interprets or executes shell syntax.
// It owns stdin and closes it on return. Close must unblock Read, allowing an
// early remote exit or cancellation to join the input copier. Writers must not
// block indefinitely. EOF half-closes input while output continues to drain.
func Run(ctx context.Context, configuration Config, remote string, stdin io.ReadCloser, stdout, stderr io.Writer) (int, error) {
	if stdin == nil {
		return 255, errors.New("SSH stdin is required")
	}
	defer stdin.Close()
	connection, err := Dial(ctx, configuration)
	if err != nil {
		return 255, err
	}
	defer connection.Close()
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancellation()
	client := connection.Client
	session, err := client.NewSession()
	if err != nil {
		return 255, fmt.Errorf("open SSH session: %w", err)
	}
	defer session.Close()
	input, err := session.StdinPipe()
	if err != nil {
		return 255, fmt.Errorf("open SSH input: %w", err)
	}
	inputDone := make(chan struct{})
	var stopping atomic.Bool
	reader := &inputReader{source: stdin, stopping: &stopping}
	var inputError error
	go func() { defer close(inputDone); _, inputError = io.Copy(input, reader); _ = input.Close() }()
	session.Stdout = stdout
	session.Stderr = stderr
	err = session.Run(remote)
	stopping.Store(true)
	_ = session.Close()
	_ = stdin.Close()
	<-inputDone
	if ctx.Err() != nil {
		return 255, ctx.Err()
	}
	if reader.err != nil {
		return 255, fmt.Errorf("read SSH stdin: %w", reader.err)
	}
	// The server may finish before consuming all input. Only ordinary channel
	// closure is expected in that case; other copy failures remain visible.
	if inputError != nil && !errors.Is(inputError, io.EOF) && !errors.Is(inputError, io.ErrClosedPipe) && !errors.Is(inputError, os.ErrClosed) && !errors.Is(inputError, net.ErrClosed) && !errors.Is(inputError, syscall.EPIPE) {
		return 255, fmt.Errorf("write SSH stdin: %w", inputError)
	}
	if err == nil {
		return 0, nil
	}
	var exitError *gossh.ExitError
	if errors.As(err, &exitError) {
		status := exitError.ExitStatus()
		if status >= 0 && status <= 255 {
			return status, nil
		}
	}
	return 255, fmt.Errorf("run SSH command: %w", err)
}

// Connection is one keepalive-supervised SSH connection. A helper that opens
// several sessions over it, as OpenSSH's ControlMaster does, owns Close.
type Connection struct {
	Client *gossh.Client
	stop   func()
	once   sync.Once
}

// Dial connects without client authentication or host-key pinning; the
// bridge accepts only unauthenticated clients on the run's private network.
func Dial(ctx context.Context, configuration Config) (*Connection, error) {
	if configuration.User == "" {
		return nil, errors.New("SSH user is required")
	}
	address := configuration.Address
	dialer := net.Dialer{Timeout: connectTimeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", address, err)
	}
	// The handshake itself takes no context; closing the socket aborts it.
	stopHandshakeCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	_ = connection.SetDeadline(time.Now().Add(connectTimeout))
	clientConfiguration := &gossh.ClientConfig{
		User:            configuration.User,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         connectTimeout,
	}
	sshConnection, channels, requests, err := gossh.NewClientConn(connection, address, clientConfiguration)
	if !stopHandshakeCancellation() && err == nil {
		err = ctx.Err()
		_ = sshConnection.Close()
	}
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("establish SSH connection: %w", err)
	}
	_ = connection.SetDeadline(time.Time{})
	client := gossh.NewClient(sshConnection, channels, requests)
	return &Connection{Client: client, stop: startKeepalive(client, connection)}, nil
}

// Close stops keepalive and closes the connection; it is safe to repeat.
func (connection *Connection) Close() error {
	connection.once.Do(connection.stop)
	return nil
}

// Capture source failures separately from writes rejected by an early remote
// exit. Closing our owned source after completion is not an input failure.
type inputReader struct {
	source   io.Reader
	stopping *atomic.Bool
	err      error
}

func (r *inputReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && !r.stopping.Load() {
		r.err = err
	}
	return n, err
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
