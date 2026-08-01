package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
	defaultEventHistorySize  = 2048
	defaultEventHistoryBytes = 16 << 20
)

var DefaultScopes = []string{"operator.write"}

// Transport is the narrow websocket-like surface used by the Gateway client.
type Transport interface {
	Send(context.Context, []byte) error
	Receive(context.Context) ([]byte, error)
	Close() error
}

type DialFunc func(context.Context) (Transport, error)

type DeviceIdentity struct {
	ID        string
	PublicKey string
	private   ed25519.PrivateKey
}

type Client struct {
	dial     DialFunc
	role     string
	scopes   []string
	token    string
	device   *DeviceIdentity
	deviceTk string

	mu           sync.Mutex
	transport    Transport
	sequence     int
	pending      map[string]chan responseDelivery
	events       []Frame
	eventCh      chan Frame
	eventMax     int
	eventByteMax int
	eventBytes   int
	readerErr    error
	readerFatal  bool
	reader       context.CancelFunc
	readerFailed chan struct{}
	connected    *ConnectSummary
}

type Options struct {
	Role              string
	Scopes            []string
	Token             string
	Device            *DeviceIdentity
	DeviceToken       string
	EventQueueSize    int
	EventHistorySize  int
	EventHistoryBytes int
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
		scopes = append([]string(nil), DefaultScopes...)
	}
	size := options.EventQueueSize
	if size <= 0 {
		size = defaultEventQueueSize
	}
	historySize := options.EventHistorySize
	if historySize <= 0 {
		historySize = defaultEventHistorySize
	}
	historyBytes := options.EventHistoryBytes
	if historyBytes <= 0 {
		historyBytes = defaultEventHistoryBytes
	}
	return &Client{
		dial: dial, role: role, scopes: scopes, token: options.Token,
		device: options.Device, deviceTk: options.DeviceToken,
		pending: make(map[string]chan responseDelivery), eventCh: make(chan Frame, size), eventMax: historySize, eventByteMax: historyBytes,
		readerFailed: make(chan struct{}),
	}, nil
}

func GenerateDeviceIdentity() (*DeviceIdentity, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate gateway device identity: %w", err)
	}
	sum := sha256.Sum256(public)
	return &DeviceIdentity{
		ID:        fmt.Sprintf("%x", sum[:]),
		PublicKey: base64.RawURLEncoding.EncodeToString(public),
		private:   private,
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
	client.reader = cancel
	client.readerErr = nil
	client.readerFatal = false
	client.readerFailed = make(chan struct{})
	client.events = nil
	client.eventBytes = 0
	client.mu.Unlock()
	go client.readLoop(readerCtx, transport)

	challengeCtx, challengeCancel := context.WithTimeout(ctx, challengeTimeout)
	defer challengeCancel()
	first, err := client.RecvEvent(challengeCtx)
	if err != nil {
		client.Close()
		return nil, err
	}
	if first.String("type") != "event" || first.String("event") != "connect.challenge" {
		client.Close()
		return nil, fmt.Errorf("expected connect.challenge, got %s", StableString(first))
	}
	return first, nil
}

func (client *Client) finishConnect(ctx context.Context, challenge Frame) (ConnectSummary, error) {
	payload := challenge.Map("payload")
	nonce, _ := payload["nonce"].(string)
	if nonce == "" {
		return ConnectSummary{}, errors.New("connect.challenge missing nonce")
	}
	authToken := client.token
	auth := map[string]any{"token": client.token}
	if client.deviceTk != "" {
		authToken = client.deviceTk
		auth = map[string]any{"deviceToken": client.deviceTk}
	}
	params := map[string]any{
		"minProtocol": 3, "maxProtocol": 4,
		"client": map[string]any{"id": "gateway-client", "version": "0.1", "platform": "go", "mode": "backend"},
		"role":   client.role, "scopes": append([]string(nil), client.scopes...),
		"caps": []any{}, "commands": []any{}, "permissions": map[string]any{},
		"auth": auth, "locale": "en-US", "userAgent": "gateway-client/aries/0.1",
	}
	if client.device != nil {
		signedAt := time.Now().UnixMilli()
		message := strings.Join([]string{
			"v3", client.device.ID, "gateway-client", "backend", client.role,
			strings.Join(client.scopes, ","), fmt.Sprint(signedAt), authToken, nonce, "go", "",
		}, "|")
		signature := ed25519.Sign(client.device.private, []byte(message))
		params["device"] = map[string]any{
			"id": client.device.ID, "publicKey": client.device.PublicKey,
			"signature": base64.RawURLEncoding.EncodeToString(signature),
			"signedAt":  signedAt, "nonce": nonce,
		}
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
		return nil, fmt.Errorf("send gateway request: %w", err)
	}
	select {
	case delivery := <-reply:
		client.removePending(id)
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
		client.mu.Unlock()
		if readerFatal {
			return nil, readerErr
		}
		select {
		case frame := <-client.eventCh:
			return frame, nil
		default:
		}
		if readerErr != nil {
			return nil, readerErr
		}
		select {
		case frame := <-client.eventCh:
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

func (client *Client) RestoreEvents(frames []Frame) {
	for _, frame := range frames {
		select {
		case client.eventCh <- frame:
		default:
			client.failConnectionFatal(errors.New("gateway event queue overflow"))
			return
		}
	}
}

func (client *Client) DiscardQueuedEvents() {
	for {
		select {
		case <-client.eventCh:
		default:
			return
		}
	}
}

func (client *Client) Events() []Frame {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]Frame(nil), client.events...)
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
	for {
		content, err := transport.Receive(ctx)
		if err != nil {
			client.failTransport(transport, fmt.Errorf("gateway reader stopped: %w", err))
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
			if id := frame.String("id"); id != "" {
				client.mu.Lock()
				reply := client.pending[id]
				client.mu.Unlock()
				if reply != nil {
					select {
					case reply <- responseDelivery{frame: frame}:
						continue
					default:
						client.failTransportFatal(transport, errors.New("gateway response queue overflow"))
						return
					}
				}
			}
		}
		if frame.String("event") == "connect.challenge" {
			select {
			case client.eventCh <- frame:
			default:
				client.failTransportFatal(transport, errors.New("gateway event queue overflow"))
				return
			}
			continue
		}
		client.mu.Lock()
		if len(client.events) >= client.eventMax || client.eventBytes+len(content) > client.eventByteMax {
			client.mu.Unlock()
			client.failTransportFatal(transport, errors.New("gateway event history overflow"))
			return
		}
		client.events = append(client.events, frame)
		client.eventBytes += len(content)
		client.mu.Unlock()
		select {
		case client.eventCh <- frame:
		default:
			client.failTransportFatal(transport, errors.New("gateway event queue overflow"))
			return
		case <-ctx.Done():
			return
		}
	}
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
	if expected != nil && client.transport != expected {
		client.mu.Unlock()
		return nil
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
	client.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, reply := range pending {
		select {
		case reply <- responseDelivery{err: deliveryErr}:
		default:
		}
	}
	if transport != nil {
		return transport.Close()
	}
	return nil
}
