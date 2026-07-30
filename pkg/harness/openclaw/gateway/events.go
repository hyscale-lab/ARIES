package gateway

import "fmt"

type Frame map[string]any

type TalkSessionInfo struct {
	SessionID         string
	RelaySessionID    string
	InputEncoding     string
	InputSampleRateHz int
}

type TalkEvent struct {
	EventType string
	SessionID string
	Payload   map[string]any
	Wrapper   map[string]any
	Raw       map[string]any
	Final     bool
	CallID    string
	ItemID    string
}

type ToolCallEvent struct {
	SessionID      string
	RelaySessionID string
	CallID         string
	Name           string
	Args           map[string]any
}

type ChatEvent struct {
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

func (frame Frame) String(key string) string {
	value, _ := frame[key].(string)
	return value
}

func (frame Frame) Bool(key string) bool {
	value, _ := frame[key].(bool)
	return value
}

func (frame Frame) Map(key string) map[string]any {
	return toMap(frame[key])
}

func toMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	if typed, ok := value.(Frame); ok {
		return map[string]any(typed)
	}
	return map[string]any{}
}

func TextFromPayload(payload any) string {
	mapped := toMap(payload)
	text, _ := mapped["text"].(string)
	return text
}

func TalkSessionInfoFromPayload(payload any) (TalkSessionInfo, error) {
	mapped := toMap(payload)
	sessionID, _ := mapped["sessionId"].(string)
	if sessionID == "" {
		return TalkSessionInfo{}, fmt.Errorf("talk.session.create payload missing sessionId: %s", StableString(payload))
	}
	relaySessionID, _ := mapped["relaySessionId"].(string)
	audio := toMap(mapped["audio"])
	encoding, _ := audio["inputEncoding"].(string)
	if encoding == "" {
		return TalkSessionInfo{}, fmt.Errorf("talk.session.create audio missing inputEncoding: %s", StableString(audio))
	}
	rate, ok := numericInt(audio["inputSampleRateHz"])
	if !ok || rate <= 0 {
		return TalkSessionInfo{}, fmt.Errorf("talk.session.create audio missing inputSampleRateHz: %s", StableString(audio))
	}
	return TalkSessionInfo{
		SessionID: sessionID, RelaySessionID: relaySessionID,
		InputEncoding: encoding, InputSampleRateHz: rate,
	}, nil
}

func TalkEventFromFrame(frame any) (TalkEvent, bool) {
	mapped := toMap(frame)
	if mapped["type"] != "event" || mapped["event"] != "talk.event" {
		return TalkEvent{}, false
	}
	wrapper := toMap(mapped["payload"])
	raw := toMap(wrapper["talkEvent"])
	eventType, _ := raw["type"].(string)
	sessionID, _ := raw["sessionId"].(string)
	payload := toMap(raw["payload"])
	if eventType == "" || sessionID == "" || payload == nil {
		return TalkEvent{}, false
	}
	callID, _ := raw["callId"].(string)
	itemID, _ := raw["itemId"].(string)
	return TalkEvent{
		EventType: eventType, SessionID: sessionID, Payload: payload,
		Wrapper: wrapper, Raw: raw, Final: truthy(raw["final"]),
		CallID: callID, ItemID: itemID,
	}, true
}

func ToolCallEventFromTalk(event TalkEvent) (ToolCallEvent, bool) {
	if event.EventType != "tool.call" || event.CallID == "" {
		return ToolCallEvent{}, false
	}
	name, _ := event.Payload["name"].(string)
	args := toMap(event.Payload["args"])
	if name == "" || args == nil {
		return ToolCallEvent{}, false
	}
	relaySessionID, _ := event.Wrapper["relaySessionId"].(string)
	return ToolCallEvent{
		SessionID: event.SessionID, RelaySessionID: relaySessionID,
		CallID: event.CallID, Name: name, Args: args,
	}, true
}

func ChatEventFromFrame(frame any) (ChatEvent, bool) {
	mapped := toMap(frame)
	if mapped["type"] != "event" || mapped["event"] != "chat" {
		return ChatEvent{}, false
	}
	payload := toMap(mapped["payload"])
	runID, _ := payload["runId"].(string)
	state, _ := payload["state"].(string)
	if runID == "" || state == "" {
		return ChatEvent{}, false
	}
	deltaText, _ := payload["deltaText"].(string)
	errorMessage, _ := payload["errorMessage"].(string)
	errorKind, _ := payload["errorKind"].(string)
	stopReason, _ := payload["stopReason"].(string)
	stream, _ := payload["stream"].(string)
	return ChatEvent{
		RunID: runID, State: state, DeltaText: deltaText,
		Replace: truthy(payload["replace"]), MessageText: textFromChatMessage(payload["message"]),
		ErrorMessage: errorMessage, ErrorKind: errorKind, StopReason: stopReason,
		Stream: stream, Data: toMap(payload["data"]),
	}, true
}

func textFromChatMessage(message any) string {
	mapped := toMap(message)
	rawContent, _ := mapped["content"].([]any)
	var out string
	for _, item := range rawContent {
		part := toMap(item)
		if part["type"] != "text" {
			continue
		}
		text, _ := part["text"].(string)
		out += text
	}
	return out
}

func truthy(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func numericInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	case jsonNumber:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

type jsonNumber interface {
	Int64() (int64, error)
}
