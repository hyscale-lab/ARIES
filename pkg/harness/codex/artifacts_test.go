package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEventRecorderStreamsPrivateRedactedEvents(t *testing.T) {
	recorder, err := newEventRecorder(t.TempDir(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	for _, fragment := range []string{`{"type":"item.started","text":"雪`, `\u0073ecret"}`, "\n"} {
		if n, err := recorder.Write([]byte(fragment)); n != len(fragment) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	content, err := os.ReadFile(recorder.path)
	if err != nil {
		t.Fatal(err)
	}
	var row struct {
		Sequence  int            `json:"sequence"`
		Timestamp time.Time      `json:"timestamp"`
		Event     map[string]any `json:"event"`
	}
	if err := json.Unmarshal(content, &row); err != nil {
		t.Fatal(err)
	}
	if row.Sequence != 1 || row.Timestamp.Before(before) || row.Timestamp.After(time.Now()) || row.Event["text"] != "雪[REDACTED]" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if _, err := recorder.Write([]byte("{\"type\":\"item.completed\"}\n")); err != nil {
		t.Fatal(err)
	}
	if err := recorder.finish(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.finish(); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(recorder.path)
	if len(strings.Split(strings.TrimSpace(string(content)), "\n")) != 2 {
		t.Fatalf("events missing: %s", content)
	}
	info, _ := os.Stat(recorder.path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v", info.Mode())
	}
}

func TestEventRecorderRejectsMalformedBoundedAndTruncatedStreams(t *testing.T) {
	for name, input := range map[string]string{"malformed": "{secret}\n", "truncated": "{\"text\":\"secret", "oversized": strings.Repeat("x", maxEventLineBytes+1), "nonobject": "null\n", "invalid_utf8": "{\"text\":\"\xff\"}\n"} {
		t.Run(name, func(t *testing.T) {
			recorder, err := newEventRecorder(t.TempDir(), []byte("secret"))
			if err != nil {
				t.Fatal(err)
			}
			if n, err := recorder.Write([]byte(input)); n != len(input) || err != nil {
				t.Fatalf("must drain: %d %v", n, err)
			}
			if err := recorder.finish(); err == nil {
				t.Fatal("expected stream error")
			}
			data, _ := os.ReadFile(recorder.path)
			if len(data) != 0 {
				t.Fatalf("unsafe fragment retained: %q", data)
			}
		})
	}
}

func TestWriteRunArtifactsParity(t *testing.T) {
	for _, tc := range []struct {
		name           string
		code           int
		err            error
		status, reason string
	}{
		{"success", 0, nil, "succeeded", "completed"},
		{"reported exit", 7, errors.New("Codex exec exited with status 7"), "failed", "nonzero_exit"},
		{"canceled exit", 137, context.Canceled, "canceled", "canceled"},
		{"deadline exit", 137, context.DeadlineExceeded, "canceled", "deadline_exceeded"}, {"exit", 1, nil, "failed", "nonzero_exit"}, {"failed", -1, errors.New("broken"), "failed", "exec_error"}, {"canceled", -1, context.Canceled, "canceled", "canceled"}, {"deadline", -1, context.DeadlineExceeded, "canceled", "deadline_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			native := []byte("{ \"type\": \"turn.completed\" }\n")
			paths, err := writeRunArtifacts(dir, time.Now().Add(-time.Second), tc.code, tc.err, "done", native, []byte("stderr"), []string{filepath.Join(dir, "telemetry", "events.jsonl")})
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != 5 {
				t.Fatalf("paths: %v", paths)
			}
			for _, path := range paths {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("private artifact: %v %v", info, err)
				}
			}
			data, _ := os.ReadFile(filepath.Join(dir, "trajectory.jsonl"))
			if string(data) != string(native) {
				t.Fatal("native output changed")
			}
			data, _ = os.ReadFile(filepath.Join(dir, "session-outcome.json"))
			var outcome map[string]any
			if err := json.Unmarshal(data, &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome["status"] != tc.status || outcome["end_reason"] != tc.reason || outcome["duration_ms"].(float64) < 1000 {
				t.Fatalf("outcome: %v", outcome)
			}
			data, _ = os.ReadFile(filepath.Join(dir, "agent-result.json"))
			var result map[string]any
			_ = json.Unmarshal(data, &result)
			if result["response"] != "done" {
				t.Fatalf("result: %v", result)
			}
			data, _ = os.ReadFile(filepath.Join(dir, "telemetry.index.json"))
			if !strings.Contains(string(data), "telemetry/events.jsonl") || strings.Contains(string(data), dir) {
				t.Fatalf("index: %s", data)
			}
		})
	}
}

func TestEventRecorderTotalBoundAndClosedFileStillDrain(t *testing.T) {
	for _, failure := range []string{"total", "file"} {
		t.Run(failure, func(t *testing.T) {
			recorder, err := newEventRecorder(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "total" {
				recorder.total = maxOutputBytes
			} else if err := recorder.file.Close(); err != nil {
				t.Fatal(err)
			}
			input := []byte("{\"type\":\"turn.completed\"}\n")
			for range 2 {
				if n, err := recorder.Write(input); n != len(input) || err != nil {
					t.Fatalf("drain: %d %v", n, err)
				}
			}
			if err := recorder.finish(); err == nil {
				t.Fatal("missing failure")
			}
		})
	}
}

func TestWriteRunArtifactsRejectsOutsideTelemetry(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeRunArtifacts(dir, time.Now(), 0, nil, "", nil, nil, []string{"../outside"}); err == nil {
		t.Fatal("accepted outside path")
	}
}
