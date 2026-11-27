package harbor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

const (
	wildClawScholar      = "73b9e9bc867c86be5c7c9125553971a9832d6760a256c4091f69f8c3a60308de"
	wildClawAffiliations = "73a1f7bfae3bf80a7fa25f6d47eda911a69492e85ed0a79e63b05c83ef6356da"
	wildClawResume       = "0b11c9b18a35d05428c3543a84095e06347d47357ba4939b75c931fa39946b6b"
)

// DecodeOpenClawResult preserves the tool's status independently of its reported process exit.
func DecodeOpenClawResult(tool string, rawContent, details json.RawMessage) (core.CommandResult, error) {
	result := core.CommandResult{NativeToolDetails: details}
	var d struct {
		Status   string
		ExitCode *int
	}
	var content []struct{ Type, Text, MimeType, Data string }
	if len(details) != 0 && json.Unmarshal(details, &d) != nil ||
		json.Unmarshal(rawContent, &content) != nil || len(content) == 0 || content[0].Type != "text" {
		return result, errors.New("invalid OpenClaw tool result")
	}
	result.Stdout = content[0].Text
	switch {
	case d.Status == "running":
		result.ExitCode = ProcessRunning
	case d.ExitCode != nil:
		result.ExitCode = *d.ExitCode
	case d.Status == "failed" || d.Status == "error":
		result.ExitCode = 1
	case tool == "read":
		if len(content) != 2 || content[1].Type != "image" || content[1].MimeType != "image/jpeg" || content[1].Data == "" {
			return result, errors.New("expected a native resized JPEG attachment")
		}
		// Compare the native attachment and the dimension/format caption together.
		result.Stdout = content[0].Text + "\n" + content[1].Data
	case d.Status != "completed":
		return result, fmt.Errorf("unexpected native %s status %q", tool, d.Status)
	}
	if d.Status == "failed" {
		last := content[0].Text[strings.LastIndex(content[0].Text, "\n")+1:]
		if signal, ok := strings.CutPrefix(last, "Process exited with signal "); ok && strings.HasPrefix(signal, "SIG") && strings.HasSuffix(signal, ".") {
			result.NativeToolSignal = strings.TrimSuffix(signal, ".")
		}
	}
	return result, nil
}

func nativeSourceCall(c WildClawSourceCall, handles map[string]int, cwd string) (replay.Call, error) {
	call := replay.Call{Sequence: c.Sequence, ToolCallID: c.ID}
	if c.Reply == nil || c.Reply.IsError == nil || *c.Reply.IsError {
		return call, errors.New("missing native source reply")
	}
	switch c.Name {
	case "read":
		call.Operation = "OpenClawImageRead"
	case "image":
		call.Operation = "OpenClawImage"
	case "process", "exec":
		if c.Name == "process" && equalJSON(c.Arguments, []byte(`{"action":"list"}`)) {
			call.Operation = "OpenClawProcessList"
		} else {
			var err error
			call, err = documentCall(c, handles, cwd)
			if err != nil {
				return call, err
			}
		}
	default:
		return call, errors.New("unreviewed native source tool")
	}
	result, err := DecodeOpenClawResult(c.Name, c.Reply.Content, c.Reply.Details)
	if err != nil {
		return call, err
	}
	call.NativeTool, call.NativeSignal, call.SourceToolDetails = c.Name, result.NativeToolSignal, c.Reply.Details
	call.Body, call.ResponseCode, call.ExitCode = c.Arguments, c.Reply.Status, int32(result.ExitCode)
	call.ResponseStdout = []byte(result.Stdout)
	call.ResponseSHA256, call.ResponseBytes = digest(call.ResponseStdout), int64(len(call.ResponseStdout))
	return call, nil
}

func sameNativeStatus(source, live json.RawMessage) bool {
	var expected, observed struct {
		Status   string
		ExitCode *int
	}
	if len(source) == 0 || len(live) == 0 {
		return bytes.Equal(source, live)
	}
	if json.Unmarshal(source, &expected) != nil || json.Unmarshal(live, &observed) != nil || expected.Status != observed.Status {
		return false
	}
	if expected.ExitCode == nil || observed.ExitCode == nil {
		return expected.ExitCode == nil && observed.ExitCode == nil
	}
	return *expected.ExitCode == *observed.ExitCode
}
