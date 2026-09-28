package sshbridge

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// Server owns one SSH listener and every connection and handler it admits.
// Concrete bridges retain their session request grammar and admission policy.
type Server struct {
	listener      net.Listener
	configuration *ssh.ServerConfig
	ctx           context.Context
	cancel        context.CancelFunc

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	started     bool
	revoked     bool
	wait        sync.WaitGroup
	revokeOnce  sync.Once
	done        chan struct{}
}

// NewServer immediately takes ownership of listener, including before Start.
// The supplied keys must be valid; only the aries user and authorized key pass.
func NewServer(listener net.Listener, hostSigner ssh.Signer, authorizedKey ssh.PublicKey) *Server {
	configuration := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != "aries" || !bytes.Equal(key.Marshal(), authorizedKey.Marshal()) {
				return nil, errors.New("public key rejected")
			}
			return &ssh.Permissions{}, nil
		},
	}
	configuration.AddHostKey(hostSigner)
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		listener: listener, configuration: configuration, ctx: ctx, cancel: cancel,
		connections: make(map[net.Conn]struct{}), done: make(chan struct{}),
	}
}

// Start admits session channels once. Calling it after Revoke has no effect.
// Handler contexts end on connection loss or revocation; Wait joins handlers.
func (server *Server) Start(handler func(context.Context, ssh.Channel, <-chan *ssh.Request), logger *logrus.Logger) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started || server.revoked {
		return
	}
	server.started = true
	server.wait.Add(1)
	go server.serve(handler, logger)
}

func (server *Server) Addr() net.Addr { return server.listener.Addr() }

func (server *Server) serve(handler func(context.Context, ssh.Channel, <-chan *ssh.Request), logger *logrus.Logger) {
	defer server.wait.Done()
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			if server.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) && logger != nil {
				logger.WithError(err).Warn("SSH bridge accept failed")
			}
			return
		}
		server.mu.Lock()
		if server.revoked {
			server.mu.Unlock()
			_ = connection.Close()
			return
		}
		server.connections[connection] = struct{}{}
		server.wait.Add(1)
		server.mu.Unlock()
		go server.handleConnection(connection, handler)
	}
}

func (server *Server) handleConnection(connection net.Conn, handler func(context.Context, ssh.Channel, <-chan *ssh.Request)) {
	defer server.wait.Done()
	defer func() {
		_ = connection.Close()
		server.mu.Lock()
		delete(server.connections, connection)
		server.mu.Unlock()
	}()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return
	}
	sshConnection, channels, requests, err := ssh.NewServerConn(connection, server.configuration)
	if err != nil {
		return
	}
	defer sshConnection.Close()
	// A ControlMaster or native executor connection outlives its handshake.
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return
	}
	connectionCtx, cancel := context.WithCancel(server.ctx)
	var background sync.WaitGroup
	background.Add(2)
	defer func() {
		cancel()
		_ = sshConnection.Close()
		background.Wait()
	}()
	go func() {
		defer background.Done()
		_ = sshConnection.Wait()
		cancel()
	}()
	go func() {
		defer background.Done()
		serveGlobalRequests(requests)
	}()
	for incoming := range channels {
		if incoming.ChannelType() != "session" || len(incoming.ExtraData()) != 0 {
			_ = incoming.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			continue
		}
		server.mu.Lock()
		if server.revoked {
			server.mu.Unlock()
			_ = channel.Close()
			return
		}
		server.wait.Add(1)
		server.mu.Unlock()
		go func() {
			defer server.wait.Done()
			defer channel.Close()
			handler(connectionCtx, channel, channelRequests)
		}()
	}
}

func serveGlobalRequests(requests <-chan *ssh.Request) {
	for request := range requests {
		accepted := request.Type == "keepalive@openssh.com" && len(request.Payload) == 0
		if request.WantReply {
			_ = request.Reply(accepted, nil)
		}
	}
}

// Revoke closes admission before canceling handlers and closing all transports.
// It is idempotent and safe on a nil Server during partial-start cleanup.
func (server *Server) Revoke() {
	if server == nil {
		return
	}
	server.revokeOnce.Do(func() {
		server.mu.Lock()
		server.revoked = true
		connections := make([]net.Conn, 0, len(server.connections))
		for connection := range server.connections {
			connections = append(connections, connection)
		}
		server.mu.Unlock()
		server.cancel()
		_ = server.listener.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
		// Admission is closed under the same lock as every Add. One waiter
		// serves all cleanup attempts, including retries after a timeout.
		go func() { server.wait.Wait(); close(server.done) }()
	})
}

// Wait confirms that a revoked server has finished all handlers and transport
// goroutines. A timeout leaves the same completion signal available for retry.
func (server *Server) Wait(ctx context.Context) error {
	if server == nil {
		return nil
	}
	select {
	case <-server.done:
		return nil
	default:
	}
	select {
	case <-server.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
