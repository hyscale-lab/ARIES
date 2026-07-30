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

	mu        sync.Mutex
	transport Transport
	sequence  int
	pending   map[string]chan Frame
	events    []Frame
	eventCh   chan Frame
	readerErr error
	reader    context.CancelFunc
}

type Options struct {
	Role           string
	Scopes         []string
	Token          string
	Device         *DeviceIdentity
	DeviceToken    string
	EventQueueSize int
}

type ConnectOptions struct {
	AllowPairingRequired bool
	Timeout              time.Duration
	RetryDelay           time.Duration
	ChallengeTimeout     time.Duration
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
		size = 128
	}
	return &Client{
		dial: dial, role: role, scopes: scopes, token: options.Token,
		device: options.Device, deviceTk: options.DeviceToken,
		pending: make(map[string]chan Frame), eventCh: make(chan Frame, size),
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

func (client *Client) Connect(ctx context.Context, options ConnectOptions) (map[string]any, error) {
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
			return client.finishConnect(ctx, first, options.AllowPairingRequired)
		}
		lastErr = err
		if ctx.Err() != nil || !time.Now().Add(retryDelay).Before(deadline) {
			break
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("gateway websocket connect failed: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("gateway websocket connect failed: %w", lastErr)
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

func (client *Client) finishConnect(ctx context.Context, challenge Frame, allowPairing bool) (map[string]any, error) {
	payload := challenge.Map("payload")
	nonce, _ := payload["nonce"].(string)
	if nonce == "" {
		return nil, fmt.Errorf("connect.challenge missing nonce: %s", StableString(challenge))
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
		"auth": auth, "locale": "en-US", "userAgent": "gateway-client/aries-realtime/0.1",
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
		return nil, err
	}
	if response.Bool("ok") {
		payload := response.Map("payload")
		return payload, nil
	}
	if allowPairing && pairingRequired(response) {
		return map[string]any{"auth": map[string]any{}, "pairing_required": true, "connect_error": map[string]any(response)}, nil
	}
	return nil, fmt.Errorf("connect failed: %s", StableString(response))
}

func (client *Client) Call(ctx context.Context, method string, params map[string]any) (Frame, error) {
	if strings.TrimSpace(method) == "" {
		return nil, errors.New("gateway method is required")
	}
	id := client.nextID(method)
	reply := make(chan Frame, 1)
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
		return nil, err
	}
	select {
	case response := <-reply:
		return response, nil
	case <-ctx.Done():
		client.removePending(id)
		return nil, ctx.Err()
	}
}

func (client *Client) RecvEvent(ctx context.Context) (Frame, error) {
	select {
	case frame := <-client.eventCh:
		return frame, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (client *Client) RestoreEvents(frames []Frame) {
	for _, frame := range frames {
		select {
		case client.eventCh <- frame:
		default:
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
	client.mu.Lock()
	cancel := client.reader
	transport := client.transport
	client.reader = nil
	client.transport = nil
	for id, ch := range client.pending {
		delete(client.pending, id)
		close(ch)
	}
	client.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if transport != nil {
		return transport.Close()
	}
	return nil
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
			client.mu.Lock()
			client.readerErr = err
			for id, ch := range client.pending {
				delete(client.pending, id)
				close(ch)
			}
			client.mu.Unlock()
			return
		}
		if len(content) > maxMessageBytes {
			continue
		}
		var frame Frame
		if err := json.Unmarshal(content, &frame); err != nil {
			frame = Frame{"type": "parse.error", "error": err.Error(), "raw": string(content)}
		}
		if frame.String("type") == "res" {
			if id := frame.String("id"); id != "" {
				client.mu.Lock()
				reply := client.pending[id]
				delete(client.pending, id)
				client.mu.Unlock()
				if reply != nil {
					reply <- frame
					close(reply)
					continue
				}
			}
		}
		client.mu.Lock()
		client.events = append(client.events, frame)
		client.mu.Unlock()
		select {
		case client.eventCh <- frame:
		default:
		case <-ctx.Done():
			return
		}
	}
}

func pairingRequired(frame Frame) bool {
	errorFrame := frame.Map("error")
	details := toMap(errorFrame["details"])
	return errorFrame["code"] == "NOT_PAIRED" || details["code"] == "PAIRING_REQUIRED"
}

func GrantedScopes(payload map[string]any) []string {
	auth := toMap(payload["auth"])
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

func DeviceToken(payload map[string]any) string {
	auth := toMap(payload["auth"])
	value, _ := auth["deviceToken"].(string)
	return value
}

func PairingRequestID(payload map[string]any) string {
	errorFrame := toMap(payload["connect_error"])
	errorPayload := toMap(errorFrame["error"])
	details := toMap(errorPayload["details"])
	value, _ := details["requestId"].(string)
	return value
}

func StableString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	content, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(content)
}
