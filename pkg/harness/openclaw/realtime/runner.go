package realtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	DefaultSessionKey               = "agent:main:aries-realtime"
	DefaultChunkDuration            = 50 * time.Millisecond
	DefaultTrailingListenDuration   = 180 * time.Second
	DefaultQuietDuration            = 8 * time.Second
	DefaultAgentWaitDuration        = 60 * time.Second
	DefaultAgentWaitFallback        = 15 * time.Second
	DefaultToolCallTimeout          = 60 * time.Second
	DefaultSubmitToolResultTimeout  = 30 * time.Second
	DefaultAppendAudioTimeout       = 10 * time.Second
	DefaultVADThreshold             = 0.3
	DefaultSilenceDurationMillis    = 1500
	DefaultPrefixPaddingMillis      = 500
	DefaultRealtimeInputEncoding    = "pcm16"
	DefaultRealtimeInputSampleRate  = 24000
	defaultRealtimeOutputBufferSize = 32 << 10
)

const (
	methodSessionCreate    = "talk.session.create"
	methodAppendAudio      = "talk.session.appendAudio"
	methodClientToolCall   = "talk.client.toolCall"
	methodSubmitToolResult = "talk.session.submitToolResult"
	methodAgentWait        = "agent.wait"
	sessionModeRealtime    = "realtime"
	sessionTransportRelay  = "gateway-relay"
	sessionBrainAgent      = "agent-consult"
	toolAgentConsult       = "openclaw_agent_consult"
	toolAgentControl       = "openclaw_agent_control"
	eventOutputAudioDone   = "output.audio.done"
	eventTranscriptDelta   = "transcript.delta"
	eventTranscriptDone    = "transcript.done"
	eventToolCall          = "tool.call"
	eventToolResult        = "tool.result"
	eventToolProgress      = "tool.progress"
	eventTurnEnded         = "turn.ended"
	eventSessionClosed     = "session.closed"
	eventOutputTextDelta   = "output.text.delta"
	eventOutputTextDone    = "output.text.done"
	eventSessionError      = "session.error"
	eventToolError         = "tool.error"
	eventTurnCancelled     = "turn.cancelled"
	chatEventPrefix        = "chat"
	chatToolEventPrefix    = "chat.tool."
	chatStateFinal         = "final"
	chatStateError         = "error"
	chatStateAborted       = "aborted"
	chatStreamTool         = "tool"
	toolArgQuestion        = "question"
	toolArgContext         = "context"
	toolArgResponseStyle   = "responseStyle"
	toolResultErrorKey     = "error"
	defaultControlMode     = "status"
	agentStatusIdle        = "idle"
	agentStatusWorking     = "working"
	roleUser               = "user"
)

type RealtimeGateway interface {
	Connect(context.Context, ConnectOptions) (ConnectSummary, error)
	Call(context.Context, string, map[string]any) (Frame, error)
	RecvEvent(context.Context) (Frame, error)
	Close() error
}

type RealtimeToolHandler interface {
	HandleRealtimeToolCall(context.Context, RealtimeToolCallRequest) (RealtimeToolCallResult, error)
}

type RealtimeToolCallRequest struct {
	Gateway               RealtimeGateway
	ToolCall              ToolCallEvent
	SessionKey            string
	AgentQuestionTemplate string
	ToolCallTimeout       time.Duration
}

type RealtimeToolCallResult struct {
	ToolResult           any
	AgentRunID           string
	ProviderToolQuestion string
	AgentQuestionUsed    string
	AgentConsultOK       bool
}

// AgentConsultBridgeToolHandler asks OpenClaw to run its agent-consult tool.
// That nested agent uses the harness configuration, including the ARIES SSH
// bridge, so sandbox tool execution and audit logs stay on the existing bridge
// path.
type AgentConsultBridgeToolHandler struct{}

type RealtimeAudio struct {
	Data           []byte
	Rate           int
	BytesPerSample int
	Encoding       string
}

type RealtimeAudioProvider func(TalkSessionInfo) (RealtimeAudio, error)

type RealtimeRunnerOptions struct {
	OriginalPrompt            string
	SessionKey                string
	Provider                  string
	Model                     string
	Voice                     string
	ReasoningEffort           string
	Audio                     RealtimeAudio
	AudioProvider             RealtimeAudioProvider
	ChunkDuration             time.Duration
	ListenDuration            time.Duration
	QuietDuration             time.Duration
	AgentWaitDuration         time.Duration
	AgentWaitFallbackDuration time.Duration
	ToolCallTimeout           time.Duration
	SubmitToolResultTimeout   time.Duration
	AppendAudioTimeout        time.Duration
	VADThreshold              *float64
	SilenceDurationMillis     *int
	PrefixPaddingMillis       *int
	AgentQuestionTemplate     string
	ToolHandler               RealtimeToolHandler
	ConnectOptions            ConnectOptions
	IncludeEvents             bool
	CloseGateway              bool
}

type RealtimeRunner struct {
	gateway RealtimeGateway
	options RealtimeRunnerOptions
	sleep   func(context.Context, time.Duration) error
}

func NewRealtimeRunner(gateway RealtimeGateway, options RealtimeRunnerOptions) (*RealtimeRunner, error) {
	if gateway == nil {
		return nil, errors.New("realtime gateway is required")
	}
	if len(options.Audio.Data) == 0 && options.AudioProvider == nil {
		return nil, errors.New("realtime audio is required")
	}
	if len(options.Audio.Data) != 0 && options.Audio.Rate <= 0 {
		return nil, errors.New("realtime audio sample rate must be positive")
	}
	if len(options.Audio.Data) != 0 && options.Audio.BytesPerSample <= 0 {
		return nil, errors.New("realtime audio bytes per sample must be positive")
	}
	return &RealtimeRunner{
		gateway: gateway,
		options: options,
		sleep:   sleepContext,
	}, nil
}

func (runner *RealtimeRunner) Run(ctx context.Context) (RealtimeResult, error) {
	result := NewRealtimeResult()
	result.OriginalPrompt = runner.options.OriginalPrompt
	if runner.options.CloseGateway {
		defer runner.gateway.Close()
	}
	connectSummary, err := runner.gateway.Connect(ctx, runner.options.ConnectOptions)
	if err != nil {
		result.AppendError(err.Error())
		return result.WithoutEvents(), err
	}
	if !connectSummary.HasScope("operator.read") || !connectSummary.HasScope("operator.write") {
		err := errors.New("realtime gateway requires operator.read and operator.write scopes")
		result.AppendError(err.Error())
		return result.WithoutEvents(), err
	}
	result.ConnectAuth = map[string]any{"role": connectSummary.Role, "scopes": append([]string(nil), connectSummary.Scopes...)}
	session, err := runner.createSession(ctx)
	if err != nil {
		result.AppendError(err.Error())
		return result.WithoutEvents(), err
	}
	result.SessionID = stringPtr(session.SessionID)
	if session.RelaySessionID != "" {
		result.RelaySessionID = stringPtr(session.RelaySessionID)
	}
	if err := runner.loadAudioForSession(session); err != nil {
		result.AppendError(err.Error())
		return result.WithoutEvents(), err
	}
	if err := runner.appendAudio(ctx, session); err != nil {
		result.AppendError(err.Error())
		return result.WithoutEvents(), err
	}
	if err := runner.processEvents(ctx, &result); err != nil {
		result.AppendError(err.Error())
		return scrubRealtimeEvents(result, runner.options.IncludeEvents), err
	}
	return scrubRealtimeEvents(result, runner.options.IncludeEvents), nil
}

func (runner *RealtimeRunner) createSession(ctx context.Context) (TalkSessionInfo, error) {
	response, err := runner.gateway.Call(ctx, methodSessionCreate, runner.sessionParams())
	if err != nil {
		return TalkSessionInfo{}, err
	}
	if !response.Bool("ok") {
		return TalkSessionInfo{}, fmt.Errorf("%s failed: %s", methodSessionCreate, StableString(response))
	}
	return TalkSessionInfoFromPayload(response.Map("payload"))
}

func (runner *RealtimeRunner) sessionParams() map[string]any {
	options := runner.options
	params := map[string]any{
		"sessionKey":        valueOrDefault(options.SessionKey, DefaultSessionKey),
		"mode":              sessionModeRealtime,
		"transport":         sessionTransportRelay,
		"brain":             sessionBrainAgent,
		"vadThreshold":      pointerOrDefault(options.VADThreshold, DefaultVADThreshold),
		"silenceDurationMs": intPointerOrDefault(options.SilenceDurationMillis, DefaultSilenceDurationMillis),
		"prefixPaddingMs":   intPointerOrDefault(options.PrefixPaddingMillis, DefaultPrefixPaddingMillis),
	}
	for _, item := range []struct {
		value string
		key   string
	}{
		{options.Provider, "provider"},
		{options.Model, "model"},
		{options.Voice, "voice"},
		{options.ReasoningEffort, "reasoningEffort"},
	} {
		if strings.TrimSpace(item.value) != "" {
			params[item.key] = item.value
		}
	}
	return params
}

func (runner *RealtimeRunner) loadAudioForSession(session TalkSessionInfo) error {
	if runner.options.AudioProvider == nil {
		return nil
	}
	audio, err := runner.options.AudioProvider(session)
	if err != nil {
		return err
	}
	if !validRealtimeAudio(audio) {
		return errors.New("realtime audio provider returned invalid audio")
	}
	runner.options.Audio = audio
	return nil
}

func validRealtimeAudio(audio RealtimeAudio) bool {
	return len(audio.Data) != 0 && audio.Rate > 0 && audio.BytesPerSample > 0
}

func (runner *RealtimeRunner) appendAudio(ctx context.Context, session TalkSessionInfo) error {
	audio := runner.options.Audio
	chunkDuration := durationOrDefault(runner.options.ChunkDuration, DefaultChunkDuration)
	chunkSize := realtimeAudioChunkSize(audio, chunkDuration)
	for offset := 0; offset < len(audio.Data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(audio.Data) {
			end = len(audio.Data)
		}
		timestamp := realtimeAudioTimestampMillis(audio, offset)
		callCtx, cancel := context.WithTimeout(ctx, durationOrDefault(runner.options.AppendAudioTimeout, DefaultAppendAudioTimeout))
		response, err := runner.gateway.Call(callCtx, methodAppendAudio, map[string]any{
			"sessionId":   session.SessionID,
			"audioBase64": base64.StdEncoding.EncodeToString(audio.Data[offset:end]),
			"timestamp":   timestamp,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("%s chunk offset %d: %w", methodAppendAudio, offset, err)
		}
		if !response.Bool("ok") {
			return fmt.Errorf("%s failed: %s", methodAppendAudio, StableString(response))
		}
		if end < len(audio.Data) {
			if err := runner.sleep(ctx, chunkDuration); err != nil {
				return err
			}
		}
	}
	return nil
}

func realtimeAudioChunkSize(audio RealtimeAudio, duration time.Duration) int {
	chunkSize := int(int64(audio.Rate) * int64(audio.BytesPerSample) * int64(duration) / int64(time.Second))
	if chunkSize < audio.BytesPerSample {
		chunkSize = audio.BytesPerSample
	}
	chunkSize -= chunkSize % audio.BytesPerSample
	if chunkSize <= 0 {
		chunkSize = audio.BytesPerSample
	}
	return chunkSize
}

func realtimeAudioTimestampMillis(audio RealtimeAudio, offset int) int {
	return offset * 1000 / max(1, audio.Rate*audio.BytesPerSample)
}

func (runner *RealtimeRunner) processEvents(ctx context.Context, result *RealtimeResult) error {
	state := realtimeEventState{
		activeAgentRuns:    map[string]struct{}{},
		completedAgentRuns: map[string]struct{}{},
		failedAgentRuns:    map[string]string{},
		deadline:           time.Now().Add(durationOrDefault(runner.options.ListenDuration, DefaultTrailingListenDuration)),
	}
	for time.Now().Before(state.deadline) {
		waitUntil := state.deadline
		if !state.hasActiveRuns() && !state.quietDeadline.IsZero() && state.quietDeadline.Before(waitUntil) {
			waitUntil = state.quietDeadline
		}
		recvCtx, cancel := context.WithDeadline(ctx, waitUntil)
		frame, err := runner.gateway.RecvEvent(recvCtx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				if !state.quietDeadline.IsZero() && !state.hasActiveRuns() && !time.Now().Before(state.quietDeadline) {
					break
				}
				continue
			}
			return err
		}
		if runner.options.IncludeEvents {
			result.Events = append(result.Events, cloneFrame(frame))
		}
		if err := runner.processFrame(ctx, frame, result, &state); err != nil {
			return err
		}
	}
	if state.latestUserTranscript == "" {
		result.Transcript = state.partialTranscript
	} else {
		result.Transcript = state.latestUserTranscript
	}
	result.OutputText = state.output.String()
	if state.hasActiveRuns() {
		runner.finishActiveAgentRuns(ctx, result, &state)
	}
	state.appendUnrecoveredFailures(result)
	return nil
}

func (runner *RealtimeRunner) processFrame(ctx context.Context, frame Frame, result *RealtimeResult, state *realtimeEventState) error {
	if chat, ok := ChatEventFromFrame(frame); ok {
		runner.processChatEvent(chat, result, state)
		return nil
	}
	talk, ok := TalkEventFromFrame(frame)
	if !ok {
		return nil
	}
	eventType, _ := talk.Wrapper["type"].(string)
	if eventType == "" {
		eventType = talk.EventType
	}
	result.IncrementEvent(eventType)
	result.IncrementEvent(talk.EventType)

	switch talk.EventType {
	case eventOutputAudioDone:
		result.OutputAudioDone = true
	case eventTranscriptDelta:
		if text := TextFromPayload(talk.Payload); text != "" {
			state.partialTranscript = text
			state.touchQuiet(runner.options.QuietDuration)
		}
	case eventTranscriptDone:
		if text := TextFromPayload(talk.Payload); text != "" {
			if role, _ := talk.Payload["role"].(string); role == roleUser {
				state.latestUserTranscript = text
				result.TranscriptDone = text
				result.TranscriptDoneParts = append(result.TranscriptDoneParts, text)
				result.Transcript = text
				state.touchQuiet(runner.options.QuietDuration)
			}
		}
	case eventToolCall:
		return runner.processToolCall(ctx, talk, result, state)
	case eventToolResult, eventToolProgress, eventTurnEnded, eventSessionClosed:
		state.touchQuiet(runner.options.QuietDuration)
	case eventOutputTextDelta, eventOutputTextDone:
		if text := TextFromPayload(talk.Payload); text != "" {
			state.output.WriteStringBounded(text, defaultRealtimeOutputBufferSize)
			state.touchQuiet(runner.options.QuietDuration)
		}
	case eventSessionError, eventToolError, eventTurnCancelled:
		result.AppendError(StableString(talk.Payload))
		state.touchQuiet(runner.options.QuietDuration)
	}
	return nil
}

func (runner *RealtimeRunner) processToolCall(ctx context.Context, talk TalkEvent, result *RealtimeResult, state *realtimeEventState) error {
	toolCall, ok := ToolCallEventFromTalk(talk)
	if !ok {
		return nil
	}
	if toolCall.Name == toolAgentControl {
		if err := runner.handleAgentControl(ctx, toolCall, result, state); err != nil {
			return err
		}
		state.touchQuiet(runner.options.QuietDuration)
		return nil
	}
	runID, err := runner.handleToolCall(ctx, toolCall, result)
	if err != nil {
		return err
	}
	if runID != "" {
		state.activeAgentRuns[runID] = struct{}{}
		state.extend(runner.options.AgentWaitDuration)
	}
	state.touchQuiet(runner.options.QuietDuration)
	return nil
}

func (runner *RealtimeRunner) handleAgentControl(ctx context.Context, toolCall ToolCallEvent, result *RealtimeResult, state *realtimeEventState) error {
	result.ToolCalls++
	mode, _ := toolCall.Args["mode"].(string)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = defaultControlMode
	}
	runIDs := sortedKeys(state.activeAgentRuns)
	status := agentStatusIdle
	message := "No agent run is currently active."
	if len(runIDs) != 0 {
		status = agentStatusWorking
		message = "The agent is still working on the request."
	}
	toolResult := map[string]any{
		"ok":           true,
		"mode":         mode,
		"status":       status,
		"message":      message,
		"activeRunIds": runIDs,
	}
	if mode != defaultControlMode {
		toolResult["message"] = fmt.Sprintf("Control mode %q is not supported by this runner; status was returned instead.", mode)
	}
	if err := runner.submitToolResult(ctx, toolCall, toolResult); err != nil {
		return err
	}
	result.ToolResults++
	return nil
}

func (runner *RealtimeRunner) processChatEvent(event ChatEvent, result *RealtimeResult, state *realtimeEventState) {
	result.IncrementEvent(chatEventPrefix)
	result.IncrementEvent(chatEventPrefix + "." + event.State)
	if _, ok := state.activeAgentRuns[event.RunID]; !ok && !containsString(result.AgentRunIDs, event.RunID) {
		return
	}
	if event.DeltaText != "" {
		if event.Replace {
			state.output.Reset()
		}
		state.output.WriteStringBounded(event.DeltaText, defaultRealtimeOutputBufferSize)
		state.touchQuiet(runner.options.QuietDuration)
	}
	switch event.State {
	case chatStateFinal:
		if event.MessageText != "" && state.output.Len() == 0 {
			state.output.WriteStringBounded(event.MessageText, defaultRealtimeOutputBufferSize)
		}
		delete(state.activeAgentRuns, event.RunID)
		state.completedAgentRuns[event.RunID] = struct{}{}
		delete(state.failedAgentRuns, event.RunID)
		state.touchQuiet(runner.options.QuietDuration)
	case chatStateError, chatStateAborted:
		if _, completed := state.completedAgentRuns[event.RunID]; completed {
			state.touchQuiet(runner.options.QuietDuration)
			return
		}
		delete(state.activeAgentRuns, event.RunID)
		detail := firstNonEmpty(event.ErrorMessage, event.StopReason, event.State)
		if event.ErrorKind != "" {
			detail += " (" + event.ErrorKind + ")"
		}
		state.failedAgentRuns[event.RunID] = fmt.Sprintf("agent run %s ended with %s: %s", event.RunID, event.State, detail)
		state.touchQuiet(runner.options.QuietDuration)
	default:
		state.extend(runner.options.AgentWaitDuration)
		if event.Stream == chatStreamTool {
			name, _ := event.Data["name"].(string)
			phase, _ := event.Data["phase"].(string)
			if name != "" && phase != "" {
				result.IncrementEvent(chatToolEventPrefix + phase)
			}
		}
		state.touchQuiet(runner.options.QuietDuration)
	}
}

func (runner *RealtimeRunner) handleToolCall(ctx context.Context, toolCall ToolCallEvent, result *RealtimeResult) (string, error) {
	result.ToolCalls++
	handler := runner.options.ToolHandler
	if handler == nil {
		handler = AgentConsultBridgeToolHandler{}
	}
	outcome, err := handler.HandleRealtimeToolCall(ctx, RealtimeToolCallRequest{
		Gateway: runner.gateway, ToolCall: toolCall,
		SessionKey:            valueOrDefault(runner.options.SessionKey, DefaultSessionKey),
		AgentQuestionTemplate: runner.options.AgentQuestionTemplate,
		ToolCallTimeout:       durationOrDefault(runner.options.ToolCallTimeout, DefaultToolCallTimeout),
	})
	if outcome.ProviderToolQuestion != "" {
		result.ProviderToolQuestion = outcome.ProviderToolQuestion
	}
	if outcome.AgentQuestionUsed != "" {
		result.AgentQuestionUsed = outcome.AgentQuestionUsed
	}
	toolResult := outcome.ToolResult
	if err != nil {
		toolResult = map[string]any{toolResultErrorKey: err.Error()}
		result.AppendError(err.Error())
	}
	if toolResult == nil {
		toolResult = map[string]any{toolResultErrorKey: "realtime tool handler returned no result"}
		result.AppendError("realtime tool handler returned no result")
	}
	if outcome.AgentRunID != "" {
		result.AgentRunIDs = append(result.AgentRunIDs, outcome.AgentRunID)
	}
	if outcome.AgentConsultOK {
		result.AgentConsultOK = true
	}
	if err := runner.submitToolResult(ctx, toolCall, toolResult); err != nil {
		return outcome.AgentRunID, err
	}
	result.ToolResults++
	return outcome.AgentRunID, nil
}

func (AgentConsultBridgeToolHandler) HandleRealtimeToolCall(ctx context.Context, request RealtimeToolCallRequest) (RealtimeToolCallResult, error) {
	if request.Gateway == nil {
		return RealtimeToolCallResult{}, errors.New("realtime gateway is required")
	}
	providerQuestion, _ := request.ToolCall.Args[toolArgQuestion].(string)
	agentQuestion := providerQuestion
	if request.AgentQuestionTemplate != "" {
		agentQuestion = strings.ReplaceAll(request.AgentQuestionTemplate, "{question}", providerQuestion)
	}
	outcome := RealtimeToolCallResult{
		ProviderToolQuestion: providerQuestion,
		AgentQuestionUsed:    agentQuestion,
	}
	if request.ToolCall.Name != toolAgentConsult {
		message := fmt.Sprintf("runner does not handle tool %q", request.ToolCall.Name)
		outcome.ToolResult = map[string]any{toolResultErrorKey: message}
		return outcome, errors.New(message)
	}
	args := map[string]any{toolArgQuestion: agentQuestion}
	for _, key := range []string{toolArgContext, toolArgResponseStyle} {
		if value, _ := request.ToolCall.Args[key].(string); value != "" {
			args[key] = value
		}
	}
	params := map[string]any{
		"sessionKey": request.SessionKey,
		"callId":     request.ToolCall.CallID,
		"name":       request.ToolCall.Name,
		"args":       args,
	}
	if request.ToolCall.RelaySessionID != "" {
		params["relaySessionId"] = request.ToolCall.RelaySessionID
	}
	callCtx, cancel := context.WithTimeout(ctx, durationOrDefault(request.ToolCallTimeout, DefaultToolCallTimeout))
	response, err := request.Gateway.Call(callCtx, methodClientToolCall, params)
	cancel()
	if err != nil {
		return outcome, err
	}
	if !response.Bool("ok") {
		return outcome, fmt.Errorf("%s failed: %s", methodClientToolCall, StableString(response))
	}
	payload := response.Map("payload")
	runID, _ := payload["runId"].(string)
	if runID == "" {
		return outcome, fmt.Errorf("%s response missing payload.runId", methodClientToolCall)
	}
	outcome.ToolResult = payload
	outcome.AgentRunID = runID
	outcome.AgentConsultOK = true
	return outcome, nil
}

func (runner *RealtimeRunner) submitToolResult(ctx context.Context, toolCall ToolCallEvent, toolResult any) error {
	sessionID := firstNonEmpty(toolCall.RelaySessionID, toolCall.SessionID)
	callCtx, cancel := context.WithTimeout(ctx, durationOrDefault(runner.options.SubmitToolResultTimeout, DefaultSubmitToolResultTimeout))
	response, err := runner.gateway.Call(callCtx, methodSubmitToolResult, map[string]any{
		"sessionId": sessionID,
		"callId":    toolCall.CallID,
		"result":    toolResult,
		"options":   map[string]any{"suppressResponse": false, "willContinue": false},
	})
	cancel()
	if err != nil {
		return err
	}
	if !response.Bool("ok") {
		return fmt.Errorf("%s failed: %s", methodSubmitToolResult, StableString(response))
	}
	return nil
}

func (runner *RealtimeRunner) finishActiveAgentRuns(ctx context.Context, result *RealtimeResult, state *realtimeEventState) {
	for runID := range state.activeAgentRuns {
		callCtx, cancel := context.WithTimeout(ctx, durationOrDefault(runner.options.AgentWaitFallbackDuration, DefaultAgentWaitFallback))
		response, err := runner.gateway.Call(callCtx, methodAgentWait, map[string]any{
			"runId":     runID,
			"timeoutMs": max(1, int(durationOrDefault(runner.options.AgentWaitFallbackDuration, DefaultAgentWaitFallback)/time.Millisecond)),
		})
		cancel()
		if err != nil {
			result.AppendError(fmt.Sprintf("timeout waiting for agent run %s; %s failed: %v", runID, methodAgentWait, err))
			continue
		}
		payload := response.Map("payload")
		status, _ := payload["status"].(string)
		if strings.EqualFold(status, "ok") {
			continue
		}
		detail := firstNonEmpty(stringFromAny(payload["error"]), stringFromAny(payload["stopReason"]), status)
		if detail == "" {
			detail = StableString(response)
		}
		result.AppendError(fmt.Sprintf("agent run %s did not finish cleanly: %s", runID, detail))
	}
}

type realtimeEventState struct {
	deadline             time.Time
	quietDeadline        time.Time
	activeAgentRuns      map[string]struct{}
	completedAgentRuns   map[string]struct{}
	failedAgentRuns      map[string]string
	partialTranscript    string
	latestUserTranscript string
	output               boundedString
}

func (state *realtimeEventState) hasActiveRuns() bool {
	return len(state.activeAgentRuns) != 0
}

func (state *realtimeEventState) touchQuiet(duration time.Duration) {
	state.quietDeadline = time.Now().Add(durationOrDefault(duration, DefaultQuietDuration))
}

func (state *realtimeEventState) extend(duration time.Duration) {
	deadline := time.Now().Add(durationOrDefault(duration, DefaultAgentWaitDuration))
	if deadline.After(state.deadline) {
		state.deadline = deadline
	}
}

func (state *realtimeEventState) appendUnrecoveredFailures(result *RealtimeResult) {
	for _, runID := range sortedStringKeys(state.failedAgentRuns) {
		result.AppendError(state.failedAgentRuns[runID])
	}
}

type boundedString struct {
	builder strings.Builder
}

func (value *boundedString) WriteStringBounded(text string, limit int) {
	if limit <= 0 || text == "" || value.builder.Len() >= limit {
		return
	}
	remaining := limit - value.builder.Len()
	if len(text) > remaining {
		text = text[:remaining]
	}
	value.builder.WriteString(text)
}

func (value *boundedString) String() string {
	return value.builder.String()
}

func (value *boundedString) Len() int {
	return value.builder.Len()
}

func (value *boundedString) Reset() {
	value.builder.Reset()
}

func scrubRealtimeEvents(result RealtimeResult, include bool) RealtimeResult {
	if include {
		return result
	}
	return result.WithoutEvents()
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func pointerOrDefault(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func intPointerOrDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func stringFromAny(value any) string {
	text, _ := value.(string)
	return text
}

func stringPtr(value string) *string {
	return &value
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneFrame(frame Frame) Frame {
	out := make(Frame, len(frame))
	for key, value := range frame {
		out[key] = value
	}
	return out
}
