package realtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
)

func TestNewResultHasStableArtifactShape(t *testing.T) {
	result := newResult()
	result.Transcript = "heard"
	result.OutputText = "provider"
	result.ProviderToolQuestion = "question"
	result.AgentQuestionUsed = "question with context"
	result.ToolCalls = 1
	result.ToolResults = 1
	result.AgentConsultOK = true
	result.OutputAudioDone = true
	result.TranscriptDoneParts = append(result.TranscriptDoneParts, "heard")
	result.AgentRunIDs = append(result.AgentRunIDs, "run-1")
	result.ConnectAuth["scopes"] = []any{"operator.write"}
	result.IncrementEvent("transcript.done")
	result.AppendError("diagnostic")
	sessionID := "session-1"
	result.SessionID = &sessionID
	result.Events = append(result.Events, gateway.Frame{"type": "event", "event": "talk.event"})

	content, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal realtime result: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("decode realtime result: %v", err)
	}
	for _, key := range []string{
		"schema_version", "transcript", "output_text",
		"original_prompt", "transcript_done", "transcript_done_parts",
		"provider_tool_question", "agent_question_used",
		"tool_calls", "tool_results", "agent_consult_ok", "output_audio_done",
		"session_id", "relay_session_id", "agent_run_ids", "event_counts",
		"connect_auth", "errors", "events",
	} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing key %q in %#v", key, decoded)
		}
	}
	if decoded["schema_version"] != float64(ResultSchemaVersion) {
		t.Fatalf("schema_version = %v", decoded["schema_version"])
	}
	if decoded["session_id"] != sessionID || decoded["relay_session_id"] != nil {
		t.Fatalf("nullable fields = session:%#v relay:%#v", decoded["session_id"], decoded["relay_session_id"])
	}
	if got := result.FinalText(); got != "provider" {
		t.Fatalf("FinalText() = %q", got)
	}
}

func TestResultWithoutEventsKeepsCountersAndDropsRawEvents(t *testing.T) {
	result := newResult()
	result.Events = append(result.Events, gateway.Frame{"event": "chat"})
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

func TestResultFinalTextFallback(t *testing.T) {
	result := newResult()
	result.Transcript = "transcript"
	if got := result.FinalText(); got != "transcript" {
		t.Fatalf("FinalText transcript = %q", got)
	}
	result.OutputText = "output"
	if got := result.FinalText(); got != "output" {
		t.Fatalf("FinalText output = %q", got)
	}
}
