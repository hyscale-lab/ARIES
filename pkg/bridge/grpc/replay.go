package grpc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"google.golang.org/grpc/codes"
)

// The bound matches the finalized audit limit. Metadata and run results use
// the existing gRPC message bound; none of these files may grow without limit.
const maxReplayLogBytes int64 = 256 << 20

const replayFileMode os.FileMode = 0o600
const replayDirectoryMode os.FileMode = 0o700

type replaySourceMetadata struct {
	SchemaVersion      int               `json:"schema_version"`
	ReplayReady        bool              `json:"replay_ready"`
	SandboxID          string            `json:"SandboxID"`
	BenchmarkType      string            `json:"benchmark_type"`
	TaskID             string            `json:"task_id"`
	Image              string            `json:"image"`
	BenchmarkRevisions map[string]string `json:"benchmark_revisions"`
	HarnessCompletedAt string            `json:"harness_completed_at"`
	ToolCallsBytes     *int64            `json:"tool_calls_bytes"`
	ToolCallsSHA256    string            `json:"tool_calls_sha256"`
}

// LoadReplaySource validates all private source evidence before returning a plan.
// It makes no sandbox call and does not run a benchmark verifier.
func LoadReplaySource(sourceDir string, task core.Task, benchmarkType string, revisions map[string]string) (replay.Plan, error) {
	var plan replay.Plan
	parent := filepath.Dir(filepath.Clean(sourceDir))
	occurrence := filepath.Base(filepath.Clean(sourceDir))
	if occurrence == "." || occurrence == string(filepath.Separator) || sourceDir == "" {
		return plan, errors.New("replay source must name a task occurrence")
	}
	for _, dir := range []string{parent, sourceDir, filepath.Join(sourceDir, "bridge")} {
		info, err := os.Lstat(dir)
		if err != nil {
			return plan, fmt.Errorf("inspect replay source directory: %w", err)
		}
		if !info.IsDir() || info.Mode().Perm() != replayDirectoryMode {
			return plan, fmt.Errorf("replay source directory %q is not private: %v", dir, info.Mode())
		}
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return plan, err
	}
	defer root.Close()
	source, err := root.OpenRoot(occurrence)
	if err != nil {
		return plan, err
	}
	defer source.Close()
	bridge, err := source.OpenRoot("bridge")
	if err != nil {
		return plan, err
	}
	defer bridge.Close()

	runtimeData, err := readReplayFile(bridge, "runtime.json", MaxMessageBytes)
	if err != nil {
		return plan, fmt.Errorf("read replay runtime: %w", err)
	}
	var metadata replaySourceMetadata
	if err := json.Unmarshal(runtimeData, &metadata); err != nil {
		return plan, fmt.Errorf("decode replay runtime: %w", err)
	}
	if metadata.SchemaVersion != 1 || !metadata.ReplayReady || metadata.SandboxID == "" || metadata.HarnessCompletedAt == "" || metadata.ToolCallsBytes == nil || *metadata.ToolCallsBytes < 0 || *metadata.ToolCallsBytes > maxReplayLogBytes || !validReplayDigest(metadata.ToolCallsSHA256) {
		return plan, errors.New("replay runtime is not finalized")
	}
	if benchmarkType == "" || task.ID == "" || task.Environment.Image == "" || len(revisions) == 0 || metadata.BenchmarkType != benchmarkType || metadata.TaskID != task.ID || metadata.Image != task.Environment.Image || !maps.Equal(metadata.BenchmarkRevisions, revisions) {
		return plan, errors.New("replay source task or benchmark does not match")
	}
	for name, revision := range revisions {
		if name == "" || revision == "" {
			return plan, errors.New("replay benchmark revision is missing")
		}
	}
	completed, err := time.Parse(time.RFC3339Nano, metadata.HarnessCompletedAt)
	if err != nil {
		return plan, fmt.Errorf("invalid replay completion marker: %w", err)
	}
	log, err := readReplayFile(bridge, "tool-calls.jsonl", maxReplayLogBytes)
	if err != nil {
		return plan, fmt.Errorf("read replay calls: %w", err)
	}
	digest := sha256.Sum256(log)
	if int64(len(log)) != *metadata.ToolCallsBytes || hex.EncodeToString(digest[:]) != metadata.ToolCallsSHA256 {
		return plan, errors.New("replay call log differs from finalized evidence")
	}
	if len(log) != 0 && log[len(log)-1] != '\n' {
		return plan, errors.New("replay call log ends with an incomplete line")
	}

	resultData, err := readReplayFile(root, "run-result.json", MaxMessageBytes)
	if err != nil {
		return plan, fmt.Errorf("read replay run result: %w", err)
	}
	var result core.RunResult
	if err := json.Unmarshal(resultData, &result); err != nil || result.RunID == "" {
		return plan, errors.New("replay source run result is invalid")
	}
	var rawResult struct {
		Tasks []struct {
			Evaluation map[string]json.RawMessage `json:"evaluation"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(resultData, &rawResult); err != nil || len(rawResult.Tasks) != len(result.Tasks) {
		return plan, errors.New("replay source evaluations are invalid")
	}
	for index, candidate := range result.Tasks {
		if candidate.TaskID != occurrence {
			continue
		}
		grades := rawResult.Tasks[index].Evaluation
		if plan.SourceTaskID != "" || candidate.Evaluation.Status != core.StatusSucceeded && candidate.Evaluation.Status != core.StatusFailed && candidate.Evaluation.Status != core.StatusCanceled || !validReplayNumber(grades["score"]) || !validReplayNumber(grades["reward"]) {
			return replay.Plan{}, errors.New("replay source has no unique recorded evaluation")
		}
		plan.SourceTaskID = candidate.TaskID
		plan.Evaluation = candidate.Evaluation
	}
	if plan.SourceTaskID == "" {
		return replay.Plan{}, errors.New("replay source has no matching evaluation")
	}
	plan.SourceRunID = result.RunID

	var first, latest time.Time
	lines := bytes.Split(log, []byte{'\n'})
	starts := make(map[uint64]time.Time, len(lines)-1)
	for index, line := range lines[:len(lines)-1] {
		if len(line) == 0 || len(line) > 2*MaxMessageBytes {
			return replay.Plan{}, fmt.Errorf("replay call %d has invalid length", index+1)
		}
		var record toolCallRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return replay.Plan{}, fmt.Errorf("decode replay call %d: %w", index+1, err)
		}
		call, started, stamped, err := parseReplayCall(record, line, uint64(index+1), plan.SourceRunID, plan.SourceTaskID, metadata.SandboxID)
		if err != nil {
			return replay.Plan{}, fmt.Errorf("replay call %d: %w", index+1, err)
		}
		starts[record.Sequence] = started
		if len(plan.Calls) == 0 || started.Before(first) {
			first = started
		}
		if stamped.After(latest) {
			latest = stamped
		}
		plan.Calls = append(plan.Calls, call)
	}
	if len(plan.Calls) == 0 {
		return plan, nil
	}
	// File order follows call completion, not admission. The start time is the
	// schedule's origin even when a later file line started first.
	slices.SortFunc(plan.Calls, func(left, right replay.Call) int {
		return starts[left.Sequence].Compare(starts[right.Sequence])
	})
	for index := range plan.Calls {
		if index > 0 && starts[plan.Calls[index-1].Sequence].Equal(starts[plan.Calls[index].Sequence]) {
			return replay.Plan{}, errors.New("replay call starts are ambiguous")
		}
		start := starts[plan.Calls[index].Sequence]
		offset := start.Sub(first)
		if offset < 0 || !first.Add(offset).Equal(start) {
			return replay.Plan{}, errors.New("replay call start offset is not representable")
		}
		plan.Calls[index].StartOffset = offset
	}
	plan.FinalWait = completed.Sub(latest)
	if plan.FinalWait < 0 || !latest.Add(plan.FinalWait).Equal(completed) {
		return replay.Plan{}, errors.New("replay completion wait is invalid")
	}
	return plan, nil
}

func readReplayFile(root *os.Root, name string, bound int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != replayFileMode || info.Size() < 0 || info.Size() > bound {
		return nil, errors.New("replay evidence is not a bounded private regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || opened.Mode().Perm() != replayFileMode || opened.Size() != info.Size() {
		return nil, errors.New("replay evidence changed before reading")
	}
	content, err := io.ReadAll(io.LimitReader(file, bound+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != info.Size() {
		return nil, errors.New("replay evidence changed while reading")
	}
	return content, nil
}

func validReplayNumber(value json.RawMessage) bool {
	return len(value) > 0 && (value[0] == '-' || value[0] >= '0' && value[0] <= '9')
}

func validReplayDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func parseReplayCall(record toolCallRecord, line []byte, sequence uint64, runID, taskID, sandboxID string) (replay.Call, time.Time, time.Time, error) {
	var call replay.Call
	if record.Sequence != sequence || record.RunID != runID || record.TaskID != taskID || record.SandboxID != sandboxID || record.Status != "completed" && record.Status != "failed" || record.ReplayError != "" || !validReplayDigest(record.ResponseSHA256) || record.ResponseBytes <= 0 || record.DurationMS < 0 {
		return call, time.Time{}, time.Time{}, errors.New("replay call has incomplete or rejected evidence")
	}
	validCode := false
	for code := codes.OK; code <= codes.Unauthenticated; code++ {
		if record.GRPCCode == code.String() {
			validCode = true
			break
		}
	}
	if !validCode || record.Status == "completed" && record.GRPCCode != codes.OK.String() || record.Status == "failed" && record.OperationClass != kindExec && record.GRPCCode == codes.OK.String() {
		return call, time.Time{}, time.Time{}, errors.New("replay call has invalid gRPC status")
	}
	started, startErr := time.Parse(time.RFC3339Nano, record.StartedAt)
	stamped, stampErr := time.Parse(time.RFC3339Nano, record.Timestamp)
	if startErr != nil || stampErr != nil || stamped.Before(started) {
		return call, time.Time{}, time.Time{}, errors.New("replay call has invalid timestamps")
	}
	if len(record.ToolCallID) > maxToolCallIDBytes || strings.Trim(record.ToolCallID, toolCallIDChars) != "" {
		return call, time.Time{}, time.Time{}, errors.New("replay call has invalid tool call ID")
	}
	call = replay.Call{Sequence: sequence, ToolCallID: record.ToolCallID, ResponseSHA256: record.ResponseSHA256, ResponseBytes: record.ResponseBytes, ResponseCode: record.GRPCCode}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return replay.Call{}, time.Time{}, time.Time{}, err
	}
	switch record.OperationClass {
	case kindExec:
		command := fields["command"]
		if len(command) > 0 && command[0] != '"' || len(fields["stdin"]) == 0 || fields["stdin"][0] != '"' || !validReplayNumber(fields["stdin_bytes"]) || record.CommandHash != bridgekit.CommandHash(record.Command) || record.StdinEncoding != "utf-8" || int64(len(record.Stdin)) != record.StdinBytes || !bridgekit.ValidWorkdir(record.Workdir) {
			return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay Exec input is incomplete")
		}
		login := slices.Equal(record.Argv, []string{bridgekit.RemoteShellPath, "-l", "-c", record.Command})
		if record.Path != bridgekit.RemoteShellPath || !login && !slices.Equal(record.Argv, []string{bridgekit.RemoteShellPath, "-c", record.Command}) {
			return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay Exec argv differs from the recorded command")
		}
		call.Operation, call.Script, call.Login, call.Body = "Exec", record.Command, login, []byte(record.Stdin)
	case kindFileStat, kindFileRead, kindFileWrite:
		if !strings.HasPrefix(record.Path, "/") || strings.ContainsRune(record.Path, 0) {
			return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay file path is invalid")
		}
		call.Path = record.Path
		switch record.OperationClass {
		case kindFileStat:
			call.Operation = "Stat"
		case kindFileRead:
			if record.FirstLine == 0 && record.MaxLines == 0 {
				if record.MaxBytes < 0 {
					return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay ReadFile limit is invalid")
				}
				call.Operation, call.MaxBytes = "ReadFile", record.MaxBytes
			} else {
				if record.FirstLine <= 0 || record.MaxLines <= 0 || record.MaxLineBytes < 0 {
					return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay ReadLines window is invalid")
				}
				call.Operation, call.FirstLine, call.MaxLines, call.MaxLineBytes = "ReadLines", record.FirstLine, record.MaxLines, record.MaxLineBytes
			}
		case kindFileWrite:
			if len(fields["stdin"]) == 0 || fields["stdin"][0] != '"' || !validReplayNumber(fields["stdin_bytes"]) || record.StdinEncoding != "utf-8" || record.StdinBytes < 0 || int64(len(record.Stdin)) != record.StdinBytes || record.Status != "completed" {
				return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay WriteFile input is incomplete")
			}
			bodyDigest := sha256.Sum256([]byte(record.Stdin))
			if hex.EncodeToString(bodyDigest[:]) != record.SHA256 {
				return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay WriteFile body differs from recorded digest")
			}
			call.Operation, call.Body = "WriteFile", []byte(record.Stdin)
		}
	default:
		return replay.Call{}, time.Time{}, time.Time{}, errors.New("unknown replay operation")
	}
	return call, started, stamped, nil
}
