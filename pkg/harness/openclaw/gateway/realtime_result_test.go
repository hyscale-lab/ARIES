package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNewRealtimeResultHasStableArtifactShape(t *testing.T) {
	result := NewRealtimeResult()
	result.Transcript = "heard"
	result.OutputText = "provider"
	result.AgentOutputText = "agent"
	result.ProviderToolQuestion = "question"
	result.AgentQuestionUsed = "question with context"
	result.AgentPromptTokens = 11
	result.AgentCompletionTokens = 7
	result.ToolCalls = 1
	result.ToolResults = 1
	result.AgentConsultOK = true
	result.OutputAudioDone = true
	result.TranscriptDoneParts = append(result.TranscriptDoneParts, "heard")
	result.AgentRunIDs = append(result.AgentRunIDs, "run-1")
	result.ConnectAuth["scopes"] = []any{"operator.write"}
	result.Timings["listen_seconds"] = 2.5
	result.IncrementEvent("transcript.done")
	result.AppendError("diagnostic")
	sessionID := "session-1"
	result.SessionID = &sessionID
	usage := "agent.final"
	result.AgentUsageSource = &usage
	result.Events = append(result.Events, Frame{"type": "event", "event": "talk.event"})

	content, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal realtime result: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("decode realtime result: %v", err)
	}
	for _, key := range []string{
		"schema_version", "transcript", "output_text", "agent_output_text",
		"original_prompt", "transcript_done", "transcript_done_parts",
		"provider_tool_question", "agent_question_used", "error_type",
		"agent_prompt_tokens", "agent_completion_tokens", "agent_usage_source",
		"tool_calls", "tool_results", "agent_consult_ok", "output_audio_done",
		"session_id", "relay_session_id", "agent_run_ids", "event_counts",
		"connect_auth", "errors", "timings", "events",
	} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing key %q in %#v", key, decoded)
		}
	}
	if decoded["schema_version"] != float64(RealtimeResultSchemaVersion) {
		t.Fatalf("schema_version = %v", decoded["schema_version"])
	}
	if decoded["session_id"] != sessionID || decoded["agent_usage_source"] != usage || decoded["relay_session_id"] != nil {
		t.Fatalf("nullable fields = session:%#v usage:%#v relay:%#v", decoded["session_id"], decoded["agent_usage_source"], decoded["relay_session_id"])
	}
	if got := result.FinalText(); got != "agent" {
		t.Fatalf("FinalText() = %q", got)
	}
}

func TestRealtimeResultWithoutEventsKeepsCountersAndDropsRawEvents(t *testing.T) {
	result := NewRealtimeResult()
	result.Events = append(result.Events, Frame{"event": "chat"})
	result.IncrementEvent("chat")

	content, err := json.Marshal(result.WithoutEvents())
	if err != nil {
		t.Fatalf("marshal realtime result without events: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("decode realtime result without events: %v", err)
	}
	if _, ok := decoded["events"]; ok {
		t.Fatalf("events were not omitted: %#v", decoded["events"])
	}
	wantCounts := map[string]any{"chat": float64(1)}
	if !reflect.DeepEqual(decoded["event_counts"], wantCounts) {
		t.Fatalf("event_counts = %#v, want %#v", decoded["event_counts"], wantCounts)
	}
}

func TestRealtimeResultFinalTextFallback(t *testing.T) {
	result := NewRealtimeResult()
	result.Transcript = "transcript"
	if got := result.FinalText(); got != "transcript" {
		t.Fatalf("FinalText transcript = %q", got)
	}
	result.OutputText = "output"
	if got := result.FinalText(); got != "output" {
		t.Fatalf("FinalText output = %q", got)
	}
	result.AgentOutputText = "agent"
	if got := result.FinalText(); got != "agent" {
		t.Fatalf("FinalText agent = %q", got)
	}
}
