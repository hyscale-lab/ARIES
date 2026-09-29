package codex

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func traceArchive(t *testing.T, name, content string, kind byte) *bytes.Reader {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	header := &tar.Header{Name: name, Mode: 0600, Typeflag: kind}
	if kind == tar.TypeReg {
		header.Size = int64(len(content))
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if header.Size > 0 {
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buffer.Bytes())
}

func nativeTraceRow(seq, milliseconds int, thread, kind, fields string) string {
	return fmt.Sprintf("{\"schema_version\":1,\"seq\":%d,\"wall_time_unix_ms\":%d,\"rollout_id\":\"rollout\",\"thread_id\":%q,\"codex_turn_id\":\"turn\",\"payload\":{\"type\":%q%s}}\n", seq, milliseconds, thread, kind, fields)
}

func TestCollectNativeTraceCallsAndPrivacy(t *testing.T) {
	content := nativeTraceRow(1, 1000, "parent", "inference_started", `,"inference_call_id":"one","model":"api-key","provider_name":"local","request_payload":{"path":"payloads/secret"}`) +
		nativeTraceRow(2, 1100, "child", "inference_started", `,"inference_call_id":"two"`) +
		nativeTraceRow(3, 1200, "parent", "inference_completed", `,"inference_call_id":"one","response_id":"response","response_payload":{"secret":"unretained"}`) +
		nativeTraceRow(4, 1300, "child", "inference_cancelled", `,"inference_call_id":"two","reason":"api-key"`) +
		nativeTraceRow(5, 1400, "parent", "inference_started", `,"inference_call_id":"three"`) +
		nativeTraceRow(6, 1450, "parent", "code_cell_started", `,"source_js":"unretained"`)
	directory := t.TempDir()
	paths, err := collectNativeTrace(traceArchive(t, "trace.jsonl", content, tar.TypeReg), directory, []byte("api-key"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v", paths)
	}
	var calls []llmCall
	for _, filename := range paths {
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("unretained")) || bytes.Contains(data, []byte("api-key")) || bytes.Contains(data, []byte("payloads/secret")) {
			t.Fatalf("private content retained: %s", data)
		}
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("permissions: %v %v", info, err)
		}
		if filepath.Base(filename) == "llm-calls.jsonl" {
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
				var call llmCall
				if err := json.Unmarshal(line, &call); err != nil {
					t.Fatal(err)
				}
				calls = append(calls, call)
			}
		}
	}
	if len(calls) != 3 || calls[0].Status != "completed" || calls[0].DurationMS == nil || *calls[0].DurationMS != 200 || calls[1].ThreadID != "child" || calls[1].Status != "cancelled" || calls[2].Status != "incomplete" || calls[2].EndedAt != nil || calls[2].DurationMS != nil {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestCollectNativeTraceFailureAndEscapedKey(t *testing.T) {
	content := nativeTraceRow(1, 1000, "parent", "inference_started", `,"inference_call_id":"one","model":"key\"secret"`) +
		nativeTraceRow(2, 1100, "parent", "inference_failed", `,"inference_call_id":"one","error":"sensitive error"`)
	paths, err := collectNativeTrace(traceArchive(t, "trace.jsonl", content, tar.TypeReg), t.TempDir(), []byte(`key"secret`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	var call llmCall
	if err := json.Unmarshal(data, &call); err != nil {
		t.Fatal(err)
	}
	if call.Status != "failed" || call.Model == `key"secret` || call.DurationMS == nil || *call.DurationMS != 100 {
		t.Fatalf("call = %+v", call)
	}
}

func TestCollectNativeTraceRejectsOversizedAndSymlinkOutput(t *testing.T) {
	content := nativeTraceRow(1, 1000, "parent", "inference_started", `,"inference_call_id":"one"`)
	directory := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(directory, "telemetry")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectNativeTrace(traceArchive(t, "trace.jsonl", content, tar.TypeReg), directory, nil); err == nil {
		t.Fatal("followed output symlink")
	}
	if _, err := collectNativeTrace(traceArchive(t, "trace.jsonl", strings.Repeat("x", maxEventLineBytes+1), tar.TypeReg), t.TempDir(), nil); err == nil {
		t.Fatal("accepted oversized line")
	}
}

func TestCollectNativeTraceRejectsUnsafeAndBrokenInput(t *testing.T) {
	valid := nativeTraceRow(1, 1000, "parent", "inference_started", `,"inference_call_id":"one"`)
	for _, test := range []struct {
		name, entry, content string
		kind                 byte
	}{
		{"path", "../trace.jsonl", valid, tar.TypeReg},
		{"link", "trace.jsonl", "", tar.TypeSymlink},
		{"payload", "payloads/1.json", "private", tar.TypeReg},
		{"malformed", "trace.jsonl", "{bad}\n", tar.TypeReg},
		{"duplicate", "trace.jsonl", valid + valid, tar.TypeReg},
		{"no start", "trace.jsonl", nativeTraceRow(1, 1000, "parent", "inference_completed", `,"inference_call_id":"one"`), tar.TypeReg},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := collectNativeTrace(traceArchive(t, test.entry, test.content, test.kind), t.TempDir(), nil); err == nil {
				t.Fatal("unsafe trace accepted")
			}
		})
	}
}

func TestCollectNativeTracePreservesPartialFinalLine(t *testing.T) {
	content := nativeTraceRow(1, 1000, "parent", "inference_started", `,"inference_call_id":"one"`) + `{"schema_version":`
	paths, err := collectNativeTrace(traceArchive(t, "trace.jsonl", content, tar.TypeReg), t.TempDir(), nil)
	if err == nil || len(paths) != 2 {
		t.Fatalf("paths=%v error=%v", paths, err)
	}
	data, _ := os.ReadFile(paths[1])
	if !strings.Contains(string(data), `"status":"incomplete"`) {
		t.Fatalf("lost completed lines: %s", data)
	}
}
