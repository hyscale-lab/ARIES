package harbor

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOriginalOmittedExecYieldUsesTenSeconds(t *testing.T) {
	call := indexedCall{step: 15, call: toolCall{Name: "exec_command", Args: json.RawMessage(`{"cmd":"true"}`)}, output: "Chunk ID: example\nWall time: 10.0019 seconds\nProcess running with session ID 70246\nOriginal token count: 0\nOutput:\n"}
	actual, err := guestCall(call, "/app", time.Time{}, time.Time{}, 1)
	if err != nil || actual.YieldTime != 10*time.Second {
		t.Fatalf("source yield %s, err %v", actual.YieldTime, err)
	}
}

func TestOriginalEmptyPollYieldRespectsCodexBounds(t *testing.T) {
	for _, test := range []struct {
		args string
		want time.Duration
	}{
		{`{"session_id":13}`, 5 * time.Second},
		{`{"session_id":13,"yield_time_ms":20000}`, 20 * time.Second},
		{`{"session_id":13,"yield_time_ms":9223372036854775807}`, 5 * time.Minute},
	} {
		call := indexedCall{call: toolCall{Name: "write_stdin", Args: json.RawMessage(test.args)}, output: "Process running with session ID 13\nOutput:\n"}
		actual, err := processPoll(call, time.Time{}, time.Time{}, 1)
		if err != nil || actual.YieldTime != test.want || actual.SessionID != 13 || actual.ExitCode != ProcessRunning {
			t.Fatalf("source poll %s: %+v, err %v", test.args, actual, err)
		}
	}
}

func TestOriginalPipeInterruptYieldAndInput(t *testing.T) {
	for _, test := range []struct {
		args string
		want time.Duration
	}{
		{`{"session_id":13,"chars":"\u0003","yield_time_ms":1}`, 250 * time.Millisecond},
		{`{"session_id":13,"chars":"\u0003","yield_time_ms":1000}`, time.Second},
		{`{"session_id":13,"chars":"\u0003","yield_time_ms":9223372036854775807}`, 30 * time.Second},
		{`{"session_id":13,"chars":"\u0003\u0003"}`, 0},
		{`{"session_id":13,"chars":"other"}`, 0},
	} {
		call := indexedCall{call: toolCall{Name: "write_stdin", Args: json.RawMessage(test.args)}, output: "Process exited with code 130\nOutput:\n"}
		got, err := processPoll(call, time.Time{}, time.Time{}, 1)
		if test.want == 0 {
			if err == nil {
				t.Fatalf("unsupported input accepted: %s", test.args)
			}
		} else if err != nil || !got.PipeInterrupt || got.YieldTime != test.want || got.SessionID != 13 || got.ExitCode != 130 {
			t.Fatalf("source interrupt %s: %+v, err %v", test.args, got, err)
		}
	}
}
