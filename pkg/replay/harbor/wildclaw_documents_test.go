package harbor

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDocumentPollBindsOriginalOpenClawName(t *testing.T) {
	noError := false
	call := func(tool, args, reply string) WildClawSourceCall {
		return WildClawSourceCall{Name: tool, Arguments: json.RawMessage(args), Reply: &WildClawSourceReply{IsError: &noError, Details: json.RawMessage(reply), Content: json.RawMessage(`[{"type":"text","text":"native output"}]`)}}
	}
	handles := map[string]int{}
	launch, err := documentCall(call("exec", `{"workdir":"/root","command":"true","yieldMs":1000,"timeout":120}`, `{"status":"running","sessionId":"marine-haven","pid":152,"cwd":"/root"}`), handles)
	if err != nil || launch.Operation != "StartProcess" || launch.SourceHandle != "marine-haven" || launch.ExitCode != ProcessRunning || launch.YieldTime != time.Second {
		t.Fatal(launch, err)
	}
	poll := call("process", `{"action":"poll","sessionId":"marine-haven","timeout":120000}`, `{"status":"completed","sessionId":"marine-haven","exitCode":0}`)
	result, err := documentCall(poll, handles)
	if err != nil || result.SessionID != launch.SessionID || result.SourceHandle != launch.SourceHandle || result.YieldTime != 120*time.Second || result.Operation != "PollProcess" {
		t.Fatal(result, err)
	}
	if _, err = documentCall(poll, handles); err == nil {
		t.Fatal("accepted a poll without a source process")
	}
}
