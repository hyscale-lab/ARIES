package realtime

import "github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"

const ResultSchemaVersion = 1

type Result struct {
	SchemaVersion         int             `json:"schema_version"`
	Transcript            string          `json:"transcript"`
	OutputText            string          `json:"output_text"`
	AgentOutputText       string          `json:"agent_output_text"`
	OriginalPrompt        string          `json:"original_prompt"`
	TranscriptDone        string          `json:"transcript_done"`
	TranscriptDoneParts   []string        `json:"transcript_done_parts"`
	ProviderToolQuestion  string          `json:"provider_tool_question"`
	AgentQuestionUsed     string          `json:"agent_question_used"`
	ErrorType             *string         `json:"error_type"`
	AgentPromptTokens     int             `json:"agent_prompt_tokens"`
	AgentCompletionTokens int             `json:"agent_completion_tokens"`
	AgentUsageSource      *string         `json:"agent_usage_source"`
	ToolCalls             int             `json:"tool_calls"`
	ToolResults           int             `json:"tool_results"`
	AgentConsultOK        bool            `json:"agent_consult_ok"`
	OutputAudioDone       bool            `json:"output_audio_done"`
	SessionID             *string         `json:"session_id"`
	RelaySessionID        *string         `json:"relay_session_id"`
	AgentRunIDs           []string        `json:"agent_run_ids"`
	EventCounts           map[string]int  `json:"event_counts"`
	ConnectAuth           map[string]any  `json:"connect_auth"`
	Errors                []string        `json:"errors"`
	Timings               map[string]any  `json:"timings"`
	Events                []gateway.Frame `json:"events,omitempty"`
}

func newResult() Result {
	return Result{
		SchemaVersion:       ResultSchemaVersion,
		TranscriptDoneParts: []string{},
		AgentRunIDs:         []string{},
		EventCounts:         map[string]int{},
		ConnectAuth:         map[string]any{},
		Errors:              []string{},
		Timings:             map[string]any{},
		Events:              []gateway.Frame{},
	}
}

func (result Result) WithoutEvents() Result {
	result.Events = nil
	return result
}

func (result *Result) IncrementEvent(name string) {
	if name == "" {
		return
	}
	if result.EventCounts == nil {
		result.EventCounts = map[string]int{}
	}
	result.EventCounts[name]++
}

func (result *Result) AppendError(message string) {
	if message == "" {
		return
	}
	if result.Errors == nil {
		result.Errors = []string{}
	}
	result.Errors = append(result.Errors, message)
}

func (result Result) FinalText() string {
	if result.AgentOutputText != "" {
		return result.AgentOutputText
	}
	if result.OutputText != "" {
		return result.OutputText
	}
	return result.Transcript
}
