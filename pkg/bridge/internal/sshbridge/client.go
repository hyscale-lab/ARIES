package sshbridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// ClientConfig contains already validated transport inputs. Each concrete
// bridge retains ownership of its argv, file, and known-hosts format policies.
type ClientConfig struct {
	Address, User     string
	Signer            ssh.Signer
	HostKey           ssh.PublicKey
	ConnectTimeout    time.Duration
	KeepaliveInterval time.Duration
}

// RunClient forwards one exact exec request with strict Ed25519 host identity.
// Stdin remains caller-owned: remote exit and cancellation never wait for its
// EOF or close it. The caller must release a blocked reader when finished so
// the independent input copier can exit.
func RunClient(ctx context.Context, configuration ClientConfig, remote string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if configuration.Address == "" || configuration.User == "" || configuration.Signer == nil || configuration.HostKey == nil || configuration.HostKey.Type() != ssh.KeyAlgoED25519 || configuration.ConnectTimeout <= 0 || configuration.KeepaliveInterval < 0 {
		return 255, errors.New("SSH client requires an address, user, signer, Ed25519 host key, and positive startup timeout")
	}
	clientConfiguration := &ssh.ClientConfig{
		User: configuration.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(configuration.Signer)},
		HostKeyCallback: func(host string, _ net.Addr, presented ssh.PublicKey) error {
			if host != configuration.Address || !bytes.Equal(presented.Marshal(), configuration.HostKey.Marshal()) {
				return errors.New("strict SSH host key verification failed")
			}
			return nil
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}, Timeout: configuration.ConnectTimeout,
	}
	dialer := net.Dialer{Timeout: configuration.ConnectTimeout}
	connection, err := dialer.DialContext(ctx, "tcp", configuration.Address)
	if err != nil {
		return clientFailure(ctx, "connect SSH bridge", err)
	}
	defer connection.Close()
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancellation()
	if err := connection.SetDeadline(time.Now().Add(configuration.ConnectTimeout)); err != nil {
		return clientFailure(ctx, "set SSH startup deadline", err)
	}
	sshConnection, channels, requests, err := ssh.NewClientConn(connection, configuration.Address, clientConfiguration)
	if err != nil {
		return clientFailure(ctx, "establish SSH connection", err)
	}
	client := ssh.NewClient(sshConnection, channels, requests)
	defer client.Close()
	stopKeepalive := startClientKeepalive(client, configuration.KeepaliveInterval)
	defer stopKeepalive()
	session, err := client.NewSession()
	if err != nil {
		return clientFailure(ctx, "open SSH session", err)
	}
	defer session.Close()
	remoteStdin, err := session.StdinPipe()
	if err != nil {
		return clientFailure(ctx, "open SSH stdin", err)
	}
	session.Stdout, session.Stderr = stdout, stderr
	if err := session.Start(remote); err != nil {
		return clientFailure(ctx, "start SSH command", err)
	}
	// Preserve a fast remote's buffered exit result even if it has already
	// closed the connection while the startup deadline is being cleared.
	if err := connection.SetDeadline(time.Time{}); err != nil && !errors.Is(err, net.ErrClosed) {
		return clientFailure(ctx, "clear SSH startup deadline", err)
	}
	inputDone := make(chan error, 1)
	go func() {
		var copyErr error
		if stdin != nil {
			_, copyErr = io.Copy(remoteStdin, stdin)
		}
		// Publish the result before EOF can let the remote report success.
		inputDone <- copyErr
		_ = remoteStdin.Close()
	}()
	err = session.Wait()
	if ctx.Err() != nil {
		return 255, ctx.Err()
	}
	if err == nil {
		select {
		case copyErr := <-inputDone:
			if copyErr != nil && copyErr != io.EOF {
				return 255, fmt.Errorf("copy SSH stdin: %w", copyErr)
			}
		default:
			// A remote exit does not wait for caller-owned input to finish.
		}
		return 0, nil
	}
	var exitError *ssh.ExitError
	if errors.As(err, &exitError) {
		if status := exitError.ExitStatus(); status >= 0 && status <= 255 {
			return status, nil
		}
	}
	return 255, fmt.Errorf("wait for SSH command: %w", err)
}

func startClientKeepalive(client *ssh.Client, interval time.Duration) func() {
	if interval == 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					failures++
					if failures >= 3 {
						_ = client.Close()
						return
					}
				} else {
					failures = 0
				}
			}
		}
	}()
	return func() { close(done) }
}

func clientFailure(ctx context.Context, operation string, err error) (int, error) {
	if ctx.Err() != nil {
		return 255, ctx.Err()
	}
	return 255, fmt.Errorf("%s: %w", operation, err)
}
