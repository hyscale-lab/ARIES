package realtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
)

const (
	defaultSessionKey               = "agent:main:aries-realtime"
	defaultChunkDuration            = 50 * time.Millisecond
	defaultTrailingListenDuration   = 180 * time.Second
	defaultQuietDuration            = 8 * time.Second
	defaultAgentWaitDuration        = 60 * time.Second
	defaultAgentWaitFallback        = 15 * time.Second
	defaultToolCallTimeout          = 60 * time.Second
	defaultSubmitToolResultTimeout  = 30 * time.Second
	defaultAppendAudioTimeout       = 10 * time.Second
	defaultVADThreshold             = 0.3
	defaultSilenceDurationMillis    = 1500
	defaultPrefixPaddingMillis      = 500
	DefaultInputEncoding            = "pcm16"
	DefaultInputSampleRate          = 24000
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

type Gateway interface {
	Connect(context.Context, gateway.ConnectOptions) (gateway.ConnectSummary, error)
	Call(context.Context, string, map[string]any) (gateway.Frame, error)
	RecvEvent(context.Context) (gateway.Frame, error)
	Close() error
}

type toolCallOutcome struct {
	ToolResult           any
	AgentRunID           string
	ProviderToolQuestion string
	AgentQuestionUsed    string
	AgentConsultOK       bool
}

type Audio struct {
	Data           []byte
	Rate           int
	BytesPerSample int
	Encoding       string
}

type AudioProvider func(SessionInfo) (Audio, error)

type Options struct {
	OriginalPrompt            string
	SessionKey                string
	Provider                  string
	Model                     string
	Voice                     string
	ReasoningEffort           string
	Audio                     Audio
	AudioProvider             AudioProvider
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
	ConnectOptions            gateway.ConnectOptions
	IncludeEvents             bool
	CloseGateway              bool
}

type Runner struct {
	gateway Gateway
	options Options
	sleep   func(context.Context, time.Duration) error
}

func New(gateway Gateway, options Options) (*Runner, error) {
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
	return &Runner{
		gateway: gateway,
		options: options,
		sleep:   sleepContext,
	}, nil
}

func (runner *Runner) Run(ctx context.Context) (Result, error) {
	result := newResult()
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

func (runner *Runner) createSession(ctx context.Context) (SessionInfo, error) {
	response, err := runner.gateway.Call(ctx, methodSessionCreate, runner.sessionParams())
	if err != nil {
		return SessionInfo{}, err
	}
	if !response.Bool("ok") {
		return SessionInfo{}, fmt.Errorf("%s failed: %s", methodSessionCreate, gateway.StableString(response))
	}
	return sessionInfoFromPayload(response.Map("payload"))
}

func (runner *Runner) sessionParams() map[string]any {
	options := runner.options
	params := map[string]any{
		"sessionKey":        valueOrDefault(options.SessionKey, defaultSessionKey),
		"mode":              sessionModeRealtime,
		"transport":         sessionTransportRelay,
		"brain":             sessionBrainAgent,
		"vadThreshold":      pointerOrDefault(options.VADThreshold, defaultVADThreshold),
		"silenceDurationMs": intPointerOrDefault(options.SilenceDurationMillis, defaultSilenceDurationMillis),
		"prefixPaddingMs":   intPointerOrDefault(options.PrefixPaddingMillis, defaultPrefixPaddingMillis),
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

func (runner *Runner) loadAudioForSession(session SessionInfo) error {
	if runner.options.AudioProvider == nil {
		return nil
	}
	audio, err := runner.options.AudioProvider(session)
	if err != nil {
		return err
	}
	if !validAudio(audio) {
		return errors.New("realtime audio provider returned invalid audio")
	}
	runner.options.Audio = audio
	return nil
}

func validAudio(audio Audio) bool {
	return len(audio.Data) != 0 && audio.Rate > 0 && audio.BytesPerSample > 0
}

func (runner *Runner) appendAudio(ctx context.Context, session SessionInfo) error {
	audio := runner.options.Audio
	chunkDuration := durationOrDefault(runner.options.ChunkDuration, defaultChunkDuration)
	chunkSize := realtimeAudioChunkSize(audio, chunkDuration)
	for offset := 0; offset < len(audio.Data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(audio.Data) {
			end = len(audio.Data)
		}
		timestamp := realtimeAudioTimestampMillis(audio, offset)
		callCtx, cancel := context.WithTimeout(ctx, durationOrDefault(runner.options.AppendAudioTimeout, defaultAppendAudioTimeout))
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
			return fmt.Errorf("%s failed: %s", methodAppendAudio, gateway.StableString(response))
		}
		if end < len(audio.Data) {
			if err := runner.sleep(ctx, chunkDuration); err != nil {
				return err
			}
		}
	}
	return nil
}

func realtimeAudioChunkSize(audio Audio, duration time.Duration) int {
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

func realtimeAudioTimestampMillis(audio Audio, offset int) int {
	return offset * 1000 / max(1, audio.Rate*audio.BytesPerSample)
}

func (runner *Runner) processEvents(ctx context.Context, result *Result) error {
	state := realtimeEventState{
		activeAgentRuns:    map[string]struct{}{},
		completedAgentRuns: map[string]struct{}{},
		failedAgentRuns:    map[string]string{},
		deadline:           time.Now().Add(durationOrDefault(runner.options.ListenDuration, defaultTrailingListenDuration)),
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

func (runner *Runner) processFrame(ctx context.Context, frame gateway.Frame, result *Result, state *realtimeEventState) error {
	if chat, ok := chatEventFromFrame(frame); ok {
		runner.processChatEvent(chat, result, state)
		return nil
	}
	talk, ok := talkEventFromFrame(frame)
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
		if text := textFromPayload(talk.Payload); text != "" {
			state.partialTranscript = text
			state.touchQuiet(runner.options.QuietDuration)
		}
	case eventTranscriptDone:
		if text := textFromPayload(talk.Payload); text != "" {
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
		if text := textFromPayload(talk.Payload); text != "" {
			state.output.WriteStringBounded(text, defaultRealtimeOutputBufferSize)
			state.touchQuiet(runner.options.QuietDuration)
		}
	case eventSessionError, eventToolError, eventTurnCancelled:
		result.AppendError(gateway.StableString(talk.Payload))
		state.touchQuiet(runner.options.QuietDuration)
	}
	return nil
}

func (runner *Runner) processToolCall(ctx context.Context, talk talkEvent, result *Result, state *realtimeEventState) error {
	toolCall, ok := toolCallEventFromTalk(talk)
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

func (runner *Runner) handleAgentControl(ctx context.Context, toolCall toolCallEvent, result *Result, state *realtimeEventState) error {
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

func (runner *Runner) processChatEvent(event chatEvent, result *Result, state *realtimeEventState) {
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

func (runner *Runner) handleToolCall(ctx context.Context, toolCall toolCallEvent, result *Result) (string, error) {
	result.ToolCalls++
	outcome, err := runner.consultAgent(ctx, toolCall)
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

// consultAgent asks OpenClaw to run its agent-consult tool. That nested agent
// uses the harness configuration, including the ARIES SSH bridge, so sandbox
// tool execution and audit logs stay on the existing bridge path.
func (runner *Runner) consultAgent(ctx context.Context, toolCall toolCallEvent) (toolCallOutcome, error) {
	providerQuestion, _ := toolCall.Args[toolArgQuestion].(string)
	agentQuestion := providerQuestion
	if runner.options.AgentQuestionTemplate != "" {
		agentQuestion = strings.ReplaceAll(runner.options.AgentQuestionTemplate, "{question}", providerQuestion)
	}
	outcome := toolCallOutcome{
		ProviderToolQuestion: providerQuestion,
		AgentQuestionUsed:    agentQuestion,
	}
	if toolCall.Name != toolAgentConsult {
		message := fmt.Sprintf("runner does not handle tool %q", toolCall.Name)
		outcome.ToolResult = map[string]any{toolResultErrorKey: message}
		return outcome, errors.New(message)
	}
	args := map[string]any{toolArgQuestion: agentQuestion}
	for _, key := range []string{toolArgContext, toolArgResponseStyle} {
		if value, _ := toolCall.Args[key].(string); value != "" {
			args[key] = value
		}
	}
	params := map[string]any{
		"sessionKey": valueOrDefault(runner.options.SessionKey, defaultSessionKey),
		"callId":     toolCall.CallID,
		"name":       toolCall.Name,
		"args":       args,
	}
	if toolCall.RelaySessionID != "" {
		params["relaySessionId"] = toolCall.RelaySessionID
	}
	callCtx, cancel := context.WithTimeout(ctx, durationOrDefault(runner.options.ToolCallTimeout, defaultToolCallTimeout))
	response, err := runner.gateway.Call(callCtx, methodClientToolCall, params)
	cancel()
	if err != nil {
		return outcome, err
	}
	if !response.Bool("ok") {
		return outcome, fmt.Errorf("%s failed: %s", methodClientToolCall, gateway.StableString(response))
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

func (runner *Runner) submitToolResult(ctx context.Context, toolCall toolCallEvent, toolResult any) error {
	sessionID := firstNonEmpty(toolCall.RelaySessionID, toolCall.SessionID)
	callCtx, cancel := context.WithTimeout(ctx, durationOrDefault(runner.options.SubmitToolResultTimeout, defaultSubmitToolResultTimeout))
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
		return fmt.Errorf("%s failed: %s", methodSubmitToolResult, gateway.StableString(response))
	}
	return nil
}

func (runner *Runner) finishActiveAgentRuns(ctx context.Context, result *Result, state *realtimeEventState) {
	for runID := range state.activeAgentRuns {
		callCtx, cancel := context.WithTimeout(ctx, durationOrDefault(runner.options.AgentWaitFallbackDuration, defaultAgentWaitFallback))
		response, err := runner.gateway.Call(callCtx, methodAgentWait, map[string]any{
			"runId":     runID,
			"timeoutMs": max(1, int(durationOrDefault(runner.options.AgentWaitFallbackDuration, defaultAgentWaitFallback)/time.Millisecond)),
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
			detail = gateway.StableString(response)
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
	state.quietDeadline = time.Now().Add(durationOrDefault(duration, defaultQuietDuration))
}

func (state *realtimeEventState) extend(duration time.Duration) {
	deadline := time.Now().Add(durationOrDefault(duration, defaultAgentWaitDuration))
	if deadline.After(state.deadline) {
		state.deadline = deadline
	}
}

func (state *realtimeEventState) appendUnrecoveredFailures(result *Result) {
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

func scrubRealtimeEvents(result Result, include bool) Result {
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

func cloneFrame(frame gateway.Frame) gateway.Frame {
	out := make(gateway.Frame, len(frame))
	for key, value := range frame {
		out[key] = value
	}
	return out
}
