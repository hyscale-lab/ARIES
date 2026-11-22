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
