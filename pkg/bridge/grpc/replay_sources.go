package grpc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/sirupsen/logrus"
)

// ReplayCallStart keeps timing and identity without a recorded request payload.
type ReplayCallStart struct {
	Sequence    uint64
	StartOffset time.Duration
}

// ReplayOccurrence describes one selected source on the shared source timeline.
type ReplayOccurrence struct {
	SourceTaskID  string
	LogicalTaskID string
	StartedAt     time.Time
	Calls         []ReplayCallStart
}

// ReplaySelection retains compact source evidence, not request payload plans.
// StartedAt is the earliest recorded call start, not a replay scheduling epoch.
type ReplaySelection struct {
	Occurrences  []ReplayOccurrence
	StartedAt    time.Time
	sources      []replaySelectedSource
	benchmark    string
	revisions    map[string]string
	maxPlanBytes int
}

type replaySelectedSource struct {
	dir     string
	task    core.Task
	offset  time.Duration
	binding replaySourceBinding
}

type replaySourceBinding struct {
	runtime   [sha256.Size]byte
	calls     [sha256.Size]byte
	result    [sha256.Size]byte
	planBytes int
}

type replayRunIndex struct {
	RunID string `json:"run_id"`
	Tasks []struct {
		TaskID     string          `json:"task_id"`
		Evaluation json.RawMessage `json:"evaluation"`
	} `json:"tasks"`
	digest [sha256.Size]byte
}

func readReplayRunIndex(ctx context.Context, root *os.Root) (*replayRunIndex, error) {
	data, err := readReplayFile(ctx, root, "run-result.json", MaxMessageBytes)
	if err != nil {
		return nil, fmt.Errorf("read replay run result: %w", err)
	}
	var result replayRunIndex
	if err := json.Unmarshal(data, &result); err != nil || result.RunID == "" {
		return nil, errors.New("replay source run result is invalid")
	}
	result.digest = sha256.Sum256(data)
	return &result, nil
}

// PreflightReplaySource selects one occurrence or every parent-indexed occurrence.
// It validates the complete selection without starting a sandbox or a verifier.
func PreflightReplaySource(ctx context.Context, sourceDir string, tasks []core.Task, benchmarkType string, revisions map[string]string, logger *logrus.Entry) (*ReplaySelection, error) {
	return preflightReplaySource(ctx, sourceDir, tasks, benchmarkType, revisions, logger, replay.MaxPlanBytes)
}

func preflightReplaySource(ctx context.Context, sourceDir string, tasks []core.Task, benchmarkType string, revisions map[string]string, logger *logrus.Entry, maxPlanBytes int) (*ReplaySelection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sourceDir == "" {
		return nil, errors.New("replay source is missing")
	}
	sourceDir, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, err
	}
	runDir := sourceDir
	var selectedID string
	if _, err := os.Lstat(filepath.Join(sourceDir, "bridge")); err == nil {
		selectedID, runDir = filepath.Base(sourceDir), filepath.Dir(sourceDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	info, err := os.Lstat(runDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != replayDirectoryMode {
		return nil, errors.New("replay source run directory is not private")
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	result, err := readReplayRunIndex(ctx, root)
	if err != nil {
		return nil, err
	}
	selection := &ReplaySelection{benchmark: benchmarkType, revisions: maps.Clone(revisions), maxPlanBytes: maxPlanBytes}
	seen := make(map[string]bool, len(result.Tasks))
	hasCalls := false
	for _, candidate := range result.Tasks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if selectedID != "" && candidate.TaskID != selectedID {
			continue
		}
		id := candidate.TaskID
		if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, "/\\\x00") || seen[id] {
			return nil, errors.New("replay source occurrence binding is invalid or duplicated")
		}
		seen[id] = true
		dir := filepath.Join(runDir, id)
		source, err := root.OpenRoot(id)
		if err != nil {
			return nil, fmt.Errorf("open replay occurrence: %w", err)
		}
		data, readErr := readReplayFile(ctx, source, "bridge/runtime.json", MaxMessageBytes)
		closeErr := source.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		var metadata replaySourceMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, fmt.Errorf("decode replay runtime: %w", err)
		}
		var task core.Task
		for _, candidate := range tasks {
			if candidate.ID == metadata.TaskID {
				if task.ID != "" {
					return nil, errors.New("replay catalog logical task is duplicated")
				}
				task = core.Task{ID: candidate.ID, Environment: core.Environment{Image: candidate.Environment.Image, Workdir: candidate.Environment.Workdir}}
			}
		}
		if task.ID == "" {
			return nil, errors.New("replay source has no matching logical task")
		}
		selected := replaySelectedSource{dir: dir, task: task}
		plan, err := loadReplaySourceContext(ctx, dir, task, benchmarkType, selection.revisions, selection.maxPlanBytes, result, &selected.binding)
		if err != nil {
			return nil, err
		}
		occurrence := ReplayOccurrence{SourceTaskID: plan.SourceTaskID, LogicalTaskID: task.ID, StartedAt: plan.StartedAt}
		for _, call := range plan.Calls {
			occurrence.Calls = append(occurrence.Calls, ReplayCallStart{Sequence: call.Sequence, StartOffset: call.StartOffset})
		}
		if len(plan.Calls) != 0 {
			if !hasCalls || plan.StartedAt.Before(selection.StartedAt) {
				selection.StartedAt = plan.StartedAt
			}
			hasCalls = true
		}
		selection.Occurrences = append(selection.Occurrences, occurrence)
		selection.sources = append(selection.sources, selected)
	}
	if len(selection.Occurrences) == 0 {
		return nil, errors.New("replay source has no selected occurrences")
	}
	starts := make(map[time.Duration]struct {
		taskID   string
		sequence uint64
	})
	for index := range selection.Occurrences {
		occurrence := &selection.Occurrences[index]
		planBytes := selection.sources[index].binding.planBytes
		if len(selection.Occurrences) > 1 && len(occurrence.Calls) != 0 {
			// Reserve the longest UTC origin before any task sandbox can start.
			const replayOriginBytes = len(`,"replay_at":"2006-01-02T15:04:05.999999999Z"`)
			planBytes += replayOriginBytes
		}
		if planBytes > selection.maxPlanBytes {
			return nil, errors.New("replay global plan exceeds the encoded byte bound")
		}
		if len(occurrence.Calls) != 0 {
			selection.sources[index].offset = occurrence.StartedAt.Sub(selection.StartedAt)
		}
		for callIndex := range occurrence.Calls {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			call := &occurrence.Calls[callIndex]
			started := occurrence.StartedAt.Add(call.StartOffset)
			offset := started.Sub(selection.StartedAt)
			if offset < 0 || !selection.StartedAt.Add(offset).Equal(started) {
				return nil, errors.New("replay global call offset is not representable")
			}
			// Only the encoded offset number changes when the source timeline expands.
			planBytes += len(fmt.Sprint(offset.Nanoseconds())) - len(fmt.Sprint(call.StartOffset.Nanoseconds()))
			if planBytes > selection.maxPlanBytes {
				return nil, errors.New("replay global plan exceeds the encoded byte bound")
			}
			call.StartOffset = offset
			if previous, exists := starts[offset]; exists {
				if logger != nil && previous.taskID != occurrence.SourceTaskID {
					logger.WithFields(logrus.Fields{
						"source_run_id": result.RunID, "source_task_id": previous.taskID, "source_sequence": previous.sequence,
						"tied_source_task_id": occurrence.SourceTaskID, "tied_source_sequence": call.Sequence,
						"started_at": started.Format(time.RFC3339Nano), "start_offset_ns": offset,
					}).Warn("replay calls have equal start timestamps")
				}
			} else {
				starts[offset] = struct {
					taskID   string
					sequence uint64
				}{occurrence.SourceTaskID, call.Sequence}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return selection, nil
}

// Load reloads one bounded plan and rejects changes to finalized source evidence.
// Call offsets and StartedAt use the shared source timeline; empty plans have no source start.
func (selection *ReplaySelection) Load(ctx context.Context, index int) (replay.Plan, error) {
	if selection == nil || index < 0 || index >= len(selection.sources) {
		return replay.Plan{}, errors.New("replay occurrence index is invalid")
	}
	source := selection.sources[index]
	var binding replaySourceBinding
	plan, err := loadReplaySourceContext(ctx, source.dir, source.task, selection.benchmark, selection.revisions, selection.maxPlanBytes, nil, &binding)
	if err != nil {
		return replay.Plan{}, err
	}
	if binding != source.binding {
		return replay.Plan{}, errors.New("replay source evidence changed after preflight")
	}
	if len(plan.Calls) != 0 {
		plan.StartedAt = plan.StartedAt.Add(-source.offset)
	}
	for callIndex := range plan.Calls {
		plan.Calls[callIndex].StartOffset += source.offset
	}
	if _, err := replay.EncodePlan(plan, selection.maxPlanBytes); err != nil {
		return replay.Plan{}, err
	}
	if err := ctx.Err(); err != nil {
		return replay.Plan{}, err
	}
	return plan, nil
}
