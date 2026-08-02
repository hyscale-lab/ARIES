package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRole              = "operator"
	DefaultConnectTimeout    = 20 * time.Second
	DefaultChallengeTimeout  = 5 * time.Second
	DefaultConnectRetryDelay = 500 * time.Millisecond
	maxMessageBytes          = 64 << 20
	defaultEventQueueSize    = 2048
	defaultEventQueueBytes   = 16 << 20
)

var defaultScopes = []string{"operator.write"}

// Transport is the narrow websocket-like surface used by the Gateway client.
type Transport interface {
	Send(context.Context, []byte) error
	Receive(context.Context) ([]byte, error)
	Close() error
}

type DialFunc func(context.Context) (Transport, error)

type EventDisposition uint8

const (
	EventDispositionDelivery EventDisposition = iota
	EventDispositionResponseOnly
)

type Client struct {
	dial             DialFunc
	role             string
	scopes           []string
	token            string
	eventDisposition EventDisposition

	mu             sync.Mutex
	transport      Transport
	sequence       int
	pending        map[string]chan responseDelivery
	eventCh        chan eventDelivery
	eventByteMax   int
	eventCount     int
	eventBytes     int
	readerErr      error
	readerFatal    bool
	reader         context.CancelFunc
	readerFailed   chan struct{}
	connected      *ConnectSummary
	awaitChallenge bool
	generation     uint64
}

type Options struct {
	Role             string
	Scopes           []string
	Token            string
	EventQueueSize   int
	EventQueueBytes  int
	EventDisposition EventDisposition
}

type ConnectOptions struct {
	Timeout          time.Duration
	RetryDelay       time.Duration
	ChallengeTimeout time.Duration
}

// ConnectSummary is the complete public result of authentication. Raw auth,
// challenge, device-token, signature, and nonce material never leaves Client.
type ConnectSummary struct {
	Role   string   `json:"role"`
	Scopes []string `json:"scopes"`
}

func (summary ConnectSummary) HasScope(want string) bool {
	for _, scope := range summary.Scopes {
		if scope == want {
			return true
		}
	}
	return false
}

type responseDelivery struct {
	frame Frame
	err   error
}

type eventDelivery struct {
	frame      Frame
	bytes      int
	generation uint64
}

func New(dial DialFunc, options Options) (*Client, error) {
	if dial == nil {
		return nil, errors.New("gateway dialer is required")
	}
	role := options.Role
	if role == "" {
		role = DefaultRole
	}
	scopes := append([]string(nil), options.Scopes...)
	if len(scopes) == 0 {
		scopes = append([]string(nil), defaultScopes...)
	}
	size := options.EventQueueSize
	if size <= 0 {
		size = defaultEventQueueSize
	}
	queueBytes := options.EventQueueBytes
	if queueBytes <= 0 {
		queueBytes = defaultEventQueueBytes
	}
	if options.EventDisposition != EventDispositionDelivery && options.EventDisposition != EventDispositionResponseOnly {
		return nil, errors.New("gateway event disposition is invalid")
	}
	return &Client{
		dial: dial, role: role, scopes: scopes, token: options.Token,
		eventDisposition: options.EventDisposition,
		pending:          make(map[string]chan responseDelivery), eventCh: make(chan eventDelivery, size), eventByteMax: queueBytes,
		readerFailed: make(chan struct{}),
	}, nil
}

func (client *Client) Connect(ctx context.Context, options ConnectOptions) (ConnectSummary, error) {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	retryDelay := options.RetryDelay
	if retryDelay <= 0 {
		retryDelay = DefaultConnectRetryDelay
	}
	challengeTimeout := options.ChallengeTimeout
	if challengeTimeout <= 0 {
		challengeTimeout = DefaultChallengeTimeout
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Until(deadline))
		first, err := client.connectOnce(attemptCtx, challengeTimeout)
		cancel()
		if err == nil {
			return client.finishConnect(ctx, first)
		}
		lastErr = err
		if ctx.Err() != nil || !time.Now().Add(retryDelay).Before(deadline) {
			break
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ConnectSummary{}, fmt.Errorf("gateway websocket connect failed: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return ConnectSummary{}, fmt.Errorf("gateway websocket connect failed: %w", lastErr)
}

func (client *Client) connectOnce(ctx context.Context, challengeTimeout time.Duration) (Frame, error) {
	client.Close()
	transport, err := client.dial(ctx)
	if err != nil {
		return nil, err
	}
	readerCtx, cancel := context.WithCancel(context.Background())
	client.mu.Lock()
	client.transport = transport
	client.generation++
	generation := client.generation
	client.reader = cancel
	client.readerErr = nil
	client.readerFatal = false
	client.readerFailed = make(chan struct{})
	client.eventCount = 0
	client.eventBytes = 0
	client.eventCh = make(chan eventDelivery, cap(client.eventCh))
	client.awaitChallenge = true
	client.mu.Unlock()
	go client.readConnection(readerCtx, transport, generation)

	challengeCtx, challengeCancel := context.WithTimeout(ctx, challengeTimeout)
	defer challengeCancel()
	first, err := client.RecvEvent(challengeCtx)
	if err != nil {
		client.Close()
		return nil, err
	}
	if first.String("type") != "event" || first.String("event") != "connect.challenge" {
		client.Close()
		return nil, errors.New("expected connect.challenge")
	}
	return first, nil
}

func (client *Client) finishConnect(ctx context.Context, challenge Frame) (ConnectSummary, error) {
	payload := challenge.Map("payload")
	nonce, _ := payload["nonce"].(string)
	if nonce == "" {
		return ConnectSummary{}, errors.New("connect.challenge missing nonce")
	}
	params := map[string]any{
		"minProtocol": 3, "maxProtocol": 4,
		"client": map[string]any{"id": "gateway-client", "version": "0.1", "platform": "go", "mode": "backend"},
		"role":   client.role, "scopes": append([]string(nil), client.scopes...),
		"caps": []any{}, "commands": []any{}, "permissions": map[string]any{},
		"auth": map[string]any{"token": client.token}, "locale": "en-US", "userAgent": "gateway-client/aries/0.1",
	}
	response, err := client.Call(ctx, "connect", params)
	if err != nil {
		return ConnectSummary{}, err
	}
	if response.Bool("ok") {
		payload := response.Map("payload")
		auth := Map(payload["auth"])
		role, _ := auth["role"].(string)
		if role == "" {
			role, _ = payload["role"].(string)
		}
		if role == "" {
			role = client.role
		}
		summary := ConnectSummary{Role: role, Scopes: grantedScopes(payload)}
		client.mu.Lock()
		client.connected = &summary
		client.mu.Unlock()
		return summary, nil
	}
	if pairingRequired(response) {
		return ConnectSummary{}, errors.New("connect failed: gateway pairing is required")
	}
	return ConnectSummary{}, errors.New("connect failed: gateway rejected authentication")
}

func (client *Client) Call(ctx context.Context, method string, params map[string]any) (Frame, error) {
	if strings.TrimSpace(method) == "" {
		return nil, errors.New("gateway method is required")
	}
	id := client.nextID(method)
	reply := make(chan responseDelivery, 1)
	client.mu.Lock()
	transport := client.transport
	generation := client.generation
	if client.readerFatal {
		err := client.readerErr
		client.mu.Unlock()
		return nil, err
	}
	if transport == nil {
		client.mu.Unlock()
		return nil, errors.New("gateway transport is not connected")
	}
	client.pending[id] = reply
	client.mu.Unlock()

	frame := Frame{"type": "req", "id": id, "method": method, "params": params}
	content, err := json.Marshal(frame)
	if err != nil {
		client.removePending(id)
		return nil, fmt.Errorf("marshal gateway request: %w", err)
	}
	if err := transport.Send(ctx, content); err != nil {
		client.removePending(id)
		if fatalErr := client.connectionFatal(generation); fatalErr != nil {
			return nil, fatalErr
		}
		return nil, fmt.Errorf("send gateway request: %w", err)
	}
	select {
	case delivery := <-reply:
		client.removePending(id)
		if fatalErr := client.connectionFatal(generation); fatalErr != nil {
			return nil, fatalErr
		}
		return delivery.frame, delivery.err
	case <-ctx.Done():
		client.removePending(id)
		return nil, ctx.Err()
	}
}

func (client *Client) RecvEvent(ctx context.Context) (Frame, error) {
	for {
		client.mu.Lock()
		readerErr := client.readerErr
		readerFatal := client.readerFatal
		readerFailed := client.readerFailed
		eventCh := client.eventCh
		generation := client.generation
		client.mu.Unlock()
		if readerFatal {
			return nil, readerErr
		}
		select {
		case delivery := <-eventCh:
			frame, err, current := client.finishEventDelivery(delivery, generation)
			if !current {
				continue
			}
			if err != nil {
				return nil, err
			}
			return frame, nil
		default:
		}
		if readerErr != nil {
			return nil, readerErr
		}
		select {
		case delivery := <-eventCh:
			frame, err, current := client.finishEventDelivery(delivery, generation)
			if !current {
				continue
			}
			if err != nil {
				return nil, err
			}
			return frame, nil
		case <-readerFailed:
			// A normal EOF can race with the event that preceded it. Loop once
			// more so an already queued event is observed before the EOF.
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (client *Client) releaseEventLocked(delivery eventDelivery) {
	if delivery.generation != client.generation {
		return
	}
	client.eventCount--
	client.eventBytes -= delivery.bytes
}

func (client *Client) finishEventDelivery(delivery eventDelivery, generation uint64) (Frame, error, bool) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.releaseEventLocked(delivery)
	if client.generation != generation || delivery.generation != generation {
		return nil, nil, false
	}
	if client.readerFatal {
		return nil, client.readerErr, true
	}
	return delivery.frame, nil, true
}

// FatalError reports a generation-fatal protocol/overflow failure without
// treating a normal reader EOF as fatal.
func (client *Client) FatalError() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.readerFatal {
		return client.readerErr
	}
	return nil
}

func (client *Client) Close() error {
	return client.failConnectionFatal(errors.New("gateway connection closed"))
}

func (client *Client) nextID(method string) string {
	clean := strings.NewReplacer(".", "-", " ", "-").Replace(method)
	client.mu.Lock()
	defer client.mu.Unlock()
	client.sequence++
	return fmt.Sprintf("%s-%d-%d", clean, client.sequence, time.Now().UnixMilli())
}

func (client *Client) removePending(id string) {
	client.mu.Lock()
	delete(client.pending, id)
	client.mu.Unlock()
}

func (client *Client) readLoop(ctx context.Context, transport Transport) {
	client.mu.Lock()
	generation := client.generation
	client.mu.Unlock()
	client.readConnection(ctx, transport, generation)
}

func (client *Client) readConnection(ctx context.Context, transport Transport, generation uint64) {
	for {
		content, err := transport.Receive(ctx)
		if err != nil {
			readerErr := fmt.Errorf("gateway reader stopped: %w", err)
			if isWebSocketProtocolError(err) {
				client.failTransportFatal(transport, readerErr)
			} else {
				client.failTransport(transport, readerErr)
			}
			return
		}
		if len(content) > maxMessageBytes {
			client.failTransportFatal(transport, errors.New("gateway message exceeded size bound"))
			return
		}
		var frame Frame
		if err := json.Unmarshal(content, &frame); err != nil {
			client.failTransportFatal(transport, errors.New("gateway returned malformed JSON"))
			return
		}
		if frame.String("type") == "res" {
			client.mu.Lock()
			if client.transport != transport || client.generation != generation {
				client.mu.Unlock()
				return
			}
			if id := frame.String("id"); id != "" {
				reply := client.pending[id]
				if reply != nil {
					select {
					case reply <- responseDelivery{frame: frame}:
						client.mu.Unlock()
					default:
						termination, ok := client.terminateConnectionLocked(transport, errors.New("gateway response queue overflow"), true)
						client.mu.Unlock()
						if ok {
							finishConnectionTermination(termination)
						}
						return
					}
				} else {
					client.mu.Unlock()
				}
			} else {
				client.mu.Unlock()
			}
			// Responses are point-to-point protocol data. Missing, stale, late,
			// and duplicate responses are discarded rather than retained or
			// exposed as events, where auth/device-token payloads could leak.
			continue
		}
		client.mu.Lock()
		if client.transport != transport || client.generation != generation {
			client.mu.Unlock()
			return
		}
		challenge := frame.String("event") == "connect.challenge"
		if challenge {
			if !client.awaitChallenge {
				client.mu.Unlock()
				continue
			}
			client.awaitChallenge = false
		} else if client.eventDisposition == EventDispositionResponseOnly && !client.awaitChallenge {
			client.mu.Unlock()
			continue
		}
		if client.eventCount >= cap(client.eventCh) || client.eventBytes+len(content) > client.eventByteMax {
			termination, ok := client.terminateConnectionLocked(transport, errors.New("gateway event queue overflow"), true)
			client.mu.Unlock()
			if ok {
				finishConnectionTermination(termination)
			}
			return
		}
		eventCh := client.eventCh
		delivery := eventDelivery{frame: frame, bytes: len(content), generation: generation}
		select {
		case eventCh <- delivery:
			client.eventCount++
			client.eventBytes += len(content)
			client.mu.Unlock()
		default:
			termination, ok := client.terminateConnectionLocked(transport, errors.New("gateway event queue overflow"), true)
			client.mu.Unlock()
			if ok {
				finishConnectionTermination(termination)
			}
			return
		}
	}
}

func (client *Client) connectionFatal(generation uint64) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.generation != generation {
		if client.readerErr != nil {
			return client.readerErr
		}
		return errors.New("gateway connection changed")
	}
	if client.readerFatal {
		return client.readerErr
	}
	return nil
}

func pairingRequired(frame Frame) bool {
	errorFrame := frame.Map("error")
	details := Map(errorFrame["details"])
	return errorFrame["code"] == "NOT_PAIRED" || details["code"] == "PAIRING_REQUIRED"
}

func grantedScopes(payload map[string]any) []string {
	auth := Map(payload["auth"])
	raw, _ := auth["scopes"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if value, ok := item.(string); ok {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func (client *Client) failTransport(transport Transport, readerErr error) {
	_ = client.terminateConnection(transport, readerErr, false)
}

func (client *Client) failConnectionFatal(readerErr error) error {
	return client.terminateConnection(nil, readerErr, true)
}

func (client *Client) failTransportFatal(transport Transport, readerErr error) {
	_ = client.terminateConnection(transport, readerErr, true)
}

func (client *Client) terminateConnection(expected Transport, readerErr error, fatal bool) error {
	client.mu.Lock()
	termination, ok := client.terminateConnectionLocked(expected, readerErr, fatal)
	client.mu.Unlock()
	if !ok {
		return nil
	}
	return finishConnectionTermination(termination)
}

type connectionTermination struct {
	cancel      context.CancelFunc
	transport   Transport
	pending     map[string]chan responseDelivery
	deliveryErr error
}

func (client *Client) terminateConnectionLocked(expected Transport, readerErr error, fatal bool) (connectionTermination, bool) {
	if expected != nil && client.transport != expected {
		return connectionTermination{}, false
	}
	if client.readerErr == nil {
		client.readerErr = readerErr
		client.readerFatal = fatal
		close(client.readerFailed)
	}
	deliveryErr := client.readerErr
	cancel := client.reader
	transport := client.transport
	client.reader = nil
	client.transport = nil
	client.connected = nil
	pending := client.pending
	client.pending = make(map[string]chan responseDelivery)
	return connectionTermination{cancel: cancel, transport: transport, pending: pending, deliveryErr: deliveryErr}, true
}

func finishConnectionTermination(termination connectionTermination) error {
	if termination.cancel != nil {
		termination.cancel()
	}
	for _, reply := range termination.pending {
		select {
		case reply <- responseDelivery{err: termination.deliveryErr}:
		default:
		}
	}
	if termination.transport != nil {
		return termination.transport.Close()
	}
	return nil
}
