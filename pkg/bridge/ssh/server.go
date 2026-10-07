package ssh

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	gossh "golang.org/x/crypto/ssh"
)

func newServerConfig(hostSigner gossh.Signer, authorized gossh.PublicKey) *gossh.ServerConfig {
	configuration := &gossh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(metadata gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			if metadata.User() != lockedUsername || !bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, errors.New("public key rejected")
			}
			return &gossh.Permissions{}, nil
		},
	}
	configuration.AddHostKey(hostSigner)
	return configuration
}

func (session *bridgeSession) serve(ctx context.Context, logger *logrus.Logger) {
	defer session.wait.Done()
	for {
		connection, err := session.listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				logger.WithError(err).Warn("SSH accept failed")
			}
			return
		}
		session.mu.Lock()
		if session.revoked || ctx.Err() != nil {
			session.mu.Unlock()
			_ = connection.Close()
			return
		}
		session.connections[connection] = struct{}{}
		session.wait.Add(1)
		session.mu.Unlock()
		go session.handleConnection(ctx, connection)
	}
}

func (session *bridgeSession) handleConnection(ctx context.Context, connection net.Conn) {
	defer session.wait.Done()
	defer func() {
		_ = connection.Close()
		session.mu.Lock()
		delete(session.connections, connection)
		session.mu.Unlock()
	}()
	if err := connection.SetDeadline(time.Now().Add(lockedConnectTimeout)); err != nil {
		return
	}
	server, channels, requests, err := gossh.NewServerConn(connection, session.configuration)
	if err != nil {
		return
	}
	// Hermes holds one ControlMaster connection open for the whole run and
	// multiplexes every later command onto it, so the handshake deadline must
	// not survive into the session channels.
	if err := connection.SetDeadline(time.Time{}); err != nil {
		_ = server.Close()
		return
	}
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var connectionWorkers sync.WaitGroup
	connectionWorkers.Add(2)
	defer func() {
		cancel()
		_ = server.Close()
		connectionWorkers.Wait()
	}()
	go func() {
		defer connectionWorkers.Done()
		_ = server.Wait()
		cancel()
	}()
	go func() {
		defer connectionWorkers.Done()
		serveGlobalRequests(requests)
	}()
	for incoming := range channels {
		if incoming.ChannelType() != "session" || len(incoming.ExtraData()) != 0 {
			_ = incoming.Reject(gossh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			continue
		}
		session.mu.Lock()
		if session.revoked || connectionCtx.Err() != nil {
			session.mu.Unlock()
			_ = channel.Close()
			continue
		}
		session.wait.Add(1)
		session.mu.Unlock()
		go func() {
			defer session.wait.Done()
			defer channel.Close()
			session.handleSession(connectionCtx, channel, channelRequests)
		}()
	}
}

func serveGlobalRequests(requests <-chan *gossh.Request) {
	for request := range requests {
		accepted := request.Type == "keepalive@openssh.com" && len(request.Payload) == 0
		if request.WantReply {
			_ = request.Reply(accepted, nil)
		}
	}
}

func (session *bridgeSession) handleSession(ctx context.Context, channel gossh.Channel, requests <-chan *gossh.Request) {
	policy := session.dialect.Policy()
	for request := range requests {
		audit := requestAudit{requestType: request.Type, wantReply: request.WantReply, payload: bytes.Clone(request.Payload)}
		if request.Type != "exec" {
			if request.WantReply {
				_ = session.reply(request, false)
			}
			if policy.UnsupportedRequests == RejectAndContinue {
				session.logRequestFailure(audit, policy.InvalidOperationClass, "unsupported", "channel request type is not exec")
				continue
			}
			session.logRejected(audit, policy.InvalidOperationClass)
			return
		}
		if !request.WantReply {
			session.logRejected(audit, policy.InvalidOperationClass)
			return
		}
		var payload struct{ Command string }
		if err := gossh.Unmarshal(request.Payload, &payload); err != nil {
			_ = session.reply(request, false)
			session.logRejected(audit, policy.InvalidOperationClass)
			return
		}
		audit.remoteCommand = payload.Command
		prepared, refusal := session.dialect.Prepare(payload.Command, session.sandbox.Workdir())
		if refusal != nil {
			_ = session.reply(request, false)
			session.logRequestFailure(audit, refusal.OperationClass, refusal.Status, refusal.Message)
			return
		}
		if prepared.Action != Execute && prepared.Action != DrainOnly {
			_ = session.reply(request, false)
			session.logRejected(audit, policy.InvalidOperationClass)
			return
		}
		if err := session.reply(request, true); err != nil {
			session.logRequestFailure(audit, prepared.RefusalClass, "failed", "SSH accept reply failed")
			return
		}
		exitCode := session.execute(ctx, channel, prepared, audit)
		_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
		return
	}
}

// reply routes through session.replyRequest, which Start always populates and
// tests override to observe accept/reject outcomes.
func (session *bridgeSession) reply(request *gossh.Request, accepted bool) error {
	if session.replyRequest != nil {
		return session.replyRequest(request, accepted)
	}
	return request.Reply(accepted, nil)
}
