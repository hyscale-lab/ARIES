package deepresearchbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// Default{Query,Reference,Criteria}File name the three vendored, mutually
// consistent files the pinned Deep Research Bench checkout ships (all keyed
// by the same 100 numeric task IDs, from a single pinned commit of
// github.com/Ayanami0730/deep_research_bench): the canonical {id, prompt}
// pairs, the official reference article to score candidates against, and the
// per-task RACE weighted-criteria rubric used by the judge.
const (
	DefaultQueryFile     = "data/prompt_data/query.jsonl"
	DefaultReferenceFile = "data/test_data/cleaned_data/reference.jsonl"
	DefaultCriteriaFile  = "data/criteria_data/criteria.jsonl"
)

// expectedTaskCount and the expected ID range are a corruption guard: the
// pinned Deep Research Bench revision is known to contain exactly 100 tasks
// numbered 1..100. A row count or ID mismatch fails loudly instead of
// silently accepting a truncated or altered source file.
const expectedTaskCount = 100

// reportPath is the fixed, ARIES-owned location the agent is instructed to
// write its final report to. It deliberately lives outside any
// benchmark-declared workdir so it exists regardless of the task image's
// filesystem layout.
const reportPath = "/tmp/aries-report.md"

// The fixed wrapper adapts upstream's report-generation task to a tool-using
// harness. Keep the dataset question verbatim and separate from judge inputs.
const taskPromptPrefix = `Complete the following Deep Research Bench task as an autonomous research agent using the tools available in this session.

## Research question

`

// Evaluate collects this file after harness termination and bridge revocation;
// the harness's final chat response is not the report artifact.
const reportInstruction = `

## Research and report instructions

- Research the question by searching the web and reading sources. Prefer native search and page-reading tools when available; if they fail or are unavailable, use another available method.
- Follow the question's scope, language, and requested deliverables. Write a complete research report, using Markdown headings unless the question requests another format.
- Support sourced claims with inline Markdown links to the source URL, e.g. NIH 2025, placed next to the claim. Do not use numbered footnotes or a separate reference list. Do not invent sources; state limitations when evidence is unavailable.
- Write the final report to ` + reportPath + ` by actually invoking a file-writing or shell tool before ending the task. A chat reply alone does not create the report file and will not be collected for evaluation.
- After saving the report, reply with a brief completion message.
`

// defaultRewardThreshold is the RACE overall score (0-100, scaled from the
// underlying target/(target+reference) ratio — see evaluate.go) at or above
// which a task is scored as a pass (Reward = 1). 50 means the candidate ties
// the reference article, not "scored 50% on an absolute rubric."
const defaultRewardThreshold = 50

// Options selects tasks from one pinned Deep Research Bench checkout.
type Options struct {
	Root    string
	TaskIDs []string
	// ExecutionTaskIDs optionally renames each TaskIDs entry to a distinct
	// per-occurrence ID (e.g. "1-001"), mirroring Terminal-Bench 2's
	// ExecutionTaskIDs. The Runner always requests a suffixed occurrence ID,
	// even for a single, non-repeated run, so wiring must supply this rather
	// than reuse TaskIDs directly whenever a numeric ID would not parse.
	// Defaults to TaskIDs when nil.
	ExecutionTaskIDs []string
	OutputDir        string
	Revision         string
	QueryFile        string
	ReferenceFile    string
	CriteriaFile     string
	TaskTimeout      time.Duration
	Environment      core.Environment
	Judge            core.ModelConfig
	JudgeDisabled    bool
	FactJudge        core.ModelConfig
	JinaAPIKeyEnv    string
	APIKeyLookup     func(string) ([]byte, bool)
	RewardThreshold  float64
}

// Benchmark discovers selected Deep Research Bench tasks and retains their
// private host-side judge inputs. Each occurrence maps its execution ID back
// to the numeric task ID used by the dataset.
type Benchmark struct {
	tasks            []core.Task
	root             string
	taskIDs          []string
	executionTaskIDs []string
	outputDir        string
	revision         string
	queryFile        string
	referenceFile    string
	criteriaFile     string
	taskTimeout      time.Duration
	environment      core.Environment
	race             raceScorer // nil when judging is disabled via Options.JudgeDisabled
	fact             factRunner // nil when FACT evaluation is not configured or was skipped
	// factSkipReason explains why fact is nil despite a "fact" profile
	// block being present (e.g. the Jina API key env var isn't set in this
	// environment), as opposed to FACT simply never having been
	// configured. Empty when FACT is either enabled or was never
	// configured at all.
	factSkipReason  string
	rewardThreshold float64

	mu          sync.RWMutex
	numericIDs  map[string]int
	judgeInputs map[int]judgeInput
}

// judgeInput retains the selected row's private scoring inputs for this run.
type judgeInput struct {
	prompt    string
	reference string
	criteria  taskCriteria
}

var _ runner.Benchmark = (*Benchmark)(nil)

func New(options Options) (*Benchmark, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("deepresearchbench root is required")
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("deepresearchbench task IDs are required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("deepresearchbench output directory is required")
	}
	if strings.TrimSpace(options.Revision) == "" {
		return nil, errors.New("deepresearchbench revision is required")
	}
	if strings.TrimSpace(options.Environment.Image) == "" {
		return nil, errors.New("deepresearchbench environment image is required")
	}
	if options.APIKeyLookup == nil {
		return nil, errors.New("deepresearchbench API key lookup is required")
	}
	rewardThreshold := options.RewardThreshold
	if rewardThreshold == 0 {
		rewardThreshold = defaultRewardThreshold
	}
	if rewardThreshold <= 0 || rewardThreshold > 100 {
		return nil, errors.New("deepresearchbench reward threshold must be in (0, 100]")
	}
	var race raceScorer
	if !options.JudgeDisabled {
		if strings.TrimSpace(options.Judge.BaseURL) == "" || strings.TrimSpace(options.Judge.Model) == "" || strings.TrimSpace(options.Judge.APIKeyEnv) == "" {
			return nil, errors.New("deepresearchbench judge model config is required")
		}
		var err error
		race, err = newRaceClient(options.Judge, options.APIKeyLookup)
		if err != nil {
			return nil, fmt.Errorf("construct deepresearchbench RACE judge: %w", err)
		}
	} else if options.Judge != (core.ModelConfig{}) {
		return nil, errors.New("deepresearchbench judge model config must not be set when the judge is disabled")
	}

	factRequested := strings.TrimSpace(options.FactJudge.BaseURL) != "" || strings.TrimSpace(options.FactJudge.Model) != "" || strings.TrimSpace(options.JinaAPIKeyEnv) != ""
	var fact factRunner
	var factSkipReason string
	switch {
	case options.JudgeDisabled && factRequested:
		// Per product decision, disabling the judge is a master switch for
		// all LLM-based grading. A still-configured fact block is not an
		// error here — it's silently skipped, the same "degrade, don't fail
		// construction" treatment as a missing Jina key below.
		factSkipReason = "judge.enabled is false, which disables both RACE and FACT; skipping FACT"
	case options.JudgeDisabled:
		// FACT was never configured either; nothing to do or warn about.
	case factRequested:
		if strings.TrimSpace(options.FactJudge.BaseURL) == "" || strings.TrimSpace(options.FactJudge.Model) == "" || strings.TrimSpace(options.FactJudge.APIKeyEnv) == "" {
			return nil, errors.New("deepresearchbench FACT judge model config is incomplete")
		}
		if strings.TrimSpace(options.JinaAPIKeyEnv) == "" {
			return nil, errors.New("deepresearchbench FACT requires a Jina API key environment variable name")
		}
		// The FACT judge model and Jina env var *name* are structural
		// profile config and must be complete when FACT is configured at
		// all (checked above). Whether the named Jina env var actually
		// holds a value in this environment is a runtime concern, not a
		// configuration error: it's entirely normal to have a "fact" block
		// configured but only export JINA_API_KEY on some machines/CI
		// runs. Treat it as absent by degrading to FACT-disabled for this
		// run rather than failing construction outright.
		jinaKey, ok := options.APIKeyLookup(options.JinaAPIKeyEnv)
		if !ok || len(jinaKey) == 0 {
			factSkipReason = fmt.Sprintf("Jina API key environment variable %q is not set; skipping FACT", options.JinaAPIKeyEnv)
		} else {
			pipeline, err := newFactPipeline(options.FactJudge, options.APIKeyLookup, jinaKey)
			if err != nil {
				return nil, fmt.Errorf("construct deepresearchbench FACT pipeline: %w", err)
			}
			fact = pipeline
		}
	}

	seen := make(map[string]struct{}, len(options.TaskIDs))
	for _, id := range options.TaskIDs {
		if _, err := parseTaskID(id); err != nil {
			return nil, fmt.Errorf("invalid deepresearchbench task ID %q: %w", id, err)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate deepresearchbench task ID %q", id)
		}
		seen[id] = struct{}{}
	}

	executionTaskIDs := options.ExecutionTaskIDs
	if executionTaskIDs == nil {
		executionTaskIDs = options.TaskIDs
	} else if len(executionTaskIDs) != len(options.TaskIDs) {
		return nil, errors.New("deepresearchbench execution task IDs must match task IDs")
	} else {
		seenExecution := make(map[string]struct{}, len(executionTaskIDs))
		for index, id := range executionTaskIDs {
			if !safeExecutionTaskID(options.TaskIDs[index], id) {
				return nil, fmt.Errorf("invalid deepresearchbench execution task ID %q", id)
			}
			if _, duplicate := seenExecution[id]; duplicate {
				return nil, fmt.Errorf("duplicate deepresearchbench execution task ID %q", id)
			}
			seenExecution[id] = struct{}{}
		}
	}

	queryFile := options.QueryFile
	if queryFile == "" {
		queryFile = DefaultQueryFile
	}
	referenceFile := options.ReferenceFile
	if referenceFile == "" {
		referenceFile = DefaultReferenceFile
	}
	criteriaFile := options.CriteriaFile
	if criteriaFile == "" {
		criteriaFile = DefaultCriteriaFile
	}

	environment := options.Environment
	// Deep Research Bench tasks are open-ended web research; network access
	// is not optional the way it is for a Terminal-Bench task.
	environment.AllowNetwork = true
	environment.Services.SearchPort = 8888

	return &Benchmark{
		root:             filepath.Clean(options.Root),
		taskIDs:          slices.Clone(options.TaskIDs),
		executionTaskIDs: slices.Clone(executionTaskIDs),
		outputDir:        filepath.Clean(options.OutputDir),
		revision:         options.Revision,
		queryFile:        queryFile,
		referenceFile:    referenceFile,
		criteriaFile:     criteriaFile,
		taskTimeout:      options.TaskTimeout,
		environment:      environment,
		race:             race,
		fact:             fact,
		factSkipReason:   factSkipReason,
		rewardThreshold:  rewardThreshold,
	}, nil
}

// FactSkipReason reports why FACT is disabled despite being configured
// (e.g. the Jina API key environment variable named in the profile isn't
// set in this process's environment), so callers can surface it however
// they see fit (a log line, a startup warning, etc.). Returns "" both when
// FACT is running normally and when it was never configured at all — use
// alongside the profile's own Fact config if you need to distinguish those
// two cases.
func (b *Benchmark) FactSkipReason() string {
	return b.factSkipReason
}

// queryRow is one row of the vendored query.jsonl: the canonical {id,
// prompt} task definitions shared by every Deep Research Bench evaluation
// mode. Topic is decoded only for forward-compatibility; ARIES does not use
// it today.
type queryRow struct {
	ID       int    `json:"id"`
	Topic    string `json:"topic"`
	Language string `json:"language"`
	Prompt   string `json:"prompt"`
}

// referenceRow is one row of the vendored reference.jsonl: the official
// reference article RACE compares every candidate report against.
type referenceRow struct {
	ID      int    `json:"id"`
	Prompt  string `json:"prompt"`
	Article string `json:"article"`
}

// ForTask creates fresh occurrence state from the catalog loaded by Tasks.
// Private verifier inputs stay inside the benchmark; runtime owners remain per occurrence.
func (b *Benchmark) ForTask(logicalID, occurrenceID string) (runner.Benchmark, error) {
	if occurrenceID != logicalID && !safeExecutionTaskID(logicalID, occurrenceID) {
		return nil, fmt.Errorf("invalid deepresearchbench execution task ID %q", occurrenceID)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, task := range b.tasks {
		if task.ID != logicalID {
			continue
		}
		private, ok := b.numericIDs[logicalID]
		if !ok {
			break
		}
		task.ID = occurrenceID
		return &Benchmark{
			root:            b.root,
			outputDir:       b.outputDir,
			revision:        b.revision,
			race:            b.race,
			fact:            b.fact,
			factSkipReason:  b.factSkipReason,
			rewardThreshold: b.rewardThreshold,
			judgeInputs:     b.judgeInputs,
			tasks:           core.CloneTasks([]core.Task{task}),
			numericIDs:      map[string]int{occurrenceID: private},
		}, nil
	}
	return nil, fmt.Errorf("deepresearchbench task %q was not loaded by Tasks", logicalID)
}

func (b *Benchmark) Tasks(ctx context.Context) ([]core.Task, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.tasks != nil {
		return core.CloneTasks(b.tasks), nil
	}

	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return nil, err
	}

	prompts, err := loadPrompts(filepath.Join(b.root, b.queryFile))
	if err != nil {
		return nil, fmt.Errorf("load deepresearchbench query file %q: %w", b.queryFile, err)
	}

	var references map[int]string
	var criteria map[int]taskCriteria
	if b.race != nil {
		references, err = loadReferenceArticles(filepath.Join(b.root, b.referenceFile))
		if err != nil {
			return nil, fmt.Errorf("load deepresearchbench reference articles: %w", err)
		}
		criteria, err = loadCriteria(filepath.Join(b.root, b.criteriaFile))
		if err != nil {
			return nil, fmt.Errorf("load deepresearchbench criteria: %w", err)
		}
	}
	judgeInputs := make(map[int]judgeInput, len(b.taskIDs))
	tasks := make([]core.Task, 0, len(b.taskIDs))
	numericIDs := make(map[string]int, len(b.taskIDs))
	for index, id := range b.taskIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		numericID, err := parseTaskID(id)
		if err != nil {
			return nil, fmt.Errorf("invalid deepresearchbench task ID %q: %w", id, err)
		}
		prompt, ok := prompts[numericID]
		if !ok {
			return nil, fmt.Errorf("deepresearchbench task %q not found in %q", id, b.queryFile)
		}
		if b.race != nil {
			reference, ok := references[numericID]
			if !ok {
				return nil, fmt.Errorf("no reference article for deepresearchbench task %q", id)
			}
			rubric, ok := criteria[numericID]
			if !ok {
				return nil, fmt.Errorf("no criteria for deepresearchbench task %q", id)
			}
			judgeInputs[numericID] = judgeInput{prompt: prompt, reference: reference, criteria: rubric}
		}
		executionID := b.executionTaskIDs[index]
		numericIDs[executionID] = numericID
		tasks = append(tasks, core.Task{
			ID:          executionID,
			Instruction: taskPromptPrefix + prompt + reportInstruction,
			Timeout:     b.taskTimeout,
			Environment: b.environment,
		})
	}

	b.numericIDs = numericIDs
	b.judgeInputs = judgeInputs
	b.tasks = core.CloneTasks(tasks)
	return tasks, nil
}

// safeExecutionTaskID requires id to be logicalID immediately followed by a
// "-" and a positive decimal occurrence index, mirroring Terminal-Bench 2's
// execution task ID contract.
func safeExecutionTaskID(logicalID, id string) bool {
	if len(id) > 149 || !strings.HasPrefix(id, logicalID+"-") {
		return false
	}
	suffix := strings.TrimPrefix(id, logicalID+"-")
	if len(suffix) < 3 {
		return false
	}
	index, err := strconv.ParseUint(suffix, 10, 64)
	return err == nil && index > 0
}

func loadReferenceArticles(path string) (map[int]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open source file: %w", err)
	}
	defer file.Close()

	articles := make(map[int]string, expectedTaskCount)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row referenceRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse line %d: %w", lineNumber, err)
		}
		if strings.TrimSpace(row.Article) == "" {
			return nil, fmt.Errorf("line %d: empty reference article for id %d", lineNumber, row.ID)
		}
		if _, duplicate := articles[row.ID]; duplicate {
			return nil, fmt.Errorf("line %d: duplicate id %d", lineNumber, row.ID)
		}
		articles[row.ID] = row.Article
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read source file: %w", err)
	}
	return articles, nil
}

func loadPrompts(path string) (map[int]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open source file: %w", err)
	}
	defer file.Close()

	prompts := make(map[int]string, expectedTaskCount)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row queryRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse line %d: %w", lineNumber, err)
		}
		if strings.TrimSpace(row.Prompt) == "" {
			return nil, fmt.Errorf("line %d: empty prompt for id %d", lineNumber, row.ID)
		}
		if _, duplicate := prompts[row.ID]; duplicate {
			return nil, fmt.Errorf("line %d: duplicate id %d", lineNumber, row.ID)
		}
		prompts[row.ID] = row.Prompt
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read source file: %w", err)
	}

	if len(prompts) != expectedTaskCount {
		return nil, fmt.Errorf("expected exactly %d tasks, found %d", expectedTaskCount, len(prompts))
	}
	for id := 1; id <= expectedTaskCount; id++ {
		if _, ok := prompts[id]; !ok {
			return nil, fmt.Errorf("missing expected task id %d", id)
		}
	}
	return prompts, nil
}

// parseTaskID accepts only the canonical decimal string form (no leading
// zeros, no sign, no whitespace) of an in-range Deep Research Bench task ID.
func parseTaskID(id string) (int, error) {
	if id == "" || len(id) > 3 {
		return 0, errors.New("must be a decimal task ID")
	}
	if id != "0" && strings.HasPrefix(id, "0") {
		return 0, errors.New("must not have a leading zero")
	}
	for _, character := range id {
		if character < '0' || character > '9' {
			return 0, errors.New("must contain only digits")
		}
	}
	numericID, err := strconv.Atoi(id)
	if err != nil {
		return 0, err
	}
	if numericID < 1 || numericID > expectedTaskCount {
		return 0, fmt.Errorf("must be between 1 and %d", expectedTaskCount)
	}
	return numericID, nil
}
