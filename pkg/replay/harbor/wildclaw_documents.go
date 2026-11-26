package harbor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay"
)

const wildClawPDF = "5d721acdbeeedc7cb51a7779d7a8328756422f2d512620bcb400a5f50663be1f"
const wildClawConflicts = "7a020f97fe201c8153c08e219839b7a6b6d77ae6f6755a42588c27a2f42fef39"

// The source PID is only a key for the open live stream. The native OpenClaw
// session name accompanies the receipt and is never passed to the guest OS.
func documentCall(c WildClawSourceCall, handles map[string]int) (replay.Call, error) {
	call := replay.Call{Sequence: c.Sequence, ToolCallID: c.ID}
	if c.Reply == nil || c.Reply.IsError == nil || *c.Reply.IsError {
		return call, errors.New("missing reviewed native reply")
	}
	var args struct {
		Path, Command, Workdir, Content, OldText, NewText, Action, SessionID string
		YieldMS                                                              int
		Timeout                                                              int
	}
	var d struct {
		Status, SessionID, CWD, Error string
		PID                           int
		ExitCode                      *int32
	}
	var content []struct{ Type, Text string }
	if err := json.Unmarshal(c.Arguments, &args); err != nil {
		return call, err
	}
	if err := json.Unmarshal(c.Reply.Content, &content); err != nil {
		return call, err
	}
	if len(content) != 1 || content[0].Type != "text" {
		return call, errors.New("expected a text reply")
	}
	if len(c.Reply.Details) > 0 {
		if err := json.Unmarshal(c.Reply.Details, &d); err != nil {
			return call, err
		}
	}
	call.ResponseStdout = []byte(content[0].Text)
	switch {
	case c.Name == "exec" && d.Status == "running":
		if d.PID <= 0 || d.SessionID == "" || handles[d.SessionID] != 0 || args.Command == "" || args.Workdir != d.CWD || args.YieldMS <= 0 {
			return call, errors.New("invalid OpenClaw running reply")
		}
		handles[d.SessionID] = d.PID
		call.SourceHandle, call.SessionID = d.SessionID, d.PID
		call.Operation, call.Script, call.Workdir = "StartProcess", args.Command, args.Workdir
		call.YieldTime, call.Timeout = time.Duration(args.YieldMS)*time.Millisecond, time.Duration(args.Timeout)*time.Second
		call.ResponseCode, call.ExitCode = "running", ProcessRunning
		call.ResponseStdout = nil // The source running reply is a handle notification.
	case c.Name == "process":
		if args.Action != "poll" || args.Timeout <= 0 || args.SessionID != d.SessionID || handles[args.SessionID] == 0 || d.Status != "completed" || d.ExitCode == nil {
			return call, errors.New("unbound or unsupported process poll")
		}
		call.SourceHandle, call.SessionID = d.SessionID, handles[d.SessionID]
		delete(handles, d.SessionID)
		call.Operation, call.ExitCode, call.ResponseCode = "PollProcess", *d.ExitCode, d.Status
		call.YieldTime = time.Duration(args.Timeout) * time.Millisecond
		call.ResponseStdout = []byte(strings.TrimSuffix(content[0].Text, fmt.Sprintf("\n\nProcess exited with code %d.", call.ExitCode)))
	case c.Name == "image" && d.Status == "error" && d.Error == "Model does not support images: new-api/gpt-5.6-sol":
		call.Operation, call.Body, call.ResponseCode = "OpenClawImage", c.Arguments, d.Status
	case c.Name == "read" && d.Status == "error":
		if args.Path == "" || d.Error != "ENOENT: no such file or directory, access '"+args.Path+"'" {
			return call, errors.New("unreviewed file read error")
		}
		call.Operation, call.Path, call.ResponseCode, call.ExitCode = "ReadFile", args.Path, "ENOENT", 1
	case c.Name == "write" || c.Name == "edit":
		if args.Path == "" || (c.Name == "edit" && args.OldText == "") {
			return call, errors.New("missing file operation arguments")
		}
		call.Operation = "WriteFile"
		if c.Name == "edit" {
			call.Operation = "EditFile"
		}
		call.Path, call.Body, call.ResponseCode = args.Path, c.Arguments, "success"
	default:
		return wildClawLiveCall(c)
	}
	call.ResponseSHA256, call.ResponseBytes = digest(call.ResponseStdout), int64(len(call.ResponseStdout))
	return call, nil
}
