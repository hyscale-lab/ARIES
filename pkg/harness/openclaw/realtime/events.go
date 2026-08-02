package realtime

import (
	"fmt"

	"github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
)

type SessionInfo struct {
	SessionID         string
	RelaySessionID    string
	InputEncoding     string
	InputSampleRateHz int
}

type talkEvent struct {
	EventType string
	SessionID string
	Payload   map[string]any
	Wrapper   map[string]any
	CallID    string
}

type toolCallEvent struct {
	SessionID      string
	RelaySessionID string
	CallID         string
	Name           string
	Args           map[string]any
}

type chatEvent struct {
	RunID        string
	State        string
	DeltaText    string
	Replace      bool
	MessageText  string
	ErrorMessage string
	ErrorKind    string
	StopReason   string
	Stream       string
	Data         map[string]any
}

func toMap(value any) map[string]any { return gateway.Map(value) }

func textFromPayload(payload any) string {
	mapped := toMap(payload)
	text, _ := mapped["text"].(string)
	return text
}

func sessionInfoFromPayload(payload any) (SessionInfo, error) {
	mapped := toMap(payload)
	sessionID, _ := mapped["sessionId"].(string)
	if sessionID == "" {
		return SessionInfo{}, fmt.Errorf("talk.session.create payload missing sessionId")
	}
	relaySessionID, _ := mapped["relaySessionId"].(string)
	audio := toMap(mapped["audio"])
	encoding, _ := audio["inputEncoding"].(string)
	if encoding == "" {
		return SessionInfo{}, fmt.Errorf("talk.session.create audio missing inputEncoding")
	}
	rate, ok := numericInt(audio["inputSampleRateHz"])
	if !ok || rate <= 0 || rate > maxRealtimeSampleRate {
		return SessionInfo{}, fmt.Errorf("talk.session.create audio missing inputSampleRateHz")
	}
	return SessionInfo{SessionID: sessionID, RelaySessionID: relaySessionID, InputEncoding: encoding, InputSampleRateHz: rate}, nil
}

func talkEventFromFrame(frame any) (talkEvent, bool) {
	mapped := toMap(frame)
	if mapped["type"] != "event" || mapped["event"] != "talk.event" {
		return talkEvent{}, false
	}
	wrapper := toMap(mapped["payload"])
	raw := toMap(wrapper["talkEvent"])
	eventType, _ := raw["type"].(string)
	sessionID, _ := raw["sessionId"].(string)
	payload := toMap(raw["payload"])
	if eventType == "" || sessionID == "" {
		return talkEvent{}, false
	}
	callID, _ := raw["callId"].(string)
	return talkEvent{EventType: eventType, SessionID: sessionID, Payload: payload, Wrapper: wrapper, CallID: callID}, true
}

func toolCallEventFromTalk(event talkEvent) (toolCallEvent, bool) {
	if event.EventType != "tool.call" || event.CallID == "" {
		return toolCallEvent{}, false
	}
	name, _ := event.Payload["name"].(string)
	args := toMap(event.Payload["args"])
	if name == "" {
		return toolCallEvent{}, false
	}
	relaySessionID, _ := event.Wrapper["relaySessionId"].(string)
	return toolCallEvent{SessionID: event.SessionID, RelaySessionID: relaySessionID, CallID: event.CallID, Name: name, Args: args}, true
}

func chatEventFromFrame(frame any) (chatEvent, bool) {
	mapped := toMap(frame)
	if mapped["type"] != "event" || mapped["event"] != "chat" {
		return chatEvent{}, false
	}
	payload := toMap(mapped["payload"])
	runID, _ := payload["runId"].(string)
	state, _ := payload["state"].(string)
	if runID == "" || state == "" {
		return chatEvent{}, false
	}
	deltaText, _ := payload["deltaText"].(string)
	errorMessage, _ := payload["errorMessage"].(string)
	errorKind, _ := payload["errorKind"].(string)
	stopReason, _ := payload["stopReason"].(string)
	stream, _ := payload["stream"].(string)
	return chatEvent{RunID: runID, State: state, DeltaText: deltaText, Replace: truthy(payload["replace"]), MessageText: textFromChatMessage(payload["message"]), ErrorMessage: errorMessage, ErrorKind: errorKind, StopReason: stopReason, Stream: stream, Data: toMap(payload["data"])}, true
}

func textFromChatMessage(message any) string {
	mapped := toMap(message)
	rawContent, _ := mapped["content"].([]any)
	var out string
	for _, item := range rawContent {
		part := toMap(item)
		if part["type"] == "text" {
			text, _ := part["text"].(string)
			out += text
		}
	}
	return out
}

func truthy(value any) bool { typed, _ := value.(bool); return typed }

func numericInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	case interface{ Int64() (int64, error) }:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}
