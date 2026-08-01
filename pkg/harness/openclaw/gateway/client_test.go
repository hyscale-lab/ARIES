package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientConnectSignsChallengeAndReturnsPayload(t *testing.T) {
	identity, err := GenerateDeviceIdentity()
	if err != nil {
		t.Fatalf("GenerateDeviceIdentity returned error: %v", err)
	}
	transport := newFakeTransport(
		Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "n-1"}},
	)
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{
		Token: "gateway-token", Device: identity, Scopes: []string{"operator.write", "operator.read"},
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		sent := transport.nextSent(t)
		if sent["method"] != "connect" {
			t.Errorf("method = %v, want connect", sent["method"])
		}
		params := sent["params"].(map[string]any)
		device := params["device"].(map[string]any)
		if device["id"] != identity.ID || device["publicKey"] != identity.PublicKey || device["signature"] == "" {
			t.Errorf("device params = %#v", device)
		}
		transport.deliver(Frame{"type": "res", "id": sent["id"], "ok": true, "payload": map[string]any{"auth": map[string]any{"scopes": []any{"operator.write"}, "deviceToken": "dt"}}})
		close(done)
	}()

	payload, err := client.Connect(context.Background(), ConnectOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	<-done
	content, _ := json.Marshal(payload)
	if strings.Contains(string(content), "dt") {
		t.Fatalf("connect summary leaked device token: %s", content)
	}
	scopes := payload.Scopes
	if len(scopes) != 1 || scopes[0] != "operator.write" {
		t.Fatalf("scopes = %#v", scopes)
	}
	if payload.Role != "operator" || len(client.Events()) != 0 {
		t.Fatalf("summary/history = %#v/%#v", payload, client.Events())
	}
}

func TestClientConnectRejectsPairingRequired(t *testing.T) {
	transport := newFakeTransport(Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "n-1"}})
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{Token: "gateway-token"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	go func() {
		sent := transport.nextSent(t)
		transport.deliver(Frame{
			"type": "res", "id": sent["id"], "ok": false,
			"error": map[string]any{"code": "NOT_PAIRED", "details": map[string]any{"requestId": "pair-1"}},
		})
	}()
	if _, err := client.Connect(context.Background(), ConnectOptions{Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "pairing") {
		t.Fatalf("Connect error = %v", err)
	}
}

func TestClientCallCorrelatesResponseAndQueuesEvents(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{Token: "gateway-token"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if _, err := client.connectOnce(context.Background(), time.Second); err == nil || !strings.Contains(err.Error(), "context deadline") {
		t.Fatalf("connectOnce error = %v, want challenge timeout", err)
	}

	transport = newFakeTransport()
	client, _ = New(func(context.Context) (Transport, error) { return transport, nil }, Options{Token: "gateway-token"})
	client.transport = transport
	go client.readLoop(context.Background(), transport)

	done := make(chan Frame)
	go func() {
		response, err := client.Call(context.Background(), "talk.session.create", map[string]any{"mode": "realtime"})
		if err != nil {
			t.Errorf("Call returned error: %v", err)
			close(done)
			return
		}
		done <- response
	}()
	sent := transport.nextSent(t)
	transport.deliver(Frame{"type": "event", "event": "talk.event", "payload": map[string]any{"talkEvent": map[string]any{"type": "session.ready", "sessionId": "s", "payload": map[string]any{}}}})
	transport.deliver(Frame{"type": "res", "id": sent["id"], "ok": true, "payload": map[string]any{"sessionId": "s"}})

	response := <-done
	if !response.Bool("ok") {
		t.Fatalf("response = %#v", response)
	}
	event, err := client.RecvEvent(context.Background())
	if err != nil {
		t.Fatalf("RecvEvent returned error: %v", err)
	}
	if event.String("event") != "talk.event" {
		t.Fatalf("event = %#v", event)
	}
	client.RestoreEvents([]Frame{event})
	restored, err := client.RecvEvent(context.Background())
	if err != nil || restored.String("event") != "talk.event" {
		t.Fatalf("restored = %#v err=%v", restored, err)
	}
}

func TestClientReaderFailsClosedWhenEventQueueIsFull(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{Token: "gateway-token", EventQueueSize: 1})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)

	done := make(chan error, 1)
	go func() {
		response, err := client.Call(context.Background(), "talk.session.appendAudio", map[string]any{"sessionId": "s"})
		if err != nil {
			done <- err
			return
		}
		if !response.Bool("ok") {
			done <- fmt.Errorf("response = %#v", response)
			return
		}
		done <- nil
	}()
	_ = transport.nextSent(t)
	transport.deliver(Frame{"type": "event", "event": "talk.event", "payload": map[string]any{"n": 1}})
	transport.deliver(Frame{"type": "event", "event": "talk.event", "payload": map[string]any{"n": 2}})

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("Call error = %v, want overflow", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending response was not woken by overflow")
	}
	if len(client.Events()) > defaultEventHistorySize {
		t.Fatalf("stored events exceeded bound: %d", len(client.Events()))
	}
}

func TestClientCloseWakesPendingCallWithExplicitError(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)
	done := make(chan error, 1)
	go func() { _, err := client.Call(context.Background(), "status", nil); done <- err }()
	_ = transport.nextSent(t)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("Call error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call blocked after Close")
	}
}

func TestClientCloseWakesPendingEventWaiter(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)
	done := make(chan error, 1)
	go func() { _, err := client.RecvEvent(context.Background()); done <- err }()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("RecvEvent error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("event waiter blocked after Close")
	}
}

func TestClientDrainsQueuedEventBeforeReaderEOF(t *testing.T) {
	for iteration := 0; iteration < 500; iteration++ {
		transport := newFakeTransport()
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		go client.readLoop(context.Background(), transport)
		transport.deliver(Frame{"type": "event", "event": "queued", "payload": map[string]any{"iteration": iteration}})
		deadline := time.Now().Add(time.Second)
		for len(client.Events()) != 1 && time.Now().Before(deadline) {
			time.Sleep(time.Microsecond)
		}
		if len(client.Events()) != 1 {
			t.Fatalf("iteration %d: event was not queued", iteration)
		}
		if err := transport.Close(); err != nil {
			t.Fatal(err)
		}
		frame, err := client.RecvEvent(context.Background())
		if err != nil || frame.String("event") != "queued" {
			t.Fatalf("iteration %d: queued frame = %#v, %v", iteration, frame, err)
		}
		if _, err := client.RecvEvent(context.Background()); err == nil || !strings.Contains(err.Error(), "reader stopped") {
			t.Fatalf("iteration %d: terminal reader error = %v", iteration, err)
		}
	}
}

func TestClientFailsClosedWhenEventHistoryOverflows(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{EventQueueSize: 8, EventHistorySize: 1})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)
	transport.deliver(Frame{"type": "event", "event": "one"})
	transport.deliver(Frame{"type": "event", "event": "two"})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		err := client.readerErr
		client.mu.Unlock()
		if err != nil {
			if !strings.Contains(err.Error(), "history overflow") {
				t.Fatalf("reader error = %v", err)
			}
			if len(client.Events()) != 1 {
				t.Fatalf("history = %d", len(client.Events()))
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("history overflow did not fail connection")
}

func TestClientFailsClosedWhenEventHistoryByteBudgetOverflows(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{
		EventQueueSize: 8, EventHistorySize: 8, EventHistoryBytes: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)
	transport.deliver(Frame{"type": "event", "event": "chat", "payload": map[string]any{"text": strings.Repeat("x", 64)}})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		err := client.readerErr
		client.mu.Unlock()
		if err != nil {
			if !strings.Contains(err.Error(), "history overflow") || len(client.Events()) != 0 {
				t.Fatalf("reader/history = %v/%#v", err, client.Events())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("history byte overflow did not fail connection")
}

type fakeTransport struct {
	in      chan []byte
	out     chan []byte
	closed  chan struct{}
	once    sync.Once
	sendErr error
}

func newFakeTransport(initial ...Frame) *fakeTransport {
	transport := &fakeTransport{in: make(chan []byte, 16), out: make(chan []byte, 16), closed: make(chan struct{})}
	for _, frame := range initial {
		transport.deliver(frame)
	}
	return transport
}

func (transport *fakeTransport) Send(ctx context.Context, content []byte) error {
	if transport.sendErr != nil {
		return transport.sendErr
	}
	select {
	case transport.out <- append([]byte(nil), content...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-transport.closed:
		return context.Canceled
	}
}

func (transport *fakeTransport) Receive(ctx context.Context) ([]byte, error) {
	select {
	case content := <-transport.in:
		return content, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-transport.closed:
		return nil, context.Canceled
	}
}

func (transport *fakeTransport) Close() error {
	transport.once.Do(func() { close(transport.closed) })
	return nil
}

func (transport *fakeTransport) deliver(frame Frame) {
	content, _ := json.Marshal(frame)
	transport.in <- content
}

func (transport *fakeTransport) nextSent(t *testing.T) Frame {
	t.Helper()
	select {
	case content := <-transport.out:
		var frame Frame
		if err := json.Unmarshal(content, &frame); err != nil {
			t.Fatalf("decode sent frame: %v", err)
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for sent frame")
		return nil
	}
}
