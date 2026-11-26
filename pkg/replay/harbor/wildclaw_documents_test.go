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
	missing, err := documentCall(call("exec", `{"command":"python --version","yieldMs":1000,"timeout":120}`, `{"status":"error","tool":"exec","error":"sh: 1: python: not found\n\nCommand not found"}`), nil, "/root")
	if err != nil || missing.Workdir != "/root" || missing.ExitCode != 127 {
		t.Fatal(missing, err)
	}
	handles := map[string]int{}
	launch, err := documentCall(call("exec", `{"command":"true","yieldMs":1000,"timeout":120}`, `{"status":"running","sessionId":"marine-haven","pid":152,"cwd":"/root"}`), handles, "/root")
	if err != nil || launch.Workdir != "/root" || launch.Operation != "StartProcess" || launch.SourceHandle != "marine-haven" || launch.ExitCode != ProcessRunning || launch.YieldTime != time.Second {
		t.Fatal(launch, err)
	}
	poll := call("process", `{"action":"poll","sessionId":"marine-haven","timeout":120000}`, `{"status":"completed","sessionId":"marine-haven","exitCode":0}`)
	result, err := documentCall(poll, handles, "/root")
	if err != nil || result.SessionID != launch.SessionID || result.SourceHandle != launch.SourceHandle || result.YieldTime != 120*time.Second || result.Operation != "PollProcess" {
		t.Fatal(result, err)
	}
	if _, err = documentCall(poll, handles, "/root"); err == nil {
		t.Fatal("accepted a poll without a source process")
	}
}
