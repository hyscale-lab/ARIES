package ssh

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
	gossh "golang.org/x/crypto/ssh"
)

func newServerConfig(hostSigner gossh.Signer) *gossh.ServerConfig {
	configuration := &gossh.ServerConfig{NoClientAuth: true}
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
		requestCtx, timings := core.WithCommandTimings(ctx)
		core.MarkCommandTiming(requestCtx, "bridge.received")
		audit := requestAudit{timings: timings, requestType: request.Type, wantReply: request.WantReply, payload: bytes.Clone(request.Payload)}
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
		finishPrepare := core.TimeCommandStage(requestCtx, "bridge.prepare")
		var payload struct{ Command string }
		if err := gossh.Unmarshal(request.Payload, &payload); err != nil {
			finishPrepare()
			_ = session.reply(request, false)
			session.logRejected(audit, policy.InvalidOperationClass)
			return
		}
		audit.remoteCommand = payload.Command
		prepared, refusal := session.dialect.Prepare(payload.Command, session.sandbox.Workdir())
		finishPrepare()
		core.MarkCommandTiming(requestCtx, "bridge.prepared")
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
		finishReply := core.TimeCommandStage(requestCtx, "bridge.accept_reply")
		if err := session.reply(request, true); err != nil {
			finishReply()
			core.MarkCommandTiming(requestCtx, "bridge.accept_reply_failed")
			session.logRequestFailure(audit, prepared.RefusalClass, "failed", "SSH accept reply failed")
			return
		}
		finishReply()
		exitCode, record, raw := session.executeRecord(requestCtx, channel, prepared, audit)
		finishResponse := core.TimeCommandStage(requestCtx, "bridge.send_exit_status")
		_, responseErr := channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
		finishResponse()
		if responseErr == nil {
			core.MarkCommandTiming(requestCtx, "bridge.response_sent")
		} else {
			core.MarkCommandTiming(requestCtx, "bridge.response_send_failed")
		}
		finishClose := core.TimeCommandStage(requestCtx, "bridge.close_channel")
		closeErr := channel.Close()
		finishClose()
		if closeErr == nil {
			core.MarkCommandTiming(requestCtx, "bridge.channel_closed")
		} else {
			core.MarkCommandTiming(requestCtx, "bridge.channel_close_failed")
		}
		if record != nil {
			if responseErr != nil {
				record.ResponseError = "SSH exit-status send failed"
			}
			if closeErr != nil {
				if record.ResponseError != "" {
					record.ResponseError += "; "
				}
				record.ResponseError += "SSH channel close failed"
			}
			session.writeTimedRecord(*record, raw, audit)
		}
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
