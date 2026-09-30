// Package sshserve is the SSH transport the two SSH bridges share: session
// keys, a server that accepts exactly one client key for the locked user, and
// the counters that meter a channel's streams. What a channel request means
// stays in each bridge.
package sshserve

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	// LockedUsername is the only user the server authenticates.
	LockedUsername = "aries"
	// MaxRecordedInputBytes bounds the stdin a RecordedInput retains for the audit.
	MaxRecordedInputBytes = 16 << 20

	handshakeTimeout = 5 * time.Second
)

// GenerateSessionKeys generates a fresh host key and client key for one session. The client
// key is returned as PKCS#8 PEM for the harness, and as the public key the
// server authorizes.
func GenerateSessionKeys() (host ssh.Signer, clientPEM []byte, authorized ssh.PublicKey, err error) {
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate SSH host key: %w", err)
	}
	host, err = ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create SSH host signer: %w", err)
	}
	_, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate SSH client key: %w", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPrivate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create SSH client signer: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(clientPrivate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal SSH client key: %w", err)
	}
	return host, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), clientSigner.PublicKey(), nil
}

// Server accepts connections on one listener until Revoke.
type Server struct {
	listener      net.Listener
	configuration *ssh.ServerConfig
	cancel        context.CancelFunc
	wait          *sync.WaitGroup
	handle        func(context.Context, ssh.Channel, <-chan *ssh.Request)

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	revokeOnce  sync.Once
}

// Serve starts accepting on listener. Every session channel is handed to
// handle; every goroutine it starts is counted in wait, so a caller that waits
// after Revoke knows no handler is still running. name prefixes the accept
// failure logged to logger.
func Serve(listener net.Listener, host ssh.Signer, authorized ssh.PublicKey, wait *sync.WaitGroup,
	logger *logrus.Logger, name string, handle func(context.Context, ssh.Channel, <-chan *ssh.Request)) *Server {
	configuration := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != LockedUsername || !bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, errors.New("public key rejected")
			}
			return &ssh.Permissions{}, nil
		},
	}
	configuration.AddHostKey(host)
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		listener: listener, configuration: configuration, cancel: cancel, wait: wait, handle: handle,
		connections: make(map[net.Conn]struct{}),
	}
	wait.Add(1)
	go server.serve(ctx, logger, name)
	return server
}

// Revoke stops accepting, cancels every running handler, and closes every
// open connection.
func (server *Server) Revoke() {
	server.revokeOnce.Do(func() {
		server.cancel()
		_ = server.listener.Close()
		server.mu.Lock()
		for connection := range server.connections {
			_ = connection.Close()
		}
		server.mu.Unlock()
	})
}

func (server *Server) serve(ctx context.Context, logger *logrus.Logger, name string) {
	defer server.wait.Done()
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				logger.WithError(err).Warn(name + " SSH accept failed")
			}
			return
		}
		server.mu.Lock()
		server.connections[connection] = struct{}{}
		server.mu.Unlock()
		server.wait.Add(1)
		go server.handleConnection(ctx, connection)
	}
}

func (server *Server) handleConnection(ctx context.Context, connection net.Conn) {
	defer server.wait.Done()
	defer func() {
		_ = connection.Close()
		server.mu.Lock()
		delete(server.connections, connection)
		server.mu.Unlock()
	}()
	_ = connection.SetDeadline(time.Now().Add(handshakeTimeout))
	serverConn, channels, requests, err := ssh.NewServerConn(connection, server.configuration)
	if err != nil {
		return
	}
	defer serverConn.Close()
	// Hermes holds one ControlMaster connection open for the whole run and
	// multiplexes every later command onto it, so the handshake deadline must
	// not survive into the session channels.
	_ = connection.SetDeadline(time.Time{})
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = serverConn.Wait()
		cancel()
	}()
	go serveGlobalRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" || len(incoming.ExtraData()) != 0 {
			_ = incoming.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			continue
		}
		server.wait.Add(1)
		go func() {
			defer server.wait.Done()
			defer channel.Close()
			server.handle(connectionCtx, channel, channelRequests)
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

// ByteCounter passes a stream through and counts its bytes; set Reader or Writer.
type ByteCounter struct {
	Reader io.Reader
	Writer io.Writer
	n      atomic.Int64
}

func (counter *ByteCounter) Read(content []byte) (int, error) {
	n, err := counter.Reader.Read(content)
	counter.n.Add(int64(n))
	return n, err
}

func (counter *ByteCounter) Write(content []byte) (int, error) {
	n, err := counter.Writer.Write(content)
	counter.n.Add(int64(n))
	return n, err
}

// Count is the number of bytes passed so far.
func (counter *ByteCounter) Count() int64 { return counter.n.Load() }

// RecordedInput passes stdin through and retains up to MaxRecordedInputBytes of it for the
// audit. Beyond that the read fails and Record reports the overflow.
type RecordedInput struct {
	Reader io.Reader

	mu       sync.Mutex
	n        int64
	data     bytes.Buffer
	overflow bool
}

func (input *RecordedInput) Read(content []byte) (int, error) {
	n, err := input.Reader.Read(content)
	if n > 0 {
		input.mu.Lock()
		remaining := MaxRecordedInputBytes - input.data.Len()
		if n > remaining {
			input.n += int64(n)
			input.data.Reset()
			input.overflow = true
			input.mu.Unlock()
			return n, fmt.Errorf("SSH stdin exceeds %d bytes", MaxRecordedInputBytes)
		}
		_, _ = input.data.Write(content[:n])
		input.n += int64(n)
		input.mu.Unlock()
	}
	return n, err
}

// Record renders the retained stdin for the structured record: the byte
// count, the text or an omission note, its encoding, the raw bytes, and
// whether the input overflowed. retainedRaw says whether this run writes
// ssh_raw.log, so the note names only an artifact that exists.
func (input *RecordedInput) Record(retainedRaw bool) (count int64, text, encoding string, raw []byte, overflow bool) {
	input.mu.Lock()
	count = input.n
	raw = bytes.Clone(input.data.Bytes())
	overflow = input.overflow
	input.mu.Unlock()
	if bridgekit.SafeStructuredText(raw) {
		return count, string(raw), "utf-8", raw, overflow
	}
	note := fmt.Sprintf("[binary input omitted; %d bytes not retained]", count)
	if retainedRaw {
		note = fmt.Sprintf("[binary input omitted; %d bytes retained in ssh_raw.log]", count)
	}
	return count, note, "binary-omitted", raw, overflow
}
