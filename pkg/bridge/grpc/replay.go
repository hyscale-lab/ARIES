package grpc

import (
	"bytes"
	"context"
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
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
)

// The bound matches the finalized audit limit. Metadata and run results use
// the existing gRPC message bound; none of these files may grow without limit.
const maxReplayLogBytes int64 = 256 << 20

const replayFileMode os.FileMode = 0o600
const replayDirectoryMode os.FileMode = 0o700

type replaySourceMetadata struct {
	RunID              string            `json:"run_id"`
	SchemaVersion      int               `json:"schema_version"`
	ReplayReady        bool              `json:"replay_ready"`
	SandboxID          string            `json:"SandboxID"`
	BenchmarkType      string            `json:"benchmark_type"`
	TaskID             string            `json:"task_id"`
	OccurrenceID       json.RawMessage   `json:"occurrence_id"`
	Image              string            `json:"image"`
	BenchmarkRevisions map[string]string `json:"benchmark_revisions"`
	HarnessCompletedAt string            `json:"harness_completed_at"`
	ToolCallsBytes     *int64            `json:"tool_calls_bytes"`
	ToolCallsSHA256    string            `json:"tool_calls_sha256"`
}

// LoadReplaySource validates all private source evidence before returning a plan.
// It makes no sandbox call and does not run a benchmark verifier.
func LoadReplaySource(sourceDir string, task core.Task, benchmarkType string, revisions map[string]string) (replay.Plan, error) {
	return loadReplaySource(sourceDir, task, benchmarkType, revisions, replay.MaxPlanBytes)
}

func loadReplaySource(sourceDir string, task core.Task, benchmarkType string, revisions map[string]string, maxPlanBytes int) (replay.Plan, error) {
	return loadReplaySourceContext(context.Background(), sourceDir, task, benchmarkType, revisions, maxPlanBytes, nil, nil, nil, false)
}

func loadReplaySourceContext(ctx context.Context, sourceDir string, task core.Task, benchmarkType string, revisions map[string]string, maxPlanBytes int, result *replayRunIndex, binding *replaySourceBinding, logger *logrus.Entry, skipResponseCheck bool) (replay.Plan, error) {
	plan := replay.Plan{SkipResponseCheck: skipResponseCheck}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
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

	runtimeData, err := readReplayFile(ctx, bridge, "runtime.json", MaxMessageBytes)
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
	if benchmarkType == "" || task.ID == "" || task.Environment.Image == "" || len(revisions) == 0 || metadata.BenchmarkType != benchmarkType || metadata.TaskID != task.ID {
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
	log, err := readReplayFile(ctx, bridge, "tool-calls.jsonl", maxReplayLogBytes)
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

	if result == nil {
		result, err = readReplayRunIndex(ctx, root)
		if err != nil {
			return plan, err
		}
	}
	if metadata.RunID != "" && metadata.RunID != result.RunID || metadata.RunID == "" && len(log) == 0 {
		return plan, errors.New("replay source run binding is missing or differs from parent result")
	}
	rawEvaluation, err := result.occurrenceEvaluation(occurrence)
	if err != nil {
		return replay.Plan{}, err
	}
	plan.Evaluation, err = parseReplayEvaluation(rawEvaluation)
	if err != nil {
		return replay.Plan{}, err
	}
	plan.SourceTaskID = occurrence
	if metadata.OccurrenceID != nil {
		var recordedID string
		if err := json.Unmarshal(metadata.OccurrenceID, &recordedID); err != nil || recordedID != plan.SourceTaskID {
			return replay.Plan{}, errors.New("replay source occurrence binding differs from parent result")
		}
	} else if len(log) == 0 {
		// Older traces must prove the binding with their call records.
		return replay.Plan{}, errors.New("replay source occurrence binding is missing")
	}
	plan.SourceRunID = result.RunID

	var first, latest time.Time
	starts := make(map[uint64]time.Time)
	for line := range bytes.Lines(log) {
		if err := ctx.Err(); err != nil {
			return replay.Plan{}, err
		}
		line = line[:len(line)-1]
		number := len(plan.Calls) + 1
		if len(line) == 0 || len(line) > 2*MaxMessageBytes {
			return replay.Plan{}, fmt.Errorf("replay call %d has invalid length", number)
		}
		var record toolCallRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return replay.Plan{}, fmt.Errorf("decode replay call %d: %w", number, err)
		}
		call, started, stamped, err := parseReplayCall(record, line, uint64(number), plan.SourceRunID, plan.SourceTaskID, metadata.SandboxID, skipResponseCheck)
		if err != nil {
			return replay.Plan{}, fmt.Errorf("replay call %d: %w", number, err)
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
	if task.Environment.Workdir != "" {
		if err := plan.ValidateWorkdir(task.Environment.Workdir); err != nil {
			return replay.Plan{}, err
		}
	}
	if len(plan.Calls) != 0 {
		plan.StartedAt = first
		// File order follows call completion, not admission. The start time is the
		// schedule's origin even when a later file line started first.
		slices.SortFunc(plan.Calls, func(left, right replay.Call) int {
			return starts[left.Sequence].Compare(starts[right.Sequence])
		})
		for index := range plan.Calls {
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
	}
	encoded, err := replay.EncodePlan(plan, maxPlanBytes)
	if err != nil {
		return replay.Plan{}, fmt.Errorf("encode replay source plan: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return replay.Plan{}, err
	}
	if binding != nil {
		*binding = replaySourceBinding{runtime: sha256.Sum256(runtimeData), calls: digest, result: result.digest, planBytes: len(encoded)}
	}
	if logger != nil {
		entry := logger.WithContext(ctx).WithFields(logrus.Fields{
			"source_run_id": plan.SourceRunID, "source_task_id": plan.SourceTaskID, "logical_task_id": task.ID,
		})
		logReplayAttribute(entry, "image", metadata.Image, task.Environment.Image)
		keys := slices.AppendSeq(slices.Collect(maps.Keys(revisions)), maps.Keys(metadata.BenchmarkRevisions))
		slices.Sort(keys)
		for _, name := range slices.Compact(keys) {
			logReplayAttribute(entry, "benchmark_revisions."+name, metadata.BenchmarkRevisions[name], revisions[name])
		}
	}
	return plan, nil
}

func logReplayAttribute(logger *logrus.Entry, attribute, source, target string) {
	if source == target {
		return
	}
	comparison := "different"
	if source == "" {
		comparison = "unverified"
	}
	logger.WithFields(logrus.Fields{
		"attribute": attribute, "comparison": comparison,
		"source_value": source, "target_value": target,
	}).Warn("replay starting environment check")
}

func parseReplayEvaluation(data json.RawMessage) (core.Evaluation, error) {
	unavailable := core.Evaluation{Status: core.StatusUnavailable}
	if data == nil {
		return unavailable, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return core.Evaluation{}, errors.New("replay source evaluation is invalid")
	}
	if len(fields) == 0 {
		return unavailable, nil
	}
	var evaluation core.Evaluation
	if err := json.Unmarshal(data, &evaluation); err != nil {
		return core.Evaluation{}, errors.New("replay source evaluation is invalid")
	}
	switch evaluation.Status {
	case core.StatusSucceeded, core.StatusFailed, core.StatusCanceled:
		if fields["score"] == nil && fields["reward"] == nil {
			return unavailable, nil
		}
		return evaluation, nil
	case core.StatusNotRun, core.StatusBlockedIsolation, core.StatusNotEnabled:
		// Native live results write zero placeholders when evaluation does not run.
		if evaluation.Score == 0 && evaluation.Reward == 0 {
			return unavailable, nil
		}
	}
	return core.Evaluation{}, errors.New("replay source evaluation status or grade is invalid")
}

func readReplayFile(ctx context.Context, root *os.Root, name string, bound int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	const readChunkBytes = 64 << 10
	content := make([]byte, info.Size()+1)
	read := 0
	for read < len(content) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(read+readChunkBytes, len(content))
		count, err := file.Read(content[read:end])
		read += count
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	if int64(read) != info.Size() {
		return nil, errors.New("replay evidence changed while reading")
	}
	return content[:read], ctx.Err()
}

func validReplayNumber(value json.RawMessage) bool {
	return len(value) > 0 && (value[0] == '-' || value[0] >= '0' && value[0] <= '9')
}

func validReplayDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// Inspect raw keys because the typed decoder folds case and merges duplicates.
func replayCallFields(line []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("expected a replay call object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("invalid replay call field name")
		}
		for _, field := range [...]string{"response_bytes", "response_sha256", "grpc_code", "exit_code"} {
			if strings.EqualFold(name, field) && (name != field || fields[field] != nil) {
				return nil, fmt.Errorf("duplicate or noncanonical replay response field %q", field)
			}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	return fields, nil
}

func parseReplayCall(record toolCallRecord, line []byte, sequence uint64, runID, taskID, sandboxID string, skipResponseCheck bool) (replay.Call, time.Time, time.Time, error) {
	var call replay.Call
	if record.Sequence != sequence || record.RunID != runID || record.TaskID != taskID || record.SandboxID != sandboxID || record.Status != "completed" && record.Status != "failed" || record.ReplayError != "" || record.DurationMS < 0 {
		return call, time.Time{}, time.Time{}, errors.New("replay call has incomplete or rejected evidence")
	}
	fields, err := replayCallFields(line)
	if err != nil {
		return call, time.Time{}, time.Time{}, err
	}
	if (!skipResponseCheck || fields["response_sha256"] != nil) && !validReplayDigest(record.ResponseSHA256) ||
		(!skipResponseCheck || fields["response_bytes"] != nil) && record.ResponseBytes <= 0 {
		return call, time.Time{}, time.Time{}, errors.New("replay call has incomplete or rejected evidence")
	}
	validCode := false
	for code := codes.OK; code <= codes.Unauthenticated; code++ {
		if record.GRPCCode == code.String() {
			validCode = true
			break
		}
	}
	if (!skipResponseCheck || fields["grpc_code"] != nil) &&
		(!validCode || record.Status == "completed" && record.GRPCCode != codes.OK.String() || record.Status == "failed" && record.OperationClass != kindExec && record.GRPCCode == codes.OK.String()) {
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
	switch record.OperationClass {
	case kindExec:
		if !skipResponseCheck || fields["exit_code"] != nil {
			if !validReplayNumber(fields["exit_code"]) || record.ExitCode < 0 || record.ExitCode > int(^uint8(0)) {
				return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay Exec exit code is invalid")
			}
			call.ExitCode = new(int32(record.ExitCode))
		}
		command := fields["command"]
		if len(command) > 0 && command[0] != '"' || len(fields["stdin"]) == 0 || fields["stdin"][0] != '"' || !validReplayNumber(fields["stdin_bytes"]) || record.CommandHash != bridgekit.CommandHash(record.Command) || record.StdinEncoding != "utf-8" || int64(len(record.Stdin)) != record.StdinBytes || !bridgekit.ValidWorkdir(record.Workdir) {
			return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay Exec input is incomplete")
		}
		login := slices.Equal(record.Argv, []string{bridgekit.RemoteShellPath, "-l", "-c", record.Command})
		if record.Path != bridgekit.RemoteShellPath || !login && !slices.Equal(record.Argv, []string{bridgekit.RemoteShellPath, "-c", record.Command}) {
			return replay.Call{}, time.Time{}, time.Time{}, errors.New("replay Exec argv differs from the recorded command")
		}
		call.Operation, call.Script, call.Login, call.Body = "Exec", record.Command, login, []byte(record.Stdin)
		call.Workdir = record.Workdir
		if record.ResponseDebug {
			call.ResponseDebug = true
			call.ResponseStdout, call.ResponseStderr = []byte(record.ResponseStdout), []byte(record.ResponseStderr)
			if record.ResponseStdoutBase64 != nil {
				call.ResponseStdout = record.ResponseStdoutBase64
			}
			if record.ResponseStderrBase64 != nil {
				call.ResponseStderr = record.ResponseStderrBase64
			}
		}
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
