package realtime

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gatewayclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
)

func TestRunnerCreatesSessionStreamsAudioAndHandlesToolCall(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{
			method: "talk.session.create",
			response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
				"sessionId": "session-1",
				"audio": map[string]any{
					"inputEncoding":     "pcm16",
					"inputSampleRateHz": 4,
				},
			}},
		},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{"runId": "run-1", "answer": "done"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "transcript.done", "sessionId": "session-1",
				"payload": map[string]any{"role": "user", "text": "hello"},
			}},
		},
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "call-1",
				"payload": map[string]any{
					"name": "openclaw_agent_consult",
					"args": map[string]any{"question": "what now?", "context": "ctx"},
				},
			}},
		},
		gatewayclient.Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{
				"runId":     "run-1",
				"state":     "delta",
				"deltaText": "answer ",
			},
		},
		gatewayclient.Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{
				"runId": "run-1",
				"state": "final",
				"message": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "unused"},
				}},
			},
		},
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "output.audio.done", "sessionId": "session-1", "payload": map[string]any{},
			}},
		},
	)
	runner, err := New(gateway, Options{
		OriginalPrompt:        "original",
		SessionKey:            "agent:test:main",
		Audio:                 Audio{Data: []byte{1, 2, 3, 4}, Rate: 4, BytesPerSample: 2, Encoding: "pcm16"},
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
		t.Fatalf("New returned error: %v", err)
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

func TestRunnerProcessesEventsWhileStreamingAudio(t *testing.T) {
	gateway := &blockingAppendGateway{eventStarted: make(chan struct{})}
	runner, err := New(gateway, Options{
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Transcript != "streamed while appending" {
		t.Fatalf("transcript = %q", result.Transcript)
	}
	gateway.mu.Lock()
	appendObserved := gateway.appendObserved
	gateway.mu.Unlock()
	if !appendObserved {
		t.Fatal("appendAudio did not run")
	}
}

func TestRunnerDrainsManyEventsWhileStreamingAudio(t *testing.T) {
	const streamedEvents = 2500
	gateway := newStreamingEventsGateway(streamedEvents)
	runner, err := New(gateway, Options{
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 20 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Transcript != "complete transcript" {
		t.Fatalf("transcript = %q", result.Transcript)
	}
	if got := result.EventCounts["input.audio.delta"]; got < streamedEvents {
		t.Fatalf("input.audio.delta count = %d, want at least %d", got, streamedEvents)
	}
}

func TestRunnerOmitsEventsByDefaultAndFallsBackToPartialTranscript(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, gatewayclient.Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"talkEvent": map[string]any{
			"type": "transcript.delta", "sessionId": "session-1", "payload": map[string]any{"text": "partial"},
		}},
	})
	runner, err := New(gateway, Options{
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
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

func TestRunnerTranscribeClosesSessionAfterFinalTranscript(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.session.close", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, gatewayclient.Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"talkEvent": map[string]any{
			"type": "transcript.done", "sessionId": "session-1", "payload": map[string]any{"role": "user", "text": "transcribed task"},
		}},
	})
	runner, err := New(gateway, Options{
		SessionMode:    SessionModeTranscribe,
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Transcript != "transcribed task" || result.TranscriptDone != "transcribed task" {
		t.Fatalf("transcript result = %#v", result)
	}
	if len(gateway.requests) != 3 || gateway.requests[2].method != "talk.session.close" || gateway.requests[2].params["sessionId"] != "session-1" {
		t.Fatalf("requests = %#v", gateway.requests)
	}
}

func TestRunnerTranscribeAcceptsFinalTranscriptWithoutRole(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.session.close", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, gatewayclient.Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{
			"final": true,
			"text":  "transcribed task",
			"type":  "transcript",
			"talkEvent": map[string]any{
				"type": "transcript.done", "sessionId": "session-1", "payload": map[string]any{"text": "transcribed task"},
			},
		},
	})
	runner, err := New(gateway, Options{
		SessionMode:    SessionModeTranscribe,
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Transcript != "transcribed task" || result.TranscriptDone != "transcribed task" {
		t.Fatalf("transcript result = %#v", result)
	}
}

func TestRunnerTranscribeRequiresFinalTranscript(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.session.close", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, gatewayclient.Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"talkEvent": map[string]any{
			"type": "transcript.delta", "sessionId": "session-1", "payload": map[string]any{"text": "partial"},
		}},
	})
	runner, err := New(gateway, Options{
		SessionMode:    SessionModeTranscribe,
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing_final_transcript") || len(result.Errors) != 1 {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if len(gateway.requests) != 3 || gateway.requests[2].method != "talk.session.close" || gateway.requests[2].params["sessionId"] != "session-1" {
		t.Fatalf("requests = %#v", gateway.requests)
	}
}

func TestRunnerTranscribeRejectsUnfinalizedTrailingPartial(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.session.close", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "transcript.done", "sessionId": "session-1", "payload": map[string]any{"text": "first segment"},
			}},
		},
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "transcript.delta", "sessionId": "session-1", "payload": map[string]any{"text": "unfinished second segment"},
			}},
		},
	)
	runner, err := New(gateway, Options{
		SessionMode:    SessionModeTranscribe,
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "incomplete_final_transcript") || len(result.Errors) != 1 {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if result.Transcript != "first segment" || result.TranscriptDone != "first segment" {
		t.Fatalf("transcript result = %#v", result)
	}
}

func TestRunnerTranscribeClosesSessionAfterAppendFailure(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{
			"ok": false, "error": map[string]any{"code": "APPEND_FAILED", "message": "append rejected"},
		}},
		scriptedCall{method: "talk.session.close", response: gatewayclient.Frame{
			"ok": false, "error": map[string]any{"code": "CLOSE_FAILED", "message": "close rejected"},
		}},
	)
	runner, err := New(gateway, Options{
		SessionMode:    SessionModeTranscribe,
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "append rejected") || !strings.Contains(err.Error(), "close rejected") {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if len(result.Errors) != 2 || !strings.Contains(result.Errors[0], "append rejected") || !strings.Contains(result.Errors[1], "close rejected") {
		t.Fatalf("result errors = %#v", result.Errors)
	}
	if len(gateway.requests) != 3 || gateway.requests[2].method != "talk.session.close" || gateway.requests[2].params["sessionId"] != "session-1" {
		t.Fatalf("requests = %#v", gateway.requests)
	}
}

func TestRunnerCanPrepareAudioAfterSessionCreate(t *testing.T) {
	var gotSession SessionInfo
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId":      "session-1",
			"relaySessionId": "relay-1",
			"audio":          map[string]any{"inputEncoding": "g711_ulaw", "inputSampleRateHz": 8000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
	)
	runner, err := New(gateway, Options{
		AudioProvider: func(session SessionInfo) (Audio, error) {
			gotSession = session
			return Audio{Data: []byte{0xff}, Rate: session.InputSampleRateHz, BytesPerSample: 1, Encoding: session.InputEncoding}, nil
		},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
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

func TestRunnerReportsFailedToolAndStillSubmitsResult(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.session.submitToolResult", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, gatewayclient.Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"talkEvent": map[string]any{
			"type": "tool.call", "sessionId": "session-1", "callId": "call-1",
			"payload": map[string]any{"name": "unknown.tool", "args": map[string]any{}},
		}},
	})
	runner, err := New(gateway, Options{
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
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

func TestRunnerHandlesAgentControlStatusWithoutStartingAnotherAgent(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{"runId": "run-1"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.session.submitToolResult", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "consult-1",
				"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
			}},
		},
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "control-1",
				"payload": map[string]any{"name": "openclaw_agent_control", "args": map[string]any{"mode": "status", "text": "status"}},
			}},
		},
		gatewayclient.Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "final", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}},
		},
	)
	runner, err := New(gateway, Options{
		Audio:                     Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration:            5 * time.Millisecond,
		QuietDuration:             time.Millisecond,
		AgentWaitDuration:         2 * time.Millisecond,
		AgentWaitFallbackDuration: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
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

func TestRunnerIgnoresRecoveredAgentErrorAfterFinal(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{"runId": "run-1"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "consult-1",
				"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
			}},
		},
		gatewayclient.Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "error", "errorMessage": "transient exec failed"},
		},
		gatewayclient.Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "final", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}},
		},
	)
	runner, err := New(gateway, Options{
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(result.Errors) != 0 || result.OutputText != "done" || result.EventCounts["chat.error"] != 1 || result.EventCounts["chat.final"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunnerIgnoresLateAgentErrorAfterFinal(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{"runId": "run-1"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events,
		gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
				"type": "tool.call", "sessionId": "session-1", "callId": "consult-1",
				"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
			}},
		},
		gatewayclient.Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "final", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}},
		},
		gatewayclient.Frame{
			"type": "event", "event": "chat",
			"payload": map[string]any{"runId": "run-1", "state": "error", "errorMessage": "late exec failed"},
		},
	)
	runner, err := New(gateway, Options{
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(result.Errors) != 0 || result.OutputText != "done" || result.EventCounts["chat.error"] != 1 || result.EventCounts["chat.final"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunnerSubmitsAgentConsultFailureAsToolResult(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1",
			"audio":     map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
		scriptedCall{method: "talk.client.toolCall", response: gatewayclient.Frame{"ok": false, "error": map[string]any{"code": "bridge_failed"}}},
		scriptedCall{method: "talk.session.submitToolResult", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, gatewayclient.Frame{
		"type": "event", "event": "talk.event",
		"payload": map[string]any{"relaySessionId": "relay-1", "talkEvent": map[string]any{
			"type": "tool.call", "sessionId": "session-1", "callId": "call-1",
			"payload": map[string]any{"name": "openclaw_agent_consult", "args": map[string]any{"question": "q"}},
		}},
	})
	runner, err := New(gateway, Options{
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: 5 * time.Millisecond,
		QuietDuration:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
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
	if got := toolResult["error"].(string); !strings.Contains(got, "talk.client.toolCall rejected request") || !strings.Contains(got, "bridge_failed") {
		t.Fatalf("submitted failure = %#v", toolResult)
	}
}

func TestRunnerProtocolErrorsDoNotRenderSecretDetails(t *testing.T) {
	secret := "gateway-auth-secret-canary"
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls, scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{
		"ok":    false,
		"error": map[string]any{"code": "DENIED", "message": "request rejected", "details": map[string]any{"token": secret}},
	}})
	runner, err := New(gateway, Options{Audio: Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2}})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := runner.Run(context.Background())
	if runErr == nil || strings.Contains(runErr.Error(), secret) || strings.Contains(strings.Join(result.Errors, "\n"), secret) {
		t.Fatalf("result/error leaked protocol details: %#v / %v", result, runErr)
	}
}

func TestRunnerDeadlineDoesNotAcceptPartialEventBeforeFatalOverflow(t *testing.T) {
	gateway := newScriptedGateway()
	gateway.calls = append(gateway.calls,
		scriptedCall{method: "talk.session.create", response: gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1", "audio": map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}},
		scriptedCall{method: "talk.session.appendAudio", response: gatewayclient.Frame{"ok": true}},
	)
	gateway.events = append(gateway.events, gatewayclient.Frame{
		"type": "event", "event": "talk.event", "payload": map[string]any{"talkEvent": map[string]any{
			"type": "transcript.delta", "sessionId": "session-1", "payload": map[string]any{"text": "partial-must-not-succeed"},
		}},
	})
	gateway.recvDelay = 5 * time.Millisecond
	gateway.recvErr = errors.New("gateway event queue overflow")
	runner, err := New(gateway, Options{
		Audio:          Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		ListenDuration: time.Millisecond, QuietDuration: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := runner.Run(context.Background())
	if runErr == nil || !strings.Contains(runErr.Error(), "event queue overflow") {
		t.Fatalf("Run accepted boundary partial result: %#v, %v", result, runErr)
	}
	if result.Transcript != "" || result.OutputText != "" {
		t.Fatalf("fatal overflow exposed partial success: %#v", result)
	}
}

func TestRunnerRejectsHugeOrMisalignedAudioBeforeGatewayUse(t *testing.T) {
	tests := []Audio{
		{Data: []byte{1, 2}, Rate: maxRealtimeSampleRate + 1, BytesPerSample: 2},
		{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: maxRealtimeBytesPerSample + 1},
		{Data: []byte{1, 2, 3}, Rate: 24000, BytesPerSample: 2},
	}
	for _, audio := range tests {
		if _, err := New(newScriptedGateway(), Options{Audio: audio}); err == nil || !strings.Contains(err.Error(), "bounds") {
			t.Fatalf("New(%#v) error = %v", audio, err)
		}
	}
	if _, err := New(newScriptedGateway(), Options{Audio: Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2}, ChunkDuration: maxRealtimeChunkDuration + 1}); err == nil || !strings.Contains(err.Error(), "duration") {
		t.Fatalf("huge chunk duration error = %v", err)
	}
}

func TestSessionInfoExtractionDoesNotRenderPayload(t *testing.T) {
	secret := "tts-secret-canary"
	_, err := sessionInfoFromPayload(map[string]any{"auth": map[string]any{"token": secret}})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("sessionInfoFromPayload error = %v", err)
	}
}

type scriptedCall struct {
	method   string
	response gatewayclient.Frame
}

type realtimeRequest struct {
	method string
	params map[string]any
}

type scriptedGateway struct {
	mu                       sync.Mutex
	connectPayload           gatewayclient.ConnectSummary
	calls                    []scriptedCall
	events                   []gatewayclient.Frame
	requests                 []realtimeRequest
	closed                   bool
	recvDelay                time.Duration
	recvErr                  error
	releaseEventsWhileAppend bool
}

func newScriptedGateway() *scriptedGateway {
	return &scriptedGateway{connectPayload: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}}
}

func TestRealtimeRequiresReadAndWriteScopesBeforeSessionCreation(t *testing.T) {
	for _, scopes := range [][]string{{"operator.read"}, {"operator.write"}, nil} {
		gateway := newScriptedGateway()
		gateway.connectPayload.Scopes = scopes
		runner, err := New(gateway, Options{
			Audio: Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "operator.read and operator.write") {
			t.Fatalf("Run error = %v for scopes %#v", err, scopes)
		}
		if len(gateway.requests) != 0 {
			t.Fatalf("realtime sent work without scopes %#v: %#v", scopes, gateway.requests)
		}
	}
}

func (gateway *scriptedGateway) Connect(context.Context, gatewayclient.ConnectOptions) (gatewayclient.ConnectSummary, error) {
	return gateway.connectPayload, nil
}

func (gateway *scriptedGateway) Call(_ context.Context, method string, params map[string]any) (gatewayclient.Frame, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
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

func (gateway *scriptedGateway) RecvEvent(ctx context.Context) (gatewayclient.Frame, error) {
	for {
		gateway.mu.Lock()
		if len(gateway.events) != 0 && (gateway.releaseEventsWhileAppend || !gateway.hasPendingAppendLocked()) {
			next := gateway.events[0]
			gateway.events = gateway.events[1:]
			delay := gateway.recvDelay
			gateway.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			return next, nil
		}
		if len(gateway.events) == 0 && gateway.recvErr != nil {
			err := gateway.recvErr
			gateway.mu.Unlock()
			return nil, err
		}
		gateway.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (gateway *scriptedGateway) hasPendingAppendLocked() bool {
	for _, call := range gateway.calls {
		if call.method == methodAppendAudio {
			return true
		}
	}
	return false
}

func (gateway *scriptedGateway) Close() error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.closed = true
	return nil
}

func (gateway *scriptedGateway) FatalError() error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return gateway.recvErr
}

type blockingAppendGateway struct {
	mu             sync.Mutex
	eventStarted   chan struct{}
	once           sync.Once
	eventSent      bool
	appendObserved bool
}

func (gateway *blockingAppendGateway) Connect(context.Context, gatewayclient.ConnectOptions) (gatewayclient.ConnectSummary, error) {
	return gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}, nil
}

func (gateway *blockingAppendGateway) Call(ctx context.Context, method string, _ map[string]any) (gatewayclient.Frame, error) {
	switch method {
	case methodSessionCreate:
		return gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1", "audio": map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}, nil
	case methodAppendAudio:
		gateway.mu.Lock()
		gateway.appendObserved = true
		gateway.mu.Unlock()
		select {
		case <-gateway.eventStarted:
			return gatewayclient.Frame{"ok": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		return nil, errors.New("unexpected call " + method)
	}
}

func (gateway *blockingAppendGateway) RecvEvent(ctx context.Context) (gatewayclient.Frame, error) {
	gateway.once.Do(func() { close(gateway.eventStarted) })
	gateway.mu.Lock()
	if !gateway.eventSent {
		gateway.eventSent = true
		gateway.mu.Unlock()
		return gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "transcript.done", "sessionId": "session-1", "payload": map[string]any{"role": "user", "text": "streamed while appending"},
			}},
		}, nil
	}
	gateway.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*blockingAppendGateway) FatalError() error { return nil }
func (*blockingAppendGateway) Close() error      { return nil }

type streamingEventsGateway struct {
	mu              sync.Mutex
	total           int
	sent            int
	appendCanReturn chan struct{}
	closed          bool
}

func newStreamingEventsGateway(total int) *streamingEventsGateway {
	return &streamingEventsGateway{total: total, appendCanReturn: make(chan struct{})}
}

func (gateway *streamingEventsGateway) Connect(context.Context, gatewayclient.ConnectOptions) (gatewayclient.ConnectSummary, error) {
	return gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}, nil
}

func (gateway *streamingEventsGateway) Call(ctx context.Context, method string, _ map[string]any) (gatewayclient.Frame, error) {
	switch method {
	case methodSessionCreate:
		return gatewayclient.Frame{"ok": true, "payload": map[string]any{
			"sessionId": "session-1", "audio": map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000},
		}}, nil
	case methodAppendAudio:
		select {
		case <-gateway.appendCanReturn:
			return gatewayclient.Frame{"ok": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		return nil, errors.New("unexpected call " + method)
	}
}

func (gateway *streamingEventsGateway) RecvEvent(ctx context.Context) (gatewayclient.Frame, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if gateway.sent < gateway.total {
		gateway.sent++
		if gateway.sent == gateway.total {
			close(gateway.appendCanReturn)
		}
		return gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "input.audio.delta", "sessionId": "session-1", "payload": map[string]any{},
			}},
		}, nil
	}
	if gateway.sent == gateway.total {
		gateway.sent++
		return gatewayclient.Frame{
			"type": "event", "event": "talk.event",
			"payload": map[string]any{"talkEvent": map[string]any{
				"type": "transcript.done", "sessionId": "session-1", "payload": map[string]any{"role": "user", "text": "complete transcript"},
			}},
		}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (gateway *streamingEventsGateway) Close() error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.closed = true
	return nil
}

func (*streamingEventsGateway) FatalError() error { return nil }

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func TestRunnerSessionParams(t *testing.T) {
	vad := 0.7
	silence := 900
	padding := 120
	runner, err := New(newScriptedGateway(), Options{
		SessionKey:            "session-key",
		Provider:              "openai",
		Model:                 "gpt-realtime",
		Voice:                 "alloy",
		ReasoningEffort:       "low",
		VADThreshold:          &vad,
		SilenceDurationMillis: &silence,
		PrefixPaddingMillis:   &padding,
		Audio:                 Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
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

func TestRunnerTranscribeSessionParams(t *testing.T) {
	runner, err := New(newScriptedGateway(), Options{
		SessionMode: SessionModeTranscribe,
		SessionKey:  "session-key",
		Provider:    "openai",
		Model:       "gpt-4o-mini-transcribe",
		Audio:       Audio{Data: []byte{1, 2}, Rate: 24000, BytesPerSample: 2},
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	want := map[string]any{
		"sessionKey": "session-key",
		"mode":       "transcription",
		"transport":  "gateway-relay",
		"brain":      "none",
		"provider":   "openai",
		"model":      "gpt-4o-mini-transcribe",
	}
	if got := runner.sessionParams(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sessionParams = %#v, want %#v", got, want)
	}
}
