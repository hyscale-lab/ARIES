package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func connectedAgentClient(t *testing.T, transport *fakeTransport, scopes ...string) *Client {
	t.Helper()
	return connectedAgentClientWithOptions(t, transport, Options{}, scopes...)
}

func connectedAgentClientWithOptions(t *testing.T, transport *fakeTransport, options Options, scopes ...string) *Client {
	t.Helper()
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, options)
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	client.connected = &ConnectSummary{Role: "operator", Scopes: scopes}
	go client.readLoop(context.Background(), transport)
	return client
}

func TestAgentResponseOnlySurvivesHighVolumeUnsolicitedEvents(t *testing.T) {
	transport := newFakeTransport()
	client := connectedAgentClientWithOptions(t, transport, Options{EventDisposition: EventDispositionResponseOnly}, "operator.write")
	defer client.Close()
	done := make(chan struct {
		result AgentResult
		err    error
	}, 1)
	go func() {
		result, err := client.Agent(context.Background(), validAgentRequest())
		done <- struct {
			result AgentResult
			err    error
		}{result, err}
	}()

	sent := transport.nextSent(t)
	id := sent.String("id")
	transport.deliver(acceptedFrame(id, "run-high-volume"))
	for index := 0; index < 2049; index++ {
		transport.deliver(Frame{"type": "event", "event": "agent.progress", "payload": map[string]any{"index": index}})
	}
	transport.deliver(terminalFrame(id, "run-high-volume", "ok", []any{map[string]any{"text": "complete"}}))

	got := <-done
	if got.err != nil || got.result.RunID != "run-high-volume" || got.result.Text != "complete" {
		t.Fatalf("Agent = %#v, %v", got.result, got.err)
	}
	if count, bytes := client.queuedEventUsage(); count != 0 || bytes != 0 || client.FatalError() != nil {
		t.Fatalf("response-only retained events or fatal state: %d/%d/%v", count, bytes, client.FatalError())
	}
	select {
	case extra := <-transport.out:
		t.Fatalf("agent request was resubmitted: %s", extra)
	default:
	}
}

func validAgentRequest() AgentRequest {
	return AgentRequest{Message: "repair", SessionKey: "agent:main:aries-fix-git", IdempotencyKey: "stable-key", Thinking: "off"}
}

func acceptedFrame(id, runID string) Frame {
	return Frame{"type": "res", "id": id, "ok": true, "payload": map[string]any{"status": "accepted", "runId": runID}}
}

func terminalFrame(id, runID, status string, payloads []any) Frame {
	return Frame{"type": "res", "id": id, "ok": true, "payload": map[string]any{"status": status, "runId": runID, "result": map[string]any{"payloads": payloads}}}
}

func TestAgentSendsOnceAndCorrelatesAcceptedAndTerminal(t *testing.T) {
	transport := newFakeTransport()
	client := connectedAgentClient(t, transport, "operator.write")
	defer client.Close()
	done := make(chan struct {
		result AgentResult
		err    error
	}, 1)
	go func() {
		result, err := client.Agent(context.Background(), validAgentRequest())
		done <- struct {
			result AgentResult
			err    error
		}{result, err}
	}()
	sent := transport.nextSent(t)
	params := sent.Map("params")
	if sent.String("method") != "agent" || params["message"] != "repair" || params["sessionKey"] != "agent:main:aries-fix-git" || params["idempotencyKey"] != "stable-key" || params["thinking"] != "off" {
		t.Fatalf("agent request = %#v", sent)
	}
	if _, ok := params["timeoutMs"]; ok {
		t.Fatalf("v2026.7.1 agent schema rejects timeoutMs: %#v", params)
	}
	id := sent.String("id")
	transport.deliver(Frame{"type": "event", "event": "chat", "payload": map[string]any{"runId": "other"}})
	transport.deliver(Frame{"type": "res", "id": "other-id", "ok": true, "payload": map[string]any{"status": "ok", "runId": "other"}})
	transport.deliver(acceptedFrame(id, "run-1"))
	transport.deliver(terminalFrame(id, "run-1", "ok", []any{map[string]any{"text": "first"}, map[string]any{"text": ""}, map[string]any{"text": "second"}}))
	got := <-done
	if got.err != nil || got.result.RunID != "run-1" || got.result.Text != "first\nsecond" {
		t.Fatalf("Agent = %#v, %v", got.result, got.err)
	}
	select {
	case extra := <-transport.out:
		t.Fatalf("agent request was resubmitted: %s", extra)
	default:
	}
}

func TestAgentRejectsProtocolSequenceAndResultViolations(t *testing.T) {
	tests := []struct {
		name   string
		frames func(id string) []Frame
		want   string
	}{
		{"terminal-before-accepted", func(id string) []Frame {
			return []Frame{terminalFrame(id, "run-1", "ok", []any{map[string]any{"text": "x"}})}
		}, "expected accepted"},
		{"accepted-missing-run", func(id string) []Frame { return []Frame{acceptedFrame(id, "")} }, "non-empty runId"},
		{"terminal-missing-run", func(id string) []Frame {
			return []Frame{acceptedFrame(id, "run-1"), terminalFrame(id, "", "ok", []any{map[string]any{"text": "x"}})}
		}, "did not match"},
		{"terminal-wrong-run", func(id string) []Frame {
			return []Frame{acceptedFrame(id, "run-1"), terminalFrame(id, "run-2", "ok", []any{map[string]any{"text": "x"}})}
		}, "did not match"},
		{"terminal-error", func(id string) []Frame {
			return []Frame{acceptedFrame(id, "run-1"), terminalFrame(id, "run-1", "error", []any{map[string]any{"text": "x"}})}
		}, "terminal status"},
		{"missing-result", func(id string) []Frame {
			return []Frame{acceptedFrame(id, "run-1"), {"type": "res", "id": id, "ok": true, "payload": map[string]any{"status": "ok", "runId": "run-1"}}}
		}, "missing result"},
		{"malformed-payload", func(id string) []Frame {
			return []Frame{acceptedFrame(id, "run-1"), terminalFrame(id, "run-1", "ok", []any{"bad"})}
		}, "malformed payload"},
		{"empty-result", func(id string) []Frame {
			return []Frame{acceptedFrame(id, "run-1"), terminalFrame(id, "run-1", "ok", []any{map[string]any{"text": ""}})}
		}, "no payload text"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newFakeTransport()
			client := connectedAgentClient(t, transport, "operator.write")
			defer client.Close()
			done := make(chan error, 1)
			go func() { _, err := client.Agent(context.Background(), validAgentRequest()); done <- err }()
			sent := transport.nextSent(t)
			for _, frame := range test.frames(sent.String("id")) {
				transport.deliver(frame)
			}
			if err := <-done; err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Agent error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAgentProtocolErrorRetainsOnlyBoundedCodeAndMessage(t *testing.T) {
	err := agentProtocolError(Frame{"error": map[string]any{
		"code": "INVALID_REQUEST", "message": "invalid agent params: missing timeout\n",
		"details": map[string]any{"gatewayToken": "never-retain", "params": map[string]any{"message": "private instruction"}},
	}})
	if got := err.Error(); got != "gateway agent rejected request (INVALID_REQUEST): invalid agent params: missing timeout" || strings.Contains(got, "never-retain") || strings.Contains(got, "private instruction") {
		t.Fatalf("sanitized error = %q", got)
	}
}

func TestAgentRequiresWriteScopeBeforeSend(t *testing.T) {
	transport := newFakeTransport()
	client := connectedAgentClient(t, transport, "operator.read")
	defer client.Close()
	if _, err := client.Agent(context.Background(), validAgentRequest()); err == nil || !strings.Contains(err.Error(), "operator.write") {
		t.Fatalf("Agent error = %v", err)
	}
	select {
	case sent := <-transport.out:
		t.Fatalf("request sent without scope: %s", sent)
	default:
	}
}

func TestAgentDisconnectIsAmbiguousAndNeverResubmits(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-accepted", true: "after-accepted"}[accepted], func(t *testing.T) {
			transport := newFakeTransport()
			client := connectedAgentClient(t, transport, "operator.write")
			done := make(chan error, 1)
			go func() { _, err := client.Agent(context.Background(), validAgentRequest()); done <- err }()
			sent := transport.nextSent(t)
			if accepted {
				transport.deliver(acceptedFrame(sent.String("id"), "run-1"))
				time.Sleep(20 * time.Millisecond)
			}
			transport.Close()
			err := <-done
			want := "ambiguous"
			if accepted {
				want = "outcome is unknown"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Agent error = %v, want %q", err, want)
			}
			select {
			case extra := <-transport.out:
				t.Fatalf("agent request was resubmitted: %s", extra)
			default:
			}
		})
	}
}

func TestAgentCancellationClassifiesAmbiguousDelivery(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-accepted", true: "after-accepted"}[accepted], func(t *testing.T) {
			transport := newFakeTransport()
			client := connectedAgentClient(t, transport, "operator.write")
			defer client.Close()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { _, err := client.Agent(ctx, validAgentRequest()); done <- err }()
			sent := transport.nextSent(t)
			if accepted {
				transport.deliver(acceptedFrame(sent.String("id"), "run-1"))
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
			err := <-done
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Agent error = %v, want cancellation classification", err)
			}
			want := "ambiguous"
			if accepted {
				want = "outcome is unknown"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("Agent error = %v, want %q", err, want)
			}
		})
	}
}

func TestAgentDropsMissingLateAndDuplicateResponses(t *testing.T) {
	transport := newFakeTransport()
	client := connectedAgentClient(t, transport, "operator.write")
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		result, err := client.Agent(context.Background(), validAgentRequest())
		if err == nil && result.Text != "ok" {
			err = errors.New("wrong result")
		}
		done <- err
	}()
	sent := transport.nextSent(t)
	id := sent.String("id")
	transport.deliver(Frame{"type": "res", "ok": true, "payload": map[string]any{"deviceToken": "secret"}})
	transport.deliver(Frame{"type": "res", "id": "late-old-id", "ok": true, "payload": map[string]any{"deviceToken": "secret"}})
	transport.deliver(acceptedFrame(id, "run-1"))
	transport.deliver(terminalFrame(id, "run-1", "ok", []any{map[string]any{"text": "ok"}}))
	transport.deliver(terminalFrame(id, "run-1", "ok", []any{map[string]any{"text": "duplicate"}}))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if count, bytes := client.queuedEventUsage(); count != 0 || bytes != 0 {
		t.Fatalf("response frames entered event queue: %d/%d", count, bytes)
	}
}

func TestAgentSendFailureIsAmbiguous(t *testing.T) {
	transport := newFakeTransport()
	transport.sendErr = errors.New("partial write")
	client := connectedAgentClient(t, transport, "operator.write")
	if _, err := client.Agent(context.Background(), validAgentRequest()); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Agent error = %v", err)
	}
}

func TestAgentRequestSerializesWithoutSecretsOrRetries(t *testing.T) {
	content, err := json.Marshal(validAgentRequest())
	if err != nil || strings.Contains(string(content), "token") {
		t.Fatalf("request snapshot = %s, %v", content, err)
	}
}

func TestAgentInterleavedResponseStress(t *testing.T) {
	for iteration := 0; iteration < 300; iteration++ {
		transport := newFakeTransport()
		client := connectedAgentClient(t, transport, "operator.write")
		done := make(chan error, 1)
		go func() {
			result, err := client.Agent(context.Background(), validAgentRequest())
			if err == nil && (result.RunID != "run-stress" || result.Text != "ok") {
				err = errors.New("cross-talk result")
			}
			done <- err
		}()
		sent := transport.nextSent(t)
		id := sent.String("id")
		transport.deliver(Frame{"type": "res", "id": "stale", "ok": true, "payload": map[string]any{"status": "accepted", "runId": "wrong"}})
		transport.deliver(Frame{"type": "event", "event": "chat", "payload": map[string]any{"runId": "wrong", "state": "final"}})
		transport.deliver(acceptedFrame(id, "run-stress"))
		transport.deliver(Frame{"type": "res", "id": "other", "ok": true, "payload": map[string]any{"status": "ok", "runId": "run-stress"}})
		transport.deliver(terminalFrame(id, "run-stress", "ok", []any{map[string]any{"text": "ok"}}))
		if err := <-done; err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		if err := client.Close(); err != nil {
			t.Fatalf("iteration %d close: %v", iteration, err)
		}
	}
}

func TestAgentFatalOverflowDominatesQueuedResponsesStress(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		transport := newFakeTransport()
		client := connectedAgentClient(t, transport, "operator.write")
		done := make(chan error, 1)
		go func() { _, err := client.Agent(context.Background(), validAgentRequest()); done <- err }()
		sent := transport.nextSent(t)
		id := sent.String("id")
		client.mu.Lock()
		reply := client.pending[id]
		reply <- responseDelivery{frame: acceptedFrame(id, "run-fatal")}
		reply <- responseDelivery{frame: terminalFrame(id, "run-fatal", "ok", []any{map[string]any{"text": "must-not-win"}})}
		reply <- responseDelivery{frame: terminalFrame(id, "run-fatal", "ok", []any{map[string]any{"text": "duplicate"}})}
		time.Sleep(time.Microsecond)
		client.readerErr = errors.New("gateway response queue overflow")
		client.readerFatal = true
		client.transport = nil
		client.connected = nil
		client.mu.Unlock()
		if err := <-done; err == nil || !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("iteration %d accepted queued response over fatal overflow: %v", iteration, err)
		}
	}
}
