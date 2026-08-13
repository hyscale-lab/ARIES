// Package swebenchpro adapts the pinned public SWE-bench Pro release to the
// benchmark-neutral ARIES Runner lifecycle.
package swebenchpro

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	DefaultRoot = ".cache/swe-bench-pro"

	repositoryPath         = "/app"
	privateContainerPath   = "/tmp/aries-swebenchpro"
	agentExecUser          = "65532:65532"
	agentUID               = "65532"
	rootExecUser           = "0:0"
	runScriptsDirectory    = "run_scripts"
	runScriptName          = "run_script.sh"
	parserFileName         = "parser.py"
	defaultAgentTimeout    = time.Hour
	defaultVerifierTimeout = time.Hour
)

type Options struct {
	Root              string
	TaskIDs           []string
	ExecutionTaskIDs  []string
	OutputDir         string
	DatasetRevision   string
	EvaluatorRevision string
}

type Benchmark struct {
	root              string
	taskIDs           []string
	executionTaskIDs  []string
	outputDir         string
	datasetRevision   string
	evaluatorRevision string

	mu      sync.RWMutex
	details map[string]taskDetails
}

type taskDetails struct {
	baseCommit      string
	goldCommit      string
	testPatch       string
	failToPass      []string
	passToPass      []string
	selectedTests   []string
	verifierFiles   []string
	runScript       string
	parser          string
	snapshot        string
	ignoredSnapshot string
	gitSnapshot     string
}

var _ runner.Benchmark = (*Benchmark)(nil)

func New(options Options) (*Benchmark, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("SWE-bench Pro root is required")
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("SWE-bench Pro task IDs are required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("SWE-bench Pro output directory is required")
	}
	if !isHex(options.DatasetRevision, 40) || !isHex(options.EvaluatorRevision, 40) {
		return nil, errors.New("SWE-bench Pro dataset and evaluator revisions must be 40-character Git revisions")
	}
	seen := make(map[string]struct{}, len(options.TaskIDs))
	for _, id := range options.TaskIDs {
		if !safeTaskID(id) {
			return nil, fmt.Errorf("invalid SWE-bench Pro task ID %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate SWE-bench Pro task ID %q", id)
		}
		seen[id] = struct{}{}
	}
	executionIDs := options.ExecutionTaskIDs
	if executionIDs == nil {
		executionIDs = options.TaskIDs
	} else if len(executionIDs) != len(options.TaskIDs) {
		return nil, errors.New("SWE-bench Pro execution task IDs must match task IDs")
	} else {
		seen = make(map[string]struct{}, len(executionIDs))
		for index, id := range executionIDs {
			if !safeExecutionTaskID(options.TaskIDs[index], id) {
				return nil, fmt.Errorf("invalid SWE-bench Pro execution task ID %q", id)
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("duplicate SWE-bench Pro execution task ID %q", id)
			}
			seen[id] = struct{}{}
		}
	}
	return &Benchmark{
		root: filepath.Clean(options.Root), taskIDs: slices.Clone(options.TaskIDs), executionTaskIDs: slices.Clone(executionIDs),
		outputDir: filepath.Clean(options.OutputDir), datasetRevision: options.DatasetRevision, evaluatorRevision: options.EvaluatorRevision,
		details: make(map[string]taskDetails, len(options.TaskIDs)),
	}, nil
}

func (b *Benchmark) Tasks(ctx context.Context) ([]core.Task, error) {
	if err := b.verifySources(ctx); err != nil {
		return nil, err
	}
	records, err := loadDataset(b.datasetRoot(), b.evaluatorRoot())
	if err != nil {
		return nil, err
	}
	if len(records) != publicTaskCount {
		return nil, fmt.Errorf("pinned public SWE-bench Pro dataset has %d tasks; want %d", len(records), publicTaskCount)
	}
	byID := make(map[string]taskRecord, len(records))
	for _, record := range records {
		byID[record.row.InstanceID] = record
	}
	tasks := make([]core.Task, 0, len(b.taskIDs))
	details := make(map[string]taskDetails, len(b.taskIDs))
	for index, id := range b.taskIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("SWE-bench Pro task %q does not exist in the pinned public dataset", id)
		}
		executionID := b.executionTaskIDs[index]
		task, private, err := record.task(executionID)
		if err != nil {
			return nil, fmt.Errorf("load SWE-bench Pro task %q: %w", id, err)
		}
		tasks = append(tasks, task)
		details[executionID] = private
	}
	b.mu.Lock()
	b.details = details
	b.mu.Unlock()
	return tasks, nil
}

func (record taskRecord) task(executionID string) (core.Task, taskDetails, error) {
	image, err := containerimage.ValidateTagOnly("docker.io/jefzda/sweap-images:" + record.row.DockerHubTag)
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("dockerhub_tag: %w", err)
	}
	instruction := record.row.ProblemStatement + "\n\nRequirements:\n" + record.row.Requirements + "\n\nNew interfaces introduced:\n" + record.row.Interface
	return core.Task{
		ID: executionID, Instruction: instruction, Timeout: defaultAgentTimeout,
		Environment: core.Environment{
			Image: image, Workdir: repositoryPath, CPU: 4, MemoryMB: 30 * 1024, StorageMB: 20 * 1024, AllowNetwork: true,
			ExecUser: agentExecUser,
		},
	}, record.details, nil
}

func (b *Benchmark) datasetRoot() string   { return filepath.Join(b.root, "dataset") }
func (b *Benchmark) evaluatorRoot() string { return filepath.Join(b.root, "evaluator") }

func (b *Benchmark) verifySources(ctx context.Context) error {
	if err := VerifyRevision(ctx, b.datasetRoot(), b.datasetRevision); err != nil {
		return fmt.Errorf("verify SWE-bench Pro dataset: %w", err)
	}
	if err := VerifyRevision(ctx, b.evaluatorRoot(), b.evaluatorRevision); err != nil {
		return fmt.Errorf("verify SWE-bench Pro evaluator: %w", err)
	}
	return nil
}

func safeTaskID(id string) bool { return safeIdentity(id, 128) }

func safeExecutionTaskID(logicalID, id string) bool {
	if !safeIdentity(id, 149) || !strings.HasPrefix(id, logicalID+"-") {
		return false
	}
	suffix := strings.TrimPrefix(id, logicalID+"-")
	if len(suffix) < 3 {
		return false
	}
	index, err := strconv.ParseUint(suffix, 10, 64)
	return err == nil && index > 0
}

func safeIdentity(id string, limit int) bool {
	if id == "" || len(id) > limit {
		return false
	}
	for index, character := range id {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return false
	}
	return true
}
