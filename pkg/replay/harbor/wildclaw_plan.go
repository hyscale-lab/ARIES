package harbor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

// LoadWildClawTimeline creates a source-clock playback with no executable calls.
func LoadWildClawTimeline(trace []byte) (*Recording, error) {
	source, err := ParseWildClawSource(trace)
	if err != nil {
		return nil, err
	}
	return wildClawRecording(source, true)
}

// LoadWildClawLive binds exact source bytes to the reviewed guest operations.
func LoadWildClawLive(trace []byte) (*Recording, error) {
	if !reviewedWildClawLive(digest(trace)) {
		return nil, errors.New("no reviewed live WildClaw plan for this source digest")
	}
	source, err := ParseWildClawSource(trace)
	if err != nil {
		return nil, err
	}
	return wildClawRecording(source, false)
}

func reviewedWildClawLive(sha string) bool {
	return braveSearchSource(sha) || localFileSource(sha) || sha == wildClawCalendarDigest || sha == wildClawImageCategoryDigest || sha == wildClawPuzzleDigest || sha == wildClawPDF || sha == wildClawConflicts
}

func wildClawRecording(s *WildClawSource, playback bool) (*Recording, error) {
	trace := &OpenClawTrace{
		Version: s.Version, RunID: s.RunID, SHA256: s.SHA256, CWD: s.CWD,
		Start: s.Start, End: s.End, ObservedThrough: s.Records[len(s.Records)-1].Time,
		Status: s.Status, StatusReason: s.StatusReason,
	}
	evaluation := core.Evaluation{Status: "source_reported", Score: s.Score}
	if reviewedWildClawLive(s.SHA256) {
		trace.Revision = wildClawDatasetRevision
		evaluation.Status = "published_source"
	}
	r := &Recording{
		Task: s.TaskID, Session: s.SessionID, Model: s.Model, OpenClawTrace: trace, SourcePlayback: playback,
		Plan: replay.Plan{SourceRunID: s.RunID, SourceTaskID: s.TaskID, StartedAt: s.Start, Evaluation: evaluation},
	}
	batches := make(map[int]WildClawSourceBatch, len(s.Batches))
	for _, batch := range s.Batches {
		batches[batch.Request.Line] = batch
	}
	replies := map[int]Event{}
	handles := map[string]int{}
	var previous time.Time
	for _, row := range s.Records {
		if previous.IsZero() && row.Role != "user" {
			continue // The session clock starts at the prompt, after harness metadata.
		}
		e := Event{Line: row.Line, Kind: row.Type, SourceTime: row.Time, Role: row.Role, SourceRecordID: row.ID, SourceParentID: row.ParentID, Basis: "source_origin"}
		if !previous.IsZero() {
			e.Basis, e.Wait = "inferred_inter_event_gap", row.Time.Sub(previous)
		}
		previous = row.Time
		if reply, ok := replies[row.Line]; ok {
			e.Kind, e.CallID, e.Step, e.ParallelSize = "toolResult", reply.CallID, reply.Step, reply.ParallelSize
			e.SourceSequence, e.Sequence = reply.SourceSequence, reply.Sequence
			e.SourceReplyStatus, e.SourceReplyError = reply.SourceReplyStatus, reply.SourceReplyError
			if playback {
				e.Basis = "recorded_inter_event_gap_to_tool_reply"
			} else {
				e.Basis, e.Wait = "replaced_by_live_guest_reply", 0
			}
			r.Events = append(r.Events, e)
			continue
		}

		e.Step = len(r.Steps) + 1
		r.Steps = append(r.Steps, Step{ID: e.Step, Source: row.Role, Timestamp: row.Time})
		batch, ok := batches[row.Line]
		if !ok {
			if row.Role == "assistant" && row.StopReason == "stop" {
				r.Plan.FinalWait = e.Wait
			}
			r.Events = append(r.Events, e)
			continue
		}

		if len(batch.Calls) > 1 {
			e.ParallelSize = len(batch.Calls)
		}
		for index, c := range batch.Calls {
			request := e
			request.Kind, request.CallID, request.SourceSequence = c.Name, c.ID, c.Sequence
			if index > 0 {
				request.Wait, request.Basis = 0, "recorded_assistant_batch_no_subcall_spacing"
			}
			if !playback {
				var call replay.Call
				var err error
				if braveSearchSource(s.SHA256) && (c.Name == "web_search" || c.Name == "web_fetch") {
					call, err = openClawWebCall(c)
				} else if (s.SHA256 == wildClawScholar || s.SHA256 == wildClawAffiliations) && (c.Name == "process" || (c.Name == "exec" && c.Reply != nil && c.Reply.Status == "running")) ||
					s.SHA256 == wildClawResume && (c.Name == "read" || c.Name == "image") {
					call, err = nativeSourceCall(c, handles, s.CWD)
				} else if braveSearchSource(s.SHA256) || localFileSource(s.SHA256) || s.SHA256 == wildClawPDF || s.SHA256 == wildClawConflicts {
					call, err = documentCall(c, handles, s.CWD)
				} else {
					call, err = wildClawLiveCall(c, s.CWD)
				}
				if err != nil {
					return nil, fmt.Errorf("WildClaw source line %d, call %d: %w", row.Line, c.Sequence, err)
				}
				call.StartOffset = row.Time.Sub(s.Start)
				r.Plan.Calls = append(r.Plan.Calls, call)
				request.Sequence = c.Sequence
			}
			if c.Reply == nil {
				request.Unanswered = true
				trace.UnansweredCalls = append(trace.UnansweredCalls, c.ID)
			} else {
				replies[c.Reply.Record.Line] = Event{CallID: c.ID, Step: e.Step, Sequence: request.Sequence, SourceSequence: c.Sequence,
					ParallelSize: e.ParallelSize, SourceReplyStatus: c.Reply.Status, SourceReplyError: c.Reply.IsError}
			}
			r.Events = append(r.Events, request)
		}
	}
	if !playback {
		if err := validateParallelGroups(r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// The pinned OpenClaw exec tool uses these defaults when the source omitted both fields.
const openClawDefaultYieldMS = 10000
const openClawDefaultTimeoutSeconds = 1800

func wildClawLiveCall(c WildClawSourceCall, sessionCWD string) (replay.Call, error) {
	call := replay.Call{ToolCallID: c.ID, Sequence: c.Sequence}
	if c.Reply == nil || c.Reply.IsError == nil || *c.Reply.IsError {
		return call, errors.New("live projection requires a completed native tool reply")
	}
	switch c.Name {
	case "read":
		var args struct {
			Path     string
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal(c.Arguments, &args); err != nil {
			return call, err
		}
		if args.Path == "" {
			args.Path = args.FilePath
		}
		var content []struct{ Type, Text, Data, MimeType string }
		if err := json.Unmarshal(c.Reply.Content, &content); err != nil {
			return call, err
		}
		if len(content) == 2 && content[0].Text == "Read image file [image/png]" && content[1].Type == "image" && content[1].MimeType == "image/png" && args.Path != "" {
			raw, err := base64.StdEncoding.DecodeString(content[1].Data)
			if err != nil || !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
				return call, errors.New("invalid PNG attachment")
			}
			call.Operation, call.Path, call.ResponseCode, call.ResponseStdout = "ReadFile", args.Path, "image/png", []byte(content[1].Data)
			break
		}
		if args.Path == "" || len(content) != 1 || content[0].Type != "text" {
			return call, errors.New("expected a successful whole-file text read")
		}
		call.Operation, call.Path = "ReadFile", args.Path
		call.ResponseCode, call.ResponseStdout = "success", []byte(content[0].Text)
	case "exec":
		var args struct {
			Command, Workdir string
			YieldMS          int `json:"yieldMs"`
			Timeout          int
		}
		args.YieldMS, args.Timeout = openClawDefaultYieldMS, openClawDefaultTimeoutSeconds
		if err := json.Unmarshal(c.Arguments, &args); err != nil {
			return call, err
		}
		var details struct {
			Status, Aggregated, CWD, Error string
			ExitCode                       *int32 `json:"exitCode"`
		}
		if err := json.Unmarshal(c.Reply.Details, &details); err != nil {
			return call, err
		}
		if args.Workdir == "" && details.CWD == sessionCWD {
			args.Workdir = details.CWD
		}
		if args.Workdir == "" && details.Status == "error" && details.CWD == "" {
			args.Workdir = sessionCWD
		}
		if details.Status == "error" && details.ExitCode == nil && strings.HasSuffix(details.Error, "\n\nCommand not found") && args.Command != "" && args.Workdir != "" {
			call.Operation, call.Script, call.Workdir = "Exec", args.Command, args.Workdir
			call.ResponseCode, call.ExitCode = "command_not_found", 127
			call.ResponseStdout = []byte(strings.TrimSuffix(details.Error, "\n\nCommand not found"))
			call.YieldTime, call.Timeout = time.Duration(args.YieldMS)*time.Millisecond, time.Duration(args.Timeout)*time.Second
			break
		}
		if args.Command == "" || args.Workdir == "" || details.Status != "completed" || details.ExitCode == nil || details.CWD != args.Workdir {
			return call, errors.New("expected a completed foreground shell with its recorded working directory")
		}
		call.Operation, call.Script, call.Workdir = "Exec", args.Command, args.Workdir
		call.YieldTime, call.Timeout = time.Duration(args.YieldMS)*time.Millisecond, time.Duration(args.Timeout)*time.Second
		call.ExitCode, call.ResponseCode = *details.ExitCode, details.Status
		output := details.Aggregated
		if call.ExitCode != 0 {
			output = strings.TrimSuffix(output, fmt.Sprintf("\n\n(Command exited with code %d)", call.ExitCode))
		}
		call.ResponseStdout = []byte(output)
	default:
		return call, fmt.Errorf("unsupported live OpenClaw tool %q", c.Name)
	}
	call.ResponseSHA256, call.ResponseBytes = digest(call.ResponseStdout), int64(len(call.ResponseStdout))
	return call, nil
}
