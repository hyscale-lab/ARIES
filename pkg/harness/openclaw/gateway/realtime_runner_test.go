package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRealtimeRunnerCreatesSessionStreamsAudioAndHandlesToolCall(t *testing.T) {
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{
			method: "talk.session.create",
			response: Frame{"ok": true, "payload": map[string]any{
				"sessionId": "session-1",
				"audio": map[string]any{
					"inputEncoding":     "pcm16",
					"inputSampleRateHz": 4,
				},
			}},
		},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: Frame{"ok": true, "payload": map[string]any{"runId": "run-1", "answer": "done"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "transcript.done", "sessionId": "session-1",
				"payload": map[string]any{"role": "user", "text": "hello"},
			}},
		},
		Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "call-1",
				"payload": map[string]any{
					"name": "openclaw_agent_consult",
					"args": map[string]any{"question": "what now?", "context": "ctx"},
				},
			}},
		},
		Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{
				"runId":     "run-1",
				"state":     "delta",
				"deltaText": "answer ",
			},
		},
		Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{
				"runId": "run-1",
				"state": "final",
				"message": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "unused"},
				}},
			},
		},
		Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "output.audio.done", "sessionId": "session-1", "payload": map[string]any{},
			}},
		},
	)
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		OriginalPrompt:        "original",
		SessionKey:            "agent:test:main",
		Audio:                 RealtimeAudio{Data: []byte{1, 2, 3, 4}, Rate: 4, BytesPerSample: 2, Encoding: "pcm16"},
		ChunkDuration:         250 * time.Millisecond,
		ListenDuration:        50 * time.Millisecond,
		QuietDuration:         time.Millisecond,
		AgentWaitDuration:     20 * time.Millisecond,
		ToolCallTimeout:       time.Second,
		AppendAudioTimeout:    time.Second,
		AgentQuestionTemplate: "use: {question}",
		IncludeEvents:         true,
		CloseGateway:          true,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}
	runner.sleep = func(context.Context, time.Duration) error { return nil }

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Transcript != "hello" || result.TranscriptDone != "hello" || result.OutputText != "answer " {
		t.Fatalf("text result = %#v", result)
	}
	if result.ProviderToolQuestion != "what now?" || result.AgentQuestionUsed != "use: what now?" {
		t.Fatalf("question fields = %#v", result)
	}
	if result.ToolCalls != 1 || result.ToolResults != 1 || !result.AgentConsultOK || !result.OutputAudioDone {
		t.Fatalf("tool/audio result = %#v", result)
	}
	if len(result.Events) != 5 {
		t.Fatalf("events retained = %d", len(result.Events))
	}
	if result.EventCounts["transcript.done"] != 2 || result.EventCounts["chat.final"] != 1 {
		t.Fatalf("event counts = %#v", result.EventCounts)
	}
	if !gateway.closed {
		t.Fatal("runner did not close gateway")
	}

	appendOne := gateway.requests[1]
	appendTwo := gateway.requests[2]
	if appendOne.method != "talk.session.appendAudio" || appendTwo.method != "talk.session.appendAudio" {
		t.Fatalf("append methods = %#v", gateway.requests)
	}
	if appendOne.params["timestamp"] != 0 || appendTwo.params["timestamp"] != 250 {
		t.Fatalf("append timestamps = %#v %#v", appendOne.params["timestamp"], appendTwo.params["timestamp"])
	}
	if appendOne.params["audioBase64"] != base64.StdEncoding.EncodeToString([]byte{1, 2}) {
		t.Fatalf("first audio chunk = %#v", appendOne.params)
	}
	toolCall := gateway.requests[3]
	params := toolCall.params
	if params["sessionKey"] != "agent:test:main" || params["callId"] != "call-1" || params["relaySessionId"] != "relay-1" {
		t.Fatalf("tool params = %#v", params)
	}
	args := params["args"].(map[string]any)
	if args["question"] != "use: what now?" || args["context"] != "ctx" {
		t.Fatalf("tool args = %#v", args)
	}
	submit := gateway.requests[4]
	if submit.params["sessionId"] != "relay-1" || submit.params["callId"] != "call-1" {
		t.Fatalf("submit params = %#v", submit.params)
	}
}

func TestRealtimeRunnerOmitsEventsByDefaultAndFallsBackToPartialTranscript(t *testing.T) {
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"talkEvent": map[string]any{
			"type": "transcript.delta", "sessionId": "session-1", "payload": map[string]any{"text": "partial"},
		}},
	})
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		Audio:          RealtimeAudio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Transcript != "partial" {
		t.Fatalf("transcript = %q", result.Transcript)
	}
	if result.Events != nil {
		t.Fatalf("events were retained by default: %#v", result.Events)
	}
}

func TestRealtimeRunnerCanPrepareAudioAfterSessionCreate(t *testing.T) {
	var gotSession TalkSessionInfo
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: Frame{"ok": true, "payload": map[string]any{
			"sessionId":      "session-1",
			"relaySessionId": "relay-1",
			"audio":          map[string]any{"inputEncoding": "g711_ulaw", "inputSampleRateHz": 8000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
	)
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		AudioProvider: func(session TalkSessionInfo) (RealtimeAudio, error) {
			gotSession = session
			return RealtimeAudio{Data: []byte{0xff}, Rate: session.InputSampleRateHz, BytesPerSample: 1, Encoding: session.InputEncoding}, nil
		},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if gotSession.InputEncoding != "g711_ulaw" || gotSession.InputSampleRateHz != 8000 {
		t.Fatalf("session = %#v", gotSession)
	}
	if result.RelaySessionID == nil || *result.RelaySessionID != "relay-1" {
		t.Fatalf("result relay = %#v", result.RelaySessionID)
	}
	appendCall := gateway.requests[1]
	if appendCall.params["audioBase64"] != base64.StdEncoding.EncodeToString([]byte{0xff}) {
		t.Fatalf("append params = %#v", appendCall.params)
	}
}

func TestRealtimeRunnerReportsFailedToolAndStillSubmitsResult(t *testing.T) {
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
		scriptedCall{method: "talk.session.submitToolResult", response: Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"talkEvent": map[string]any{
			"type": "tool.call", "sessionId": "session-1", "callId": "call-1",
			"payload": map[string]any{"name": "unknown.tool", "args": map[string]any{}},
		}},
	})
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		Audio:          RealtimeAudio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ToolCalls != 1 || result.ToolResults != 1 || len(result.Errors) != 1 {
		t.Fatalf("result = %#v", result)
	}
	submit := gateway.requests[2]
	payload := submit.params["result"].(map[string]any)
	if payload["error"] != `runner does not handle tool "unknown.tool"` {
		t.Fatalf("submitted tool error = %#v", payload)
	}
}

func TestRealtimeRunnerHandlesAgentControlStatusWithoutStartingAnotherAgent(t *testing.T) {
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: Frame{"ok": true, "payload": map[string]any{"runId": "run-1"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: Frame{"ok": true}},
		scriptedCall{method: "talk.session.submitToolResult", response: Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "consult-1",
				"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
			}},
		},
		Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "control-1",
				"payload": map[string]any{"name": "openclaw_agent_control", "args": map[string]any{"mode": "status", "text": "status"}},
			}},
		},
		Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "final", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}},
		},
	)
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		Audio:                     RealtimeAudio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration:            5 * time.Millisecond,
		QuietDuration:             time.Millisecond,
		AgentWaitDuration:         2 * time.Millisecond,
		AgentWaitFallbackDuration: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ToolCalls != 2 || result.ToolResults != 2 || len(result.Errors) != 0 || len(result.AgentRunIDs) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(gateway.requests) != 5 || gateway.requests[4].method != "talk.session.submitToolResult" {
		t.Fatalf("requests = %#v", gateway.requests)
	}
	controlResult := gateway.requests[4].params["result"].(map[string]any)
	if controlResult["status"] != "working" || !reflect.DeepEqual(controlResult["activeRunIds"], []string{"run-1"}) {
		t.Fatalf("control result = %#v", controlResult)
	}
}

func TestRealtimeRunnerIgnoresRecoveredAgentErrorAfterFinal(t *testing.T) {
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: Frame{"ok": true, "payload": map[string]any{"runId": "run-1"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "consult-1",
				"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
			}},
		},
		Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "error", "errorMessage": "transient exec failed"},
		},
		Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "final", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}},
		},
	)
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		Audio:          RealtimeAudio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(result.Errors) != 0 || result.OutputText != "done" || result.EventCounts["chat.error"] != 1 || result.EventCounts["chat.final"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRealtimeRunnerIgnoresLateAgentErrorAfterFinal(t *testing.T) {
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: Frame{"ok": true, "payload": map[string]any{"runId": "run-1"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "consult-1",
				"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
			}},
		},
		Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "final", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}},
		},
		Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "error", "errorMessage": "late exec failed"},
		},
	)
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		Audio:          RealtimeAudio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(result.Errors) != 0 || result.OutputText != "done" || result.EventCounts["chat.error"] != 1 || result.EventCounts["chat.final"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRealtimeRunnerSubmitsAgentConsultFailureAsToolResult(t *testing.T) {
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: Frame{"ok": false, "error": map[string]any{"code": "bridge_failed"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
			"type": "tool.call", "sessionId": "session-1", "callId": "call-1",
			"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
		}},
	})
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		Audio:          RealtimeAudio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ToolCalls != 1 || result.ToolResults != 1 || result.AgentConsultOK || len(result.AgentRunIDs) != 0 || len(result.Errors) != 1 {
		t.Fatalf("result = %#v", result)
	}
	submit := gateway.requests[3]
	if submit.method != "talk.session.submitToolResult" {
		t.Fatalf("submit method = %q", submit.method)
	}
	toolResult := submit.params["result"].(map[string]any)
	if got := toolResult["error"].(string); !strings.Contains(got, "talk.client.toolCall failed") || !strings.Contains(got, "bridge_failed") {
		t.Fatalf("submitted failure = %#v", toolResult)
	}
}

func TestRealtimeRunnerUsesInjectedToolHandlerBeforeSubmitting(t *testing.T) {
	handler := &capturingRealtimeToolHandler{outcome: RealtimeToolCallResult{
		ToolResult:           map[string]any{"ok": true},
		AgentRunID:           "run-custom",
		ProviderToolQuestion: "provider question",
		AgentQuestionUsed:    "agent question",
		AgentConsultOK:       true,
	}}
	gateway := newScriptedRealtimeGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: Frame{"ok": true}},
		scriptedCall{method: "talk.session.submitToolResult", response: Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"talkEvent": map[string]any{
			"type": "tool.call", "sessionId": "session-1", "callId": "call-1",
			"payload": map[string]any{"name": "custom", "args": map[string]any{"question": "q"}},
		}},
	})
	runner, err := NewRealtimeRunner(gateway, RealtimeRunnerOptions{
		SessionKey:                "session-key",
		ToolHandler:               handler,
		Audio:                     RealtimeAudio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration:            5 * time.Millisecond,
		QuietDuration:             time.Millisecond,
		AgentWaitDuration:         2 * time.Millisecond,
		AgentWaitFallbackDuration: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if handler.calls != 1 || handler.request.SessionKey != "session-key" || handler.request.ToolCall.Name != "custom" {
		t.Fatalf("handler calls=%d request=%#v", handler.calls, handler.request)
	}
	if result.AgentRunIDs[0] != "run-custom" || result.ProviderToolQuestion != "provider question" || result.AgentQuestionUsed != "agent question" || !result.AgentConsultOK {
		t.Fatalf("result = %#v", result)
	}
	submit := gateway.requests[2]
	if !reflect.DeepEqual(submit.params["result"], map[string]any{"ok": true}) {
		t.Fatalf("submitted result = %#v", submit.params["result"])
	}
}

type scriptedCall struct {
	method   string
	response Frame
}

type realtimeRequest struct {
	method string
	params map[string]any
}

type scriptedRealtimeGateway struct {
	connectPayload map[string]any
	calls          []scriptedCall
	events         []Frame
	requests       []realtimeRequest
	closed         bool
}

func newScriptedRealtimeGateway() *scriptedRealtimeGateway {
	return &scriptedRealtimeGateway{connectPayload: map[string]any{"auth": map[string]any{"scopes": []any{"operator.write"}}}}
}

func (gateway *scriptedRealtimeGateway) Connect(context.Context, ConnectOptions) (map[string]any, error) {
	return gateway.connectPayload, nil
}

func (gateway *scriptedRealtimeGateway) Call(_ context.Context, method string, params map[string]any) (Frame, error) {
	gateway.requests = append(gateway.requests, realtimeRequest{method: method, params: cloneMap(params)})
	if len(gateway.calls) == 0 {
		return nil, errors.New("unexpected call " + method)
	}
	next := gateway.calls[0]
	gateway.calls = gateway.calls[1:]
	if next.method != method {
		return nil, errors.New("call method = " + method + ", want " + next.method)
	}
	return next.response, nil
}

func (gateway *scriptedRealtimeGateway) RecvEvent(ctx context.Context) (Frame, error) {
	if len(gateway.events) == 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	next := gateway.events[0]
	gateway.events = gateway.events[1:]
	return next, nil
}

func (gateway *scriptedRealtimeGateway) Close() error {
	gateway.closed = true
	return nil
}

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

type capturingRealtimeToolHandler struct {
	calls   int
	request RealtimeToolCallRequest
	outcome RealtimeToolCallResult
	err     error
}

func (handler *capturingRealtimeToolHandler) HandleRealtimeToolCall(_ context.Context, request RealtimeToolCallRequest) (RealtimeToolCallResult, error) {
	handler.calls++
	handler.request = request
	return handler.outcome, handler.err
}

func TestRealtimeRunnerSessionParams(t *testing.T) {
	vad := 0.7
	silence := 900
	padding := 120
	runner, err := NewRealtimeRunner(newScriptedRealtimeGateway(), RealtimeRunnerOptions{
		SessionKey:            "session-key",
		Provider:              "openai",
		Model:                 "gpt-realtime",
		Voice:                 "alloy",
		ReasoningEffort:       "low",
		VADThreshold:          &vad,
		SilenceDurationMillis: &silence,
		PrefixPaddingMillis:   &padding,
		Audio:                 RealtimeAudio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
	})
	if err != nil {
		t.Fatalf("NewRealtimeRunner returned error: %v", err)
	}
	want := map[string]any{
		"sessionKey":        "session-key",
		"mode":              "realtime",
		"transport":         "gateway-relay",
		"brain":             "agent-consult",
		"vadThreshold":      0.7,
		"silenceDurationMs": 900,
		"prefixPaddingMs":   120,
		"provider":          "openai",
		"model":             "gpt-realtime",
		"voice":             "alloy",
		"reasoningEffort":   "low",
	}
	if got := runner.sessionParams(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sessionParams = %#v, want %#v", got, want)
	}
}
