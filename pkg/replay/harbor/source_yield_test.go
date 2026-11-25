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
