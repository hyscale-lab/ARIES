package codex

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const rolloutTraceContainerPath = stagedRoot + "/codex/traces"
const maxNativeTraceBytes = 64 << 20
const maxNativeTraceEvents = 100000

// Only structural fields are retained. Native payloads can contain the entire
// growing model context and are deliberately neither downloaded nor exported.
type nativeTraceEvent struct {
	SchemaVersion int                `json:"schema_version"`
	Sequence      uint64             `json:"seq"`
	WallTimeMS    int64              `json:"wall_time_unix_ms"`
	RolloutID     string             `json:"rollout_id"`
	ThreadID      string             `json:"thread_id,omitempty"`
	TurnID        string             `json:"codex_turn_id,omitempty"`
	Payload       nativeTracePayload `json:"payload"`
}

type nativeTracePayload struct {
	Type               string `json:"type"`
	InferenceCallID    string `json:"inference_call_id,omitempty"`
	ToolCallID         string `json:"tool_call_id,omitempty"`
	ModelVisibleCallID string `json:"model_visible_call_id,omitempty"`
	ThreadID           string `json:"thread_id,omitempty"`
	TurnID             string `json:"codex_turn_id,omitempty"`
	Model              string `json:"model,omitempty"`
	ProviderName       string `json:"provider_name,omitempty"`
	ResponseID         string `json:"response_id,omitempty"`
	UpstreamRequestID  string `json:"upstream_request_id,omitempty"`
	Status             string `json:"status,omitempty"`
}

// A call is one native inference lifecycle, not necessarily one HTTP retry.
// No first-token field exists: the native trace does not observe that boundary.
type llmCall struct {
	Source            string     `json:"source"`
	RolloutID         string     `json:"rollout_id"`
	CallID            string     `json:"call_id"`
	ThreadID          string     `json:"thread_id,omitempty"`
	TurnID            string     `json:"turn_id,omitempty"`
	Model             string     `json:"model,omitempty"`
	ProviderName      string     `json:"provider_name,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
	DurationMS        *int64     `json:"duration_ms,omitempty"`
	Status            string     `json:"status"`
	ResponseID        string     `json:"response_id,omitempty"`
	UpstreamRequestID string     `json:"upstream_request_id,omitempty"`
}

// collectNativeTrace accepts Docker's archive of trace.jsonl, not the trace
// root (which also contains large private payload blobs). Complete events are
// retained even when cancellation leaves a partial final line. The error still
// reports that collection was incomplete; callers must retain returned paths.
func collectNativeTrace(archive io.Reader, artifactDir string, key []byte) ([]string, error) {
	bounded := &io.LimitedReader{R: archive, N: maxNativeTraceBytes + (1 << 20)}
	reader := tar.NewReader(bounded)
	var native bytes.Buffer
	var calls []llmCall
	indices := make(map[[2]string]int)
	sequences := make(map[string]uint64)
	total, eventCount, files := int64(0), 0, 0
	var collectErr error
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			collectErr = errors.New("read Codex trace archive")
			break
		}
		clean := path.Clean(header.Name)
		if clean != header.Name || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(clean, "\\\x00") {
			collectErr = errors.New("unsafe Codex trace archive path")
			break
		}
		if header.Typeflag != tar.TypeReg || path.Base(clean) != "trace.jsonl" {
			collectErr = errors.New("Codex trace archive must contain only regular trace.jsonl files")
			break
		}
		if header.Size < 0 || header.Size > maxNativeTraceBytes-total {
			collectErr = errors.New("Codex native trace exceeded its byte bound")
			break
		}
		total += header.Size
		files++
		lines := bufio.NewReaderSize(reader, 64<<10)
		for {
			line, readErr := readTraceLine(lines)
			if len(line) == 0 && readErr == io.EOF {
				break
			}
			if readErr != nil {
				collectErr = errors.New("Codex native trace has an incomplete or oversized event")
				break
			}
			eventCount++
			if eventCount > maxNativeTraceEvents {
				collectErr = errors.New("Codex native trace exceeded its event bound")
				break
			}
			var event nativeTraceEvent
			if err := json.Unmarshal(line, &event); err != nil || event.SchemaVersion != 1 || event.RolloutID == "" || event.WallTimeMS <= 0 || event.Sequence <= sequences[event.RolloutID] || event.Payload.Type == "" {
				collectErr = errors.New("invalid Codex native trace event")
				break
			}
			sequences[event.RolloutID] = event.Sequence
			if err := recordInference(event, &calls, indices); err != nil {
				collectErr = err
				break
			}
			if err := encodePrivateTrace(&native, event, key); err != nil {
				collectErr = err
				break
			}
		}
		if collectErr != nil {
			break
		}
	}
	if bounded.N == 0 {
		collectErr = errors.Join(collectErr, errors.New("Codex trace archive exceeded its byte bound"))
	}
	if files == 0 || eventCount == 0 {
		return nil, errors.Join(collectErr, errors.New("Codex native trace is missing or empty"))
	}
	var normalized bytes.Buffer
	for _, call := range calls {
		if err := encodePrivateTrace(&normalized, call, key); err != nil {
			return nil, errors.Join(collectErr, err)
		}
	}
	var paths []string
	for _, output := range []struct {
		name    string
		content []byte
	}{
		{"native-trace.jsonl", native.Bytes()},
		{"llm-calls.jsonl", normalized.Bytes()},
	} {
		filename := filepath.Join(artifactDir, "telemetry", output.name)
		if err := writeArtifact(filename, output.content); err != nil {
			return paths, errors.Join(collectErr, err)
		}
		paths = append(paths, filename)
	}
	return paths, collectErr
}

func readTraceLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > maxEventLineBytes-len(line) {
			return nil, errors.New("trace event exceeds bound")
		}
		line = append(line, part...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

func recordInference(event nativeTraceEvent, calls *[]llmCall, indices map[[2]string]int) error {
	payload := event.Payload
	if !strings.HasPrefix(payload.Type, "inference_") {
		return nil
	}
	if payload.InferenceCallID == "" {
		return errors.New("Codex inference event has no call ID")
	}
	identity := [2]string{event.RolloutID, payload.InferenceCallID}
	index, found := indices[identity]
	if payload.Type == "inference_started" {
		if found {
			return errors.New("duplicate Codex inference start")
		}
		thread, turn := event.ThreadID, event.TurnID
		if thread == "" {
			thread = payload.ThreadID
		}
		if turn == "" {
			turn = payload.TurnID
		}
		indices[identity] = len(*calls)
		*calls = append(*calls, llmCall{Source: "codex_native_rollout_trace", RolloutID: event.RolloutID, CallID: payload.InferenceCallID, ThreadID: thread, TurnID: turn, Model: payload.Model, ProviderName: payload.ProviderName, StartedAt: time.UnixMilli(event.WallTimeMS).UTC(), Status: "incomplete"})
		return nil
	}
	if payload.Type != "inference_completed" && payload.Type != "inference_failed" && payload.Type != "inference_cancelled" {
		return errors.New("unknown Codex inference event")
	}
	if !found {
		return errors.New("Codex inference terminal event has no start")
	}
	call := &(*calls)[index]
	if call.EndedAt != nil || (event.ThreadID != "" && event.ThreadID != call.ThreadID) {
		return errors.New("conflicting Codex inference terminal event")
	}
	end := time.UnixMilli(event.WallTimeMS).UTC()
	duration := event.WallTimeMS - call.StartedAt.UnixMilli()
	if duration < 0 {
		return errors.New("Codex inference clock moved backwards")
	}
	call.EndedAt, call.DurationMS = &end, &duration
	call.Status = strings.TrimPrefix(payload.Type, "inference_")
	call.ResponseID, call.UpstreamRequestID = payload.ResponseID, payload.UpstreamRequestID
	return nil
}

func encodePrivateTrace(output io.Writer, value any, key []byte) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode Codex trace metadata: %w", err)
	}
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(redactEventValue(fields, key))
}
