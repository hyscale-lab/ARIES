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
	if DeviceToken(payload) != "dt" {
		t.Fatalf("device token = %q, want dt", DeviceToken(payload))
	}
	scopes := GrantedScopes(payload)
	if len(scopes) != 1 || scopes[0] != "operator.write" {
		t.Fatalf("scopes = %#v", scopes)
	}
}

func TestClientConnectCanReturnPairingRequired(t *testing.T) {
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
	payload, err := client.Connect(context.Background(), ConnectOptions{AllowPairingRequired: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if !payload["pairing_required"].(bool) || PairingRequestID(payload) != "pair-1" {
		t.Fatalf("payload = %#v", payload)
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

func TestClientReaderDoesNotBlockResponsesWhenEventQueueIsFull(t *testing.T) {
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
	sent := transport.nextSent(t)
	transport.deliver(Frame{"type": "event", "event": "talk.event", "payload": map[string]any{"n": 1}})
	transport.deliver(Frame{"type": "event", "event": "talk.event", "payload": map[string]any{"n": 2}})
	transport.deliver(Frame{"type": "res", "id": sent["id"], "ok": true})

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("response was blocked behind a full event queue")
	}
	if len(client.Events()) != 2 {
		t.Fatalf("stored events = %d, want 2", len(client.Events()))
	}
}

func TestTalkAndChatParsers(t *testing.T) {
	session, err := TalkSessionInfoFromPayload(map[string]any{
		"sessionId": "s-1", "relaySessionId": "r-1",
		"audio": map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": float64(24000)},
	})
	if err != nil {
		t.Fatalf("TalkSessionInfoFromPayload returned error: %v", err)
	}
	if session.SessionID != "s-1" || session.RelaySessionID != "r-1" || session.InputSampleRateHz != 24000 {
		t.Fatalf("session = %#v", session)
	}

	frame := Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{
			"relaySessionId": "relay",
			"talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "s-1", "callId": "call-1",
				"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
			},
		},
	}
	talk, ok := TalkEventFromFrame(frame)
	if !ok {
		t.Fatalf("TalkEventFromFrame rejected frame")
	}
	tool, ok := ToolCallEventFromTalk(talk)
	if !ok || tool.RelaySessionID != "relay" || tool.Args["question"] != "q" {
		t.Fatalf("tool = %#v ok=%v", tool, ok)
	}

	chat, ok := ChatEventFromFrame(Frame{
		"type": "event", "event": "chat",
		"payload": map[string]any{
			"runId": "run-1", "state": "final",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "hello"}, map[string]any{"type": "text", "text": " world"}}},
		},
	})
	if !ok || chat.MessageText != "hello world" {
		t.Fatalf("chat = %#v ok=%v", chat, ok)
	}
}

type fakeTransport struct {
	in     chan []byte
	out    chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFakeTransport(initial ...Frame) *fakeTransport {
	transport := &fakeTransport{in: make(chan []byte, 16), out: make(chan []byte, 16), closed: make(chan struct{})}
	for _, frame := range initial {
		transport.deliver(frame)
	}
	return transport
}

func (transport *fakeTransport) Send(ctx context.Context, content []byte) error {
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
