package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const maxEventLineBytes = 1 << 20

// eventRecorder timestamps complete native events at host receipt. Recording
// failures are latched so Docker output still drains and cleanup can complete.
type eventRecorder struct {
	path     string
	file     *os.File
	key      []byte
	pending  []byte
	total    int
	sequence uint64
	err      error
	closed   bool
}

func newEventRecorder(artifactDir string, key []byte) (*eventRecorder, error) {
	path := filepath.Join(artifactDir, "telemetry", "events.jsonl")
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	return &eventRecorder{path: path, file: file, key: bytes.Clone(key)}, nil
}

func (recorder *eventRecorder) Write(content []byte) (int, error) {
	length := len(content)
	if recorder.closed || recorder.err != nil {
		return length, nil
	}
	if length > maxOutputBytes-recorder.total {
		recorder.err = errors.New("Codex event stream exceeded its bound")
		clear(recorder.pending)
		recorder.pending = nil
		return length, nil
	}
	recorder.total += length
	for len(content) > 0 {
		end := bytes.IndexByte(content, '\n')
		size := len(content)
		if end >= 0 {
			size = end
		}
		if size > maxEventLineBytes-len(recorder.pending) {
			recorder.err = errors.New("Codex event line exceeded its bound")
			break
		}
		recorder.pending = append(recorder.pending, content[:size]...)
		if end < 0 {
			break
		}
		if err := recorder.record(recorder.pending); err != nil {
			recorder.err = err
			break
		}
		clear(recorder.pending)
		recorder.pending = recorder.pending[:0]
		content = content[end+1:]
	}
	if recorder.err != nil {
		clear(recorder.pending)
		recorder.pending = nil
	}
	return length, nil
}

func (recorder *eventRecorder) record(line []byte) error {
	timestamp := time.Now().UTC()
	if !utf8.Valid(line) {
		return errors.New("Codex event contains invalid UTF-8")
	}
	var event map[string]any
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&event); err != nil || event == nil || !json.Valid(line) {
		return errors.New("Codex event is not a complete JSON object")
	}
	event = redactEventValue(event, recorder.key).(map[string]any)
	recorder.sequence++
	row, err := json.Marshal(struct {
		Sequence  uint64         `json:"sequence"`
		Timestamp time.Time      `json:"timestamp"`
		Event     map[string]any `json:"event"`
	}{recorder.sequence, timestamp, event})
	if err != nil {
		return fmt.Errorf("encode Codex event: %w", err)
	}
	_, err = recorder.file.Write(append(row, '\n'))
	return err
}

func redactEventValue(value any, key []byte) any {
	switch value := value.(type) {
	case string:
		return string(redact([]byte(value), key))
	case []any:
		for index := range value {
			value[index] = redactEventValue(value[index], key)
		}
		return value
	case map[string]any:
		result := make(map[string]any, len(value))
		for name, child := range value {
			result[string(redact([]byte(name), key))] = redactEventValue(child, key)
		}
		return result
	default:
		return value
	}
}

func (recorder *eventRecorder) finish() error {
	if recorder.closed {
		return recorder.err
	}
	recorder.closed = true
	if len(recorder.pending) > 0 && recorder.err == nil {
		recorder.err = errors.New("Codex event stream ended with an incomplete line")
	}
	clear(recorder.pending)
	recorder.pending = nil
	clear(recorder.key)
	recorder.err = errors.Join(recorder.err, recorder.file.Sync(), recorder.file.Close())
	return recorder.err
}

// Inputs are already credential-redacted by the harness. Native stdout remains
// unchanged; timing and normalized terminal outcome live in separate artifacts.
func writeRunArtifacts(artifactDir string, started time.Time, exitCode int, runErr error, final string, nativeStdout, stderr []byte, telemetryPaths []string) ([]string, error) {
	ended := time.Now()
	outcome := struct {
		Status     string `json:"status"`
		EndReason  string `json:"end_reason"`
		ExitCode   int    `json:"exit_code"`
		StartedAt  string `json:"started_at"`
		EndedAt    string `json:"ended_at"`
		DurationMS int64  `json:"duration_ms"`
	}{"succeeded", "completed", exitCode, started.UTC().Format(time.RFC3339Nano), ended.UTC().Format(time.RFC3339Nano), ended.Sub(started).Milliseconds()}
	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		outcome.Status, outcome.EndReason = "canceled", "deadline_exceeded"
	case errors.Is(runErr, context.Canceled):
		outcome.Status, outcome.EndReason = "canceled", "canceled"
	case exitCode > 0:
		outcome.Status, outcome.EndReason = "failed", "nonzero_exit"
	case runErr != nil || exitCode < 0:
		outcome.Status, outcome.EndReason = "failed", "exec_error"
	}
	errorText := ""
	if runErr != nil {
		errorText = runErr.Error()
	}
	result := struct {
		Response string `json:"response"`
		Error    string `json:"error,omitempty"`
	}{final, errorText}
	relative := make([]string, 0, len(telemetryPaths))
	for _, path := range telemetryPaths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(artifactDir, path)
		}
		rel, err := filepath.Rel(artifactDir, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("Codex telemetry path escapes artifact directory")
		}
		relative = append(relative, filepath.ToSlash(rel))
	}
	outcomeJSON, _ := json.MarshalIndent(outcome, "", "  ")
	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	indexJSON, _ := json.MarshalIndent(struct {
		Paths []string `json:"paths"`
	}{relative}, "", "  ")
	paths := make([]string, 0, 5)
	var errs []error
	for _, artifact := range []struct {
		name string
		data []byte
	}{
		{"trajectory.jsonl", nativeStdout}, {"stderr.log", stderr}, {"session-outcome.json", append(outcomeJSON, '\n')}, {"agent-result.json", append(resultJSON, '\n')}, {"telemetry.index.json", append(indexJSON, '\n')},
	} {
		path := filepath.Join(artifactDir, artifact.name)
		if err := writeArtifact(path, artifact.data); err != nil {
			errs = append(errs, fmt.Errorf("write Codex %s: %w", artifact.name, err))
			continue
		}
		paths = append(paths, path)
	}
	return paths, errors.Join(errs...)
}
