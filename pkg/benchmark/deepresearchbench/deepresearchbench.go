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

// reportInstruction is appended to every task's instruction so the agent
// knows the evaluation contract, on top of taskPromptTemplate. Unlike the
// shell CLI tools this used to point at, native web_search/web_fetch (see
// pkg/harness/openclaw/config.go) are advertised to the model structurally
// as callable tools, so no prompt-level discovery hint is needed here.
//
// The "you must do this by actually invoking a tool" clause exists because
// weaker models have been observed to end their final turn narrating intent
// ("now let me write the report to ...") without ever issuing the tool call
// that creates the file, which OpenClaw then treats as a terminal response
// with nothing written.
const reportInstruction = "\n\nWrite your final, complete research report to " + reportPath + ". " +
	"You must do this by actually invoking a tool (e.g. a shell/exec command that creates the file) " +
	"in this same turn — a message that only states an intention to write the report, without a " +
	"tool call that performs it, does not satisfy this requirement and will be scored as a failure."

// taskPromptTemplate is the fixed instruction every Deep Research Bench task
// is wrapped in before being sent to the harness. It is intentionally not
// configurable: the citation format it mandates is a hard requirement of the
// FACT grading pipeline (see criteria.go/fact.go), so every task must use the
// exact same contract. "{question}" is replaced with the task's actual
// research prompt by applyPromptTemplate.
//
// The web fetch/extract nudge (step 3, and again under Rules) exists because
// models are structurally aware of the tool (it's in their function-calling
// schema) but don't reliably prefer it: observed smoke-test transcripts show
// the agent writing its own curl-plus-HTML-parser pipeline over the terminal
// tool instead of calling web_fetch/web_extract, which is slower and
// duplicates work the tool already does — for Hermes, extraction is backed
// by Tavily when harness.web_search.extract_api_key_env is configured (see
// pkg/harness/hermes/config.go); for OpenClaw, web_fetch needs no backend.
const taskPromptTemplate = "" +
	"You are an autonomous research analyst tackling a DeepResearch-Bench task in\n" +
	"a Linux container. Carry it out by searching the web, reading sources, and\n" +
	"writing a long-form report.\n" +
	"\n" +
	"## Research Question\n" +
	"\n" +
	"{question}\n" +
	"\n" +
	"## Task\n" +
	"\n" +
	"1. Read the research question carefully — note the exact scope, language,\n" +
	"   and any specific deliverables the prompter asks for.\n" +
	"2. Plan the work: decompose the question into sub-questions and decide what\n" +
	"   evidence each sub-question needs.\n" +
	"3. Search the web with the search tool to find authoritative sources, then\n" +
	"   read them with your web fetch/extract tool (e.g. `web_fetch`/`web_extract`)\n" +
	"   — it returns the page's readable text directly. Do not write a custom\n" +
	"   script (curl, wget, a Python HTML parser, etc.) to download and parse\n" +
	"   pages yourself; that duplicates what the tool already does.\n" +
	"4. Synthesize the evidence into a coherent long-form markdown report at\n" +
	"   `" + reportPath + "`, with **inline citations** to the source URLs.\n" +
	"5. When the report is complete, reply with a single line: `DONE`.\n" +
	"\n" +
	"## Output format\n" +
	"\n" +
	"Markdown with section headings. Use the language of the research question\n" +
	"(English or Chinese).\n" +
	"\n" +
	"### Citation format — strict, automated grading\n" +
	"\n" +
	"Every non-trivial factual claim must be followed by an **inline markdown\n" +
	"link** of the form `[short label](https://full-public-url)`. This is the\n" +
	"ONLY citation format accepted by the automated grader (see \"Why\" below).\n" +
	"\n" +
	"**Correct — markdown link inline next to the claim:**\n" +
	"\n" +
	"```\n" +
	"According to a 2025 NIH meta-analysis, magnesium intake above 400 mg/day\n" +
	"reduces CVD mortality by 12% [NIH meta-analysis 2025](https://pubmed.ncbi.nlm.nih.gov/12345678/).\n" +
	"```\n" +
	"\n" +
	"**Wrong — numbered footnote with separate reference list:**\n" +
	"\n" +
	"```\n" +
	"According to a 2025 NIH meta-analysis, magnesium intake above 400 mg/day\n" +
	"reduces CVD mortality by 12% [4].\n" +
	"...\n" +
	"References\n" +
	"[4] NIH meta-analysis 2025, https://pubmed.ncbi.nlm.nih.gov/12345678/\n" +
	"```\n" +
	"\n" +
	"**Wrong — bare URL or angle-bracket URL:**\n" +
	"\n" +
	"```\n" +
	"... reduces CVD mortality by 12% (https://pubmed.ncbi.nlm.nih.gov/12345678/).\n" +
	"... reduces CVD mortality by 12% <https://pubmed.ncbi.nlm.nih.gov/12345678/>.\n" +
	"```\n" +
	"\n" +
	"**Wrong — citation only in a final reference table, no inline link:**\n" +
	"\n" +
	"```\n" +
	"... reduces CVD mortality by 12%.\n" +
	"\n" +
	"| ref | title | url |\n" +
	"|---|---|---|\n" +
	"| 1 | NIH meta-analysis 2025 | https://pubmed.ncbi.nlm.nih.gov/12345678/ |\n" +
	"```\n" +
	"\n" +
	"### URL rules\n" +
	"\n" +
	"- URLs must resolve from the open internet (no `localhost`, no auth\n" +
	"  walls, no PDF-behind-login).\n" +
	"- Cite the page you actually read, not a search-result page or an\n" +
	"  internal redirect.\n" +
	"\n" +
	"### Why this matters\n" +
	"\n" +
	"The grader (FACT pipeline) extracts every `[text](url)` markdown link\n" +
	"from the report, fetches each URL via Jina Reader, and uses an LLM to\n" +
	"verify the cited claim is supported by the page. Citations in **any\n" +
	"other format are silently dropped — your citation accuracy will be 0**\n" +
	"no matter how good your research was.\n" +
	"\n" +
	"### Length\n" +
	"\n" +
	"A few thousand words is typical for PhD-level questions. Don't pad —\n" +
	"but don't bullet-point your way out of substance either.\n" +
	"\n" +
	"## Rules\n" +
	"\n" +
	"- Edit files directly with the file-editing tool. Do not paste the report\n" +
	"  into chat.\n" +
	"- Do not write markdown fences, explanations, or summaries in your final\n" +
	"  reply.\n" +
	"- Do not ask the user for clarification — infer from the prompt.\n" +
	"- Treat your search/fetch tool outputs as evidence; do not invent sources.\n" +
	"  If a search returns nothing useful, broaden the query or try a different\n" +
	"  angle — but never fabricate a citation.\n" +
	"- Prefer your web fetch/extract tool over shell commands (curl, wget) or\n" +
	"  ad-hoc scripts for retrieving page content — it is faster and already\n" +
	"  extracts readable text, so there is nothing to gain by reimplementing it.\n" +
	"- The agent loop is on a wall-clock budget. Plan early; cut depth on\n" +
	"  sub-questions where you've already found enough evidence rather than\n" +
	"  searching infinitely.\n" +
	"- Reply `DONE` as a standalone line, with nothing else, once the report at\n" +
	"  `" + reportPath + "` is written."

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

// Benchmark discovers selected Deep Research Bench tasks. Unlike
// Terminal-Bench 2, Deep Research Bench has no private verifier material, but
// it does retain a mapping from each occurrence's execution ID back to its
// numeric task ID, populated by Tasks() and read by Evaluate.
type Benchmark struct {
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

	mu         sync.RWMutex
	numericIDs map[string]int
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

func (b *Benchmark) Tasks(ctx context.Context) ([]core.Task, error) {
	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return nil, err
	}

	prompts, err := loadPrompts(filepath.Join(b.root, b.queryFile))
	if err != nil {
		return nil, fmt.Errorf("load deepresearchbench query file %q: %w", b.queryFile, err)
	}

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
		executionID := b.executionTaskIDs[index]
		numericIDs[executionID] = numericID
		tasks = append(tasks, core.Task{
			ID:          executionID,
			Instruction: applyPromptTemplate(taskPromptTemplate, prompt) + reportInstruction,
			Timeout:     b.taskTimeout,
			Environment: b.environment,
		})
	}

	b.mu.Lock()
	b.numericIDs = numericIDs
	b.mu.Unlock()
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

// applyPromptTemplate mirrors the {question}-placeholder convention used by
// harness.realtime.agent_question_template: an empty template leaves the raw
// prompt unchanged.
func applyPromptTemplate(template, prompt string) string {
	if template == "" {
		return prompt
	}
	return strings.ReplaceAll(template, "{question}", prompt)
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
