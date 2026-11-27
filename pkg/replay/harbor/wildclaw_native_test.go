package harbor

import (
	"encoding/json"
	"testing"
)

func TestNativeOpenClawPollPreservesSignalAndExitZero(t *testing.T) {
	details := json.RawMessage(`{"status":"failed","exitCode":0,"sessionId":"new-handle"}`)
	content := json.RawMessage(`[{"type":"text","text":"(no new output)\n\nProcess exited with signal SIGTERM."}]`)
	got, err := DecodeOpenClawResult("process", content, details)
	if err != nil || got.NativeToolSignal != "SIGTERM" || got.ExitCode != 0 ||
		sameNativeStatus(json.RawMessage(`{"status":"completed","exitCode":0}`), got.NativeToolDetails) ||
		!sameNativeStatus(json.RawMessage(`{"status":"failed","exitCode":0,"sessionId":"old-handle"}`), got.NativeToolDetails) {
		t.Fatalf("poll %+v, error %v", got, err)
	}
}
