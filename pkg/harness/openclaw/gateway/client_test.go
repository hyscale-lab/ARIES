package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func (client *Client) queuedEventUsage() (int, int) {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.eventCount, client.eventBytes
}

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
	if count, bytes := client.queuedEventUsage(); payload.Role != "operator" || count != 0 || bytes != 0 {
		t.Fatalf("summary/queued usage = %#v/%d/%d", payload, count, bytes)
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
}

func TestClientDiscardsUnmatchedResponsesAndLateChallenges(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)

	secret := "device-token-must-not-escape"
	transport.deliver(Frame{"type": "res", "id": "late-connect", "ok": true, "payload": map[string]any{"deviceToken": secret}})
	transport.deliver(Frame{"type": "res", "ok": false, "error": map[string]any{"details": map[string]any{"token": secret}}})
	transport.deliver(Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": secret}})
	transport.deliver(Frame{"type": "event", "event": "safe", "payload": map[string]any{"status": "ready"}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	frame, err := client.RecvEvent(ctx)
	if err != nil || frame.String("event") != "safe" {
		t.Fatalf("RecvEvent = %#v, %v", frame, err)
	}
	content, _ := json.Marshal(frame)
	if count, bytes := client.queuedEventUsage(); strings.Contains(string(content), secret) || count != 0 || bytes != 0 {
		t.Fatalf("unsafe response/challenge retained: %s usage=%d/%d", content, count, bytes)
	}
}

func TestClientResponseOnlyPreservesChallengeOrderingAndDiscardsLaterEvents(t *testing.T) {
	t.Run("event-before-challenge", func(t *testing.T) {
		transport := newFakeTransport(
			Frame{"type": "event", "event": "early"},
			Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "n"}},
		)
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{EventDisposition: EventDispositionResponseOnly})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.connectOnce(context.Background(), time.Second); err == nil || !strings.Contains(err.Error(), "expected connect.challenge") {
			t.Fatalf("connectOnce error = %v", err)
		}
	})

	t.Run("challenge-once-then-discard", func(t *testing.T) {
		transport := newFakeTransport()
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{EventDisposition: EventDispositionResponseOnly})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		client.awaitChallenge = true
		go client.readLoop(context.Background(), transport)
		transport.deliver(Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "first"}})
		frame, err := client.RecvEvent(context.Background())
		if err != nil || frame.Map("payload")["nonce"] != "first" {
			t.Fatalf("challenge = %#v, %v", frame, err)
		}
		transport.deliver(Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "late"}})
		transport.deliver(Frame{"type": "event", "event": "progress", "payload": map[string]any{"text": "discard"}})
		deadline := time.Now().Add(time.Second)
		for len(transport.in) != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Microsecond)
		}
		if count, bytes := client.queuedEventUsage(); count != 0 || bytes != 0 || client.FatalError() != nil {
			t.Fatalf("discarded event usage/fatal = %d/%d/%v", count, bytes, client.FatalError())
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if frame, err := client.RecvEvent(ctx); !errors.Is(err, context.DeadlineExceeded) || frame != nil {
			t.Fatalf("discarded event was delivered: %#v, %v", frame, err)
		}
	})
}

func TestClientEventDispositionDefaultsToDeliveryAndRejectsUnknown(t *testing.T) {
	client, err := New(func(context.Context) (Transport, error) { return newFakeTransport(), nil }, Options{})
	if err != nil || client.eventDisposition != EventDispositionDelivery {
		t.Fatalf("default disposition = %v, %v", client.eventDisposition, err)
	}
	if _, err := New(func(context.Context) (Transport, error) { return newFakeTransport(), nil }, Options{EventDisposition: EventDisposition(255)}); err == nil {
		t.Fatal("unknown event disposition was accepted")
	}
}

func TestClientResponseOnlyStillRejectsMalformedJSON(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{EventDisposition: EventDispositionResponseOnly})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)
	transport.in <- []byte("{")
	deadline := time.Now().Add(time.Second)
	for client.FatalError() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Microsecond)
	}
	if err := client.FatalError(); err == nil || !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("FatalError = %v", err)
	}
}

func TestResponseErrorRetainsOnlyBoundedCodeAndMessage(t *testing.T) {
	secret := "auth-secret-canary"
	err := ResponseError("connect", Frame{"error": map[string]any{
		"code": "DENIED", "message": "authentication rejected\n",
		"details": map[string]any{"token": secret, "nonce": secret},
	}})
	if got := err.Error(); got != "gateway connect rejected request (DENIED): authentication rejected" || strings.Contains(got, secret) {
		t.Fatalf("ResponseError = %q", got)
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
	if count, _ := client.queuedEventUsage(); count > cap(client.eventCh) {
		t.Fatalf("queued events exceeded bound: %d", count)
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
		for count, _ := client.queuedEventUsage(); count != 1 && time.Now().Before(deadline); count, _ = client.queuedEventUsage() {
			time.Sleep(time.Microsecond)
		}
		if count, _ := client.queuedEventUsage(); count != 1 {
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

func TestClientDeliverySupportsMoreThanOldLifetimeEventLimit(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{EventQueueSize: 4, EventQueueBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)
	const total = 2049
	received := make(chan error)
	go func() {
		for index := 0; index < total; index++ {
			frame, err := client.RecvEvent(context.Background())
			if err != nil {
				received <- err
				return
			}
			if got := int(frame.Map("payload")["index"].(float64)); got != index {
				received <- fmt.Errorf("event index = %d, want %d", got, index)
				return
			}
			received <- nil
		}
	}()
	for index := 0; index < total; index++ {
		transport.deliver(Frame{"type": "event", "event": "progress", "payload": map[string]any{"index": index}})
		if err := <-received; err != nil {
			t.Fatal(err)
		}
	}
	if count, bytes := client.queuedEventUsage(); count != 0 || bytes != 0 {
		t.Fatalf("queued usage = %d/%d, want zero", count, bytes)
	}
}

func TestClientFailsClosedWhenEventQueueByteBudgetOverflows(t *testing.T) {
	transport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{
		EventQueueSize: 8, EventQueueBytes: 32,
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
			if count, bytes := client.queuedEventUsage(); !strings.Contains(err.Error(), "queue overflow") || count != 0 || bytes != 0 {
				t.Fatalf("reader/queued usage = %v/%d/%d", err, count, bytes)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("queue byte overflow did not fail connection")
}

func TestClientReleasesEventQueueByteBudgetOnDelivery(t *testing.T) {
	transport := newFakeTransport()
	frame := Frame{"type": "event", "event": "progress", "payload": map[string]any{"text": "bounded"}}
	content, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{EventQueueSize: 1, EventQueueBytes: len(content)})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	go client.readLoop(context.Background(), transport)
	for iteration := 0; iteration < 2; iteration++ {
		transport.deliver(frame)
		deadline := time.Now().Add(time.Second)
		for count, _ := client.queuedEventUsage(); count == 0 && time.Now().Before(deadline); count, _ = client.queuedEventUsage() {
			time.Sleep(time.Microsecond)
		}
		if count, bytes := client.queuedEventUsage(); count != 1 || bytes != len(content) {
			t.Fatalf("iteration %d queued usage = %d/%d", iteration, count, bytes)
		}
		if _, err := client.RecvEvent(context.Background()); err != nil {
			t.Fatal(err)
		}
		if count, bytes := client.queuedEventUsage(); count != 0 || bytes != 0 {
			t.Fatalf("iteration %d released usage = %d/%d", iteration, count, bytes)
		}
	}
}

func TestEventDequeueReleasesChargeBeforeFatalDominance(t *testing.T) {
	client, err := New(func(context.Context) (Transport, error) { return newFakeTransport(), nil }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	client.generation = 7
	client.eventCount = 1
	client.eventBytes = 23
	client.readerFatal = true
	client.readerErr = errors.New("fatal protocol state")
	frame, err, current := client.finishEventDelivery(eventDelivery{frame: Frame{"event": "partial"}, bytes: 23, generation: 7}, 7)
	if !current || frame != nil || err == nil || !strings.Contains(err.Error(), "fatal protocol state") {
		t.Fatalf("delivery result = %#v, %v, %v", frame, err, current)
	}
	if count, bytes := client.queuedEventUsage(); count != 0 || bytes != 0 {
		t.Fatalf("fatal-dominated delivery remained charged: %d/%d", count, bytes)
	}
}

func TestStaleGenerationDequeueDoesNotReleaseCurrentCharge(t *testing.T) {
	client, err := New(func(context.Context) (Transport, error) { return newFakeTransport(), nil }, Options{EventQueueSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	client.generation = 2
	client.eventCount = 1
	client.eventBytes = 11
	client.eventCh <- eventDelivery{frame: Frame{"event": "stale"}, bytes: 99, generation: 1}
	client.eventCh <- eventDelivery{frame: Frame{"event": "current"}, bytes: 11, generation: 2}
	frame, err := client.RecvEvent(context.Background())
	if err != nil || frame.String("event") != "current" {
		t.Fatalf("RecvEvent = %#v, %v", frame, err)
	}
	if count, bytes := client.queuedEventUsage(); count != 0 || bytes != 0 {
		t.Fatalf("stale dequeue changed current accounting: %d/%d", count, bytes)
	}
}

func TestEventOverflowDominatesQueuedPartialEvent(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		prepare func(*Client)
		frames  []Frame
		want    string
	}{
		{
			name: "event-queue", options: Options{EventQueueSize: 1},
			frames: []Frame{{"type": "event", "event": "partial"}, {"type": "event", "event": "overflow"}},
			want:   "event queue overflow",
		},
		{
			name: "challenge-queue", options: Options{EventQueueSize: 1},
			prepare: func(client *Client) { client.awaitChallenge = true },
			frames:  []Frame{{"type": "event", "event": "partial"}, {"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "n"}}},
			want:    "event queue overflow",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newFakeTransport()
			client, err := New(func(context.Context) (Transport, error) { return transport, nil }, test.options)
			if err != nil {
				t.Fatal(err)
			}
			client.transport = transport
			if test.prepare != nil {
				test.prepare(client)
			}
			go client.readLoop(context.Background(), transport)
			for _, frame := range test.frames {
				transport.deliver(frame)
			}
			deadline := time.Now().Add(time.Second)
			for {
				client.mu.Lock()
				fatal := client.readerFatal
				client.mu.Unlock()
				if fatal || time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Microsecond)
			}
			frame, err := client.RecvEvent(context.Background())
			if err == nil || frame != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RecvEvent accepted partial frame %#v over fatal state: %v", frame, err)
			}
		})
	}
}

func TestFatalErrorDistinguishesNormalEOFFromOverflow(t *testing.T) {
	t.Run("normal-eof", func(t *testing.T) {
		transport := newFakeTransport()
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{EventQueueSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		go client.readLoop(context.Background(), transport)
		transport.deliver(Frame{"type": "event", "event": "complete"})
		deadline := time.Now().Add(time.Second)
		for count, _ := client.queuedEventUsage(); count == 0 && time.Now().Before(deadline); count, _ = client.queuedEventUsage() {
			time.Sleep(time.Microsecond)
		}
		if err := transport.Close(); err != nil {
			t.Fatal(err)
		}
		for {
			client.mu.Lock()
			readerErr := client.readerErr
			client.mu.Unlock()
			if readerErr != nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Microsecond)
		}
		if err := client.FatalError(); err != nil {
			t.Fatalf("normal EOF reported fatal: %v", err)
		}
		frame, err := client.RecvEvent(context.Background())
		if err != nil || frame.String("event") != "complete" {
			t.Fatalf("queued complete event = %#v, %v", frame, err)
		}
	})

	t.Run("fatal-overflow", func(t *testing.T) {
		transport := newFakeTransport()
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{EventQueueSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		go client.readLoop(context.Background(), transport)
		transport.deliver(Frame{"type": "event", "event": "partial"})
		transport.deliver(Frame{"type": "event", "event": "overflow"})
		deadline := time.Now().Add(time.Second)
		for client.FatalError() == nil && time.Now().Before(deadline) {
			time.Sleep(time.Microsecond)
		}
		if err := client.FatalError(); err == nil || !strings.Contains(err.Error(), "event queue overflow") {
			t.Fatalf("fatal overflow not reported: %v", err)
		}
	})
}

func TestCallFatalStateDominatesQueuedSuccess(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		transport := newFakeTransport()
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		done := make(chan error, 1)
		go func() { _, err := client.Call(context.Background(), "status", nil); done <- err }()
		sent := transport.nextSent(t)
		client.mu.Lock()
		reply := client.pending[sent.String("id")]
		reply <- responseDelivery{frame: Frame{"type": "res", "id": sent.String("id"), "ok": true}}
		time.Sleep(time.Microsecond)
		fatalErr := errors.New("gateway response queue overflow")
		client.readerErr = fatalErr
		client.readerFatal = true
		client.transport = nil
		client.mu.Unlock()
		if err := <-done; err == nil || !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("iteration %d accepted queued success over fatal state: %v", iteration, err)
		}
	}
}

func TestReadConnectionPublishesResponseOverflowBeforeCallCanAcceptSuccess(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		release := make(chan struct{})
		transport := newFakeTransport()
		transport.sendRelease = release
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		go client.readLoop(context.Background(), transport)
		done := make(chan error, 1)
		go func() { _, err := client.Call(context.Background(), "status", nil); done <- err }()
		sent := transport.nextSent(t)
		response := Frame{"type": "res", "id": sent.String("id"), "ok": true}
		transport.deliver(response)
		transport.deliver(response)
		deadline := time.Now().Add(time.Second)
		for {
			client.mu.Lock()
			fatal := client.readerFatal
			client.mu.Unlock()
			if fatal || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Microsecond)
		}
		close(release)
		if err := <-done; err == nil || !strings.Contains(err.Error(), "response queue overflow") {
			t.Fatalf("iteration %d accepted success before overflow publication: %v", iteration, err)
		}
	}
}

func TestOldConnectionCannotDispatchIntoNewGeneration(t *testing.T) {
	oldTransport := newFakeTransport()
	newTransport := newFakeTransport()
	client, err := New(func(context.Context) (Transport, error) { return nil, errors.New("unused") }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.transport = oldTransport
	client.generation = 1
	client.eventCh = make(chan eventDelivery, 8)
	client.readerFailed = make(chan struct{})
	client.mu.Unlock()
	go client.readConnection(context.Background(), oldTransport, 1)

	client.mu.Lock()
	client.transport = newTransport
	client.generation = 2
	client.eventCh = make(chan eventDelivery, 8)
	client.eventCount = 0
	client.eventBytes = 0
	client.readerErr = nil
	client.readerFatal = false
	client.readerFailed = make(chan struct{})
	client.awaitChallenge = true
	client.mu.Unlock()
	go client.readConnection(context.Background(), newTransport, 2)

	oldTransport.deliver(Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "stale"}})
	oldTransport.deliver(Frame{"type": "event", "event": "stale"})
	oldTransport.deliver(Frame{"type": "res", "id": "late", "ok": true})
	newTransport.deliver(Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "current"}})
	newTransport.deliver(Frame{"type": "event", "event": "current"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	challenge, err := client.RecvEvent(ctx)
	if err != nil || challenge.String("event") != "connect.challenge" || challenge.Map("payload")["nonce"] != "current" {
		t.Fatalf("new generation challenge = %#v, %v", challenge, err)
	}
	frame, err := client.RecvEvent(ctx)
	if err != nil || frame.String("event") != "current" {
		t.Fatalf("new generation event = %#v, %v", frame, err)
	}
	if count, bytes := client.queuedEventUsage(); count != 0 || bytes != 0 {
		t.Fatalf("stale generation changed queued usage: %d/%d", count, bytes)
	}
}

type fakeTransport struct {
	in          chan []byte
	out         chan []byte
	closed      chan struct{}
	once        sync.Once
	sendErr     error
	sendRelease <-chan struct{}
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
		if transport.sendRelease == nil {
			return nil
		}
		select {
		case <-transport.sendRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-transport.closed:
			return context.Canceled
		}
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
