// Package sweatlas implements the Codebase QA track of SWE-Atlas
// (https://github.com/scaleapi/SWE-Atlas): grading is done host-side by an
// LLM judge against a per-task rubric, like Deep Research Bench, rather than
// a deterministic pass/fail script. The only sandbox material Evaluate
// touches is the agent's own answer file (answerPath); the private rubric,
// prompts, and judge HTTP calls never enter the sandbox. Only the QA track
// is implemented; SWE-Atlas's Test-Writing and Refactoring tracks have
// different task and grading shapes and are out of scope.
package sweatlas

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	DefaultRoot = ".cache/swe-atlas-qa"

	// qaSubdirectory is where the QA track's task directories live inside a
	// full SWE-Atlas checkout (alongside the tw/ and rf/ tracks this package
	// does not implement). Root/Setup/VerifyRevision all operate on the full
	// checkout (matching the pinned-revision git identity), while task
	// loading looks one level deeper.
	qaSubdirectory = "data/qa"

	// answerPath is the fixed location instruction.md already tells the
	// agent to write its final answer to (wrapped in <<FINAL_ANSWER>> tags).
	// Evaluate downloads it once; nothing else touches the sandbox during
	// evaluation.
	answerPath = "/logs/agent/answer.txt"
)

// Options selects tasks from one pinned SWE-Atlas checkout and names the
// judge model the injected verifier grades with. Judge and APIKeyLookup are
// both mandatory: unlike Deep Research Bench, there is no default judge (no
// benchmark model call happens otherwise) and no "disabled" grading mode —
// judge-graded rubric scoring is this benchmark's entire output.
type Options struct {
	Root             string
	TaskIDs          []string
	ExecutionTaskIDs []string
	OutputDir        string
	Revision         string
	Judge            core.ModelConfig
	APIKeyLookup     func(string) ([]byte, bool)
}

// Benchmark discovers selected SWE-Atlas QA tasks and retains their private
// verifier trees until evaluation.
type Benchmark struct {
	root             string
	taskIDs          []string
	executionTaskIDs []string
	outputDir        string
	revision         string
	judge            chatter

	mu      sync.RWMutex
	details map[string]taskDetails
}

// taskDetails carries the host-local path to the task's private tests/
// directory (rubrics.json, system_prompt.txt, user_prompt_template.txt,
// optional prompt.txt) so Evaluate can load them directly from the pinned
// checkout — none of this material is ever uploaded into the sandbox.
type taskDetails struct {
	testsDir string
	timeout  time.Duration
}

type taskFile struct {
	SchemaVersion string          `toml:"schema_version"`
	Artifacts     []string        `toml:"artifacts"`
	Task          taskSection     `toml:"task"`
	Metadata      toml.Primitive  `toml:"metadata"`
	Verifier      verifierSection `toml:"verifier"`
	Agent         agentSection    `toml:"agent"`
	Environment   environmentFile `toml:"environment"`
	Solution      toml.Primitive  `toml:"solution"`
}

type taskSection struct {
	Name string `toml:"name"`
}

// verifierSection.Env is decoded but intentionally unused: task.toml's
// [verifier.env] values are shell-style template placeholders (e.g.
// "${OPENAI_API_KEY}"), meant for Scale's own reference harness ("Harbor")
// to expand from its own process environment, not literal secrets. Grading
// no longer execs anything inside the sandbox at all (see evaluate.go), so
// this package never reads or synthesizes a verifier environment from it —
// the field only exists so decoding [verifier.env] doesn't trip
// rejectUnknownExecutionFields on real dataset task.toml files.
type verifierSection struct {
	TimeoutSeconds float64           `toml:"timeout_sec"`
	Env            map[string]string `toml:"env"`
}

type agentSection struct {
	TimeoutSeconds float64 `toml:"timeout_sec"`
}

type environmentFile struct {
	BuildTimeoutSeconds float64           `toml:"build_timeout_sec"`
	DockerImage         string            `toml:"docker_image"`
	CPUs                float64           `toml:"cpus"`
	MemoryMB            int               `toml:"memory_mb"`
	StorageMB           int               `toml:"storage_mb"`
	GPUs                int               `toml:"gpus"`
	AllowInternet       bool              `toml:"allow_internet"`
	MCPServers          []string          `toml:"mcp_servers"`
	Env                 map[string]string `toml:"env"`
}

var _ runner.Benchmark = (*Benchmark)(nil)

func New(options Options) (*Benchmark, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("sweatlas root is required")
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("sweatlas task IDs are required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("sweatlas output directory is required")
	}
	if strings.TrimSpace(options.Revision) == "" {
		return nil, errors.New("sweatlas revision is required")
	}
	if strings.TrimSpace(options.Judge.Provider) == "" || strings.TrimSpace(options.Judge.BaseURL) == "" ||
		strings.TrimSpace(options.Judge.Model) == "" || strings.TrimSpace(options.Judge.APIKeyEnv) == "" {
		return nil, errors.New("sweatlas judge model is required")
	}
	if options.APIKeyLookup == nil {
		return nil, errors.New("sweatlas judge API key lookup is required")
	}
	judge, err := newJudgeClient(options.Judge, options.APIKeyLookup)
	if err != nil {
		return nil, fmt.Errorf("construct sweatlas judge: %w", err)
	}

	seen := make(map[string]struct{}, len(options.TaskIDs))
	for _, id := range options.TaskIDs {
		if !safeTaskID(id) {
			return nil, fmt.Errorf("invalid sweatlas task ID %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate sweatlas task ID %q", id)
		}
		seen[id] = struct{}{}
	}
	executionIDs := options.ExecutionTaskIDs
	if executionIDs == nil {
		executionIDs = options.TaskIDs
	} else if len(executionIDs) != len(options.TaskIDs) {
		return nil, errors.New("sweatlas execution task IDs must match task IDs")
	} else {
		seen = make(map[string]struct{}, len(executionIDs))
		for index, id := range executionIDs {
			if !safeExecutionTaskID(options.TaskIDs[index], id) {
				return nil, fmt.Errorf("invalid sweatlas execution task ID %q", id)
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("duplicate sweatlas execution task ID %q", id)
			}
			seen[id] = struct{}{}
		}
	}

	return &Benchmark{
		root:             filepath.Clean(options.Root),
		taskIDs:          slices.Clone(options.TaskIDs),
		executionTaskIDs: slices.Clone(executionIDs),
		outputDir:        filepath.Clean(options.OutputDir),
		revision:         options.Revision,
		judge:            judge,
		details:          make(map[string]taskDetails, len(options.TaskIDs)),
	}, nil
}

func (b *Benchmark) Tasks(ctx context.Context) ([]core.Task, error) {
	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return nil, err
	}

	tasks := make([]core.Task, 0, len(b.taskIDs))
	details := make(map[string]taskDetails, len(b.taskIDs))
	for index, id := range b.taskIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task, private, err := loadTask(b.root, id)
		if err != nil {
			return nil, fmt.Errorf("load sweatlas task %q: %w", id, err)
		}
		executionID := b.executionTaskIDs[index]
		task.ID = executionID
		tasks = append(tasks, task)
		details[executionID] = private
	}

	b.mu.Lock()
	b.details = details
	b.mu.Unlock()
	return tasks, nil
}

// PrepareSandbox confirms the agent's designated answer path starts absent
// before the harness gets bridge access, mirroring Deep Research Bench's
// PrepareSandbox (pkg/benchmark/deepresearchbench/sandbox.go): sweatlas has
// no sandbox-resident private verifier tree either — its private material
// (rubrics, prompts) never enters the sandbox at all, staying host-side for
// the judge calls in Evaluate. This guards the one channel Evaluate trusts,
// the answer path, against a reused or dirty sandbox pre-seeding a fake
// answer.
func (b *Benchmark) PrepareSandbox(ctx context.Context, task core.Task, sandbox runner.Sandbox) error {
	if sandbox == nil {
		return errors.New("sweatlas preparation requires a live sandbox")
	}
	b.mu.RLock()
	_, loaded := b.details[task.ID]
	b.mu.RUnlock()
	if !loaded {
		return fmt.Errorf("sweatlas task %q was not loaded by Tasks", task.ID)
	}
	removed, err := sandbox.Exec(ctx, core.Command{Path: "/bin/rm", Args: []string{"-rf", "--", answerPath}})
	if err != nil {
		return fmt.Errorf("remove answer path before harness: %w", err)
	}
	if removed.ExitCode != 0 {
		return fmt.Errorf("remove answer path before harness: exit code %d", removed.ExitCode)
	}
	const absencePredicate = `for path do [ ! -e "$path" ] && [ ! -L "$path" ] || exit 1; done`
	probed, err := sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh", Args: []string{"-c", absencePredicate, "aries-answer-absence", answerPath},
	})
	if err != nil {
		return fmt.Errorf("confirm answer path absent before harness: %w", err)
	}
	if probed.ExitCode != 0 {
		return fmt.Errorf("confirm answer path absent before harness: exit code %d", probed.ExitCode)
	}
	return nil
}

func loadTask(root, id string) (core.Task, taskDetails, error) {
	taskDir := filepath.Join(root, qaSubdirectory, id)
	info, err := os.Stat(taskDir)
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("open task directory: %w", err)
	}
	if !info.IsDir() {
		return core.Task{}, taskDetails{}, errors.New("task path is not a directory")
	}

	var parsed taskFile
	meta, err := toml.DecodeFile(filepath.Join(taskDir, "task.toml"), &parsed)
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("parse task.toml: %w", err)
	}
	if err := rejectUnknownExecutionFields(meta); err != nil {
		return core.Task{}, taskDetails{}, err
	}
	image, err := validateTaskFile(id, parsed)
	if err != nil {
		return core.Task{}, taskDetails{}, err
	}

	instructionBytes, err := os.ReadFile(filepath.Join(taskDir, "instruction.md"))
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("read instruction.md: %w", err)
	}
	instruction := strings.TrimSpace(string(instructionBytes))
	if instruction == "" {
		return core.Task{}, taskDetails{}, errors.New("instruction.md is empty")
	}

	workdir, err := finalWorkdir(filepath.Join(taskDir, "environment", "Dockerfile"))
	if err != nil {
		return core.Task{}, taskDetails{}, err
	}
	testsDir, err := validateTestsDir(filepath.Join(taskDir, "tests"))
	if err != nil {
		return core.Task{}, taskDetails{}, err
	}

	agentTimeout, err := checkedDurationSeconds(parsed.Agent.TimeoutSeconds)
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("agent.timeout_sec: %w", err)
	}
	verifierTimeout, err := checkedDurationSeconds(parsed.Verifier.TimeoutSeconds)
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("verifier.timeout_sec: %w", err)
	}
	return core.Task{
			ID:          id,
			Instruction: instruction,
			Timeout:     agentTimeout,
			Environment: core.Environment{
				Image:        image,
				Workdir:      workdir,
				CPU:          parsed.Environment.CPUs,
				MemoryMB:     parsed.Environment.MemoryMB,
				GPUs:         parsed.Environment.GPUs,
				AllowNetwork: parsed.Environment.AllowInternet,
				Env:          cloneMap(parsed.Environment.Env),
			},
		}, taskDetails{
			testsDir: testsDir,
			timeout:  verifierTimeout,
		}, nil
}

func rejectUnknownExecutionFields(meta toml.MetaData) error {
	for _, key := range meta.Undecoded() {
		parts := key.String()
		top := parts
		if index := strings.IndexByte(parts, '.'); index >= 0 {
			top = parts[:index]
		}
		switch top {
		case "task", "metadata", "solution":
			continue
		default:
			return fmt.Errorf("task.toml contains unsupported field %q", parts)
		}
	}
	return nil
}

func validateTaskFile(id string, parsed taskFile) (string, error) {
	if parsed.SchemaVersion != "1.1" {
		return "", fmt.Errorf("unsupported task schema_version %q; want %q", parsed.SchemaVersion, "1.1")
	}
	if parsed.Task.Name != "scale-ai/"+id {
		return "", fmt.Errorf("sweatlas task identity directory=%q name=%q does not match", id, parsed.Task.Name)
	}
	if len(parsed.Artifacts) != 0 {
		return "", errors.New("task artifacts are unsupported")
	}
	if len(parsed.Environment.MCPServers) != 0 {
		return "", errors.New("environment.mcp_servers is unsupported")
	}
	if !finiteDurationSeconds(parsed.Environment.BuildTimeoutSeconds) {
		return "", errors.New("environment.build_timeout_sec must be finite and positive")
	}
	if !finiteDurationSeconds(parsed.Agent.TimeoutSeconds) {
		return "", errors.New("agent.timeout_sec must be finite and positive")
	}
	if !finiteDurationSeconds(parsed.Verifier.TimeoutSeconds) {
		return "", errors.New("verifier.timeout_sec must be finite and positive")
	}
	if !finitePositive(parsed.Environment.CPUs) {
		return "", errors.New("environment.cpus must be finite and positive")
	}
	if parsed.Environment.MemoryMB <= 0 || parsed.Environment.StorageMB <= 0 || parsed.Environment.GPUs < 0 {
		return "", fmt.Errorf("invalid environment resources memory_mb=%d storage_mb=%d gpus=%d", parsed.Environment.MemoryMB, parsed.Environment.StorageMB, parsed.Environment.GPUs)
	}
	if err := validateEnvironmentMap("environment.env", parsed.Environment.Env); err != nil {
		return "", err
	}

	image, err := containerimage.ValidateTagOnly(parsed.Environment.DockerImage)
	if err != nil {
		return "", fmt.Errorf("environment.docker_image: %w", err)
	}
	return image, nil
}

// validateTestsDir confirms the task's private tests/ directory contains the
// files Evaluate reads directly from the host checkout (rubrics.json,
// system_prompt.txt, user_prompt_template.txt — evaluate_answer.py's own
// required inputs; prompt.txt is optional there too) and returns its
// absolute path. Unlike the old sandbox-injection contract, test.sh is no
// longer required: nothing execs it anymore.
func validateTestsDir(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve tests directory: %w", err)
	}
	for _, name := range []string{"rubrics.json", "system_prompt.txt", "user_prompt_template.txt"} {
		info, err := os.Stat(filepath.Join(absolute, name))
		if err != nil {
			return "", fmt.Errorf("tests/%s is required: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("tests/%s is not a regular file", name)
		}
	}
	return absolute, nil
}

func finalWorkdir(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "/", nil
		}
		return "", fmt.Errorf("open environment/Dockerfile: %w", err)
	}
	defer file.Close()

	workdir := "/"
	var logicalLine strings.Builder
	continuing := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		continued := strings.HasSuffix(line, "\\")
		if continued {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		}
		if logicalLine.Len() > 0 && line != "" {
			logicalLine.WriteByte(' ')
		}
		logicalLine.WriteString(line)
		if continued {
			continuing = true
			continue
		}

		continuing = false
		var valid bool
		workdir, valid = applyDockerfileWorkdir(logicalLine.String(), workdir)
		logicalLine.Reset()
		if !valid {
			return "/", nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read environment/Dockerfile: %w", err)
	}
	if continuing || logicalLine.Len() != 0 {
		return "/", nil
	}
	return workdir, nil
}

func applyDockerfileWorkdir(line, workdir string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.ContainsRune(line, 0) {
		return workdir, false
	}
	switch {
	case strings.EqualFold(fields[0], "FROM"):
		if !validFromDirective(fields[1:]) {
			return workdir, false
		}
		return "/", true
	case strings.EqualFold(fields[0], "WORKDIR"):
		if len(fields) != 2 {
			return workdir, false
		}
		candidate := fields[1]
		if !path.IsAbs(candidate) {
			candidate = path.Join(workdir, candidate)
		}
		candidate = path.Clean(candidate)
		if !shellNeutralWorkdir(candidate) {
			return workdir, false
		}
		return candidate, true
	default:
		return workdir, true
	}
}

func validFromDirective(arguments []string) bool {
	if len(arguments) > 0 && strings.HasPrefix(arguments[0], "--platform=") && len(arguments[0]) > len("--platform=") {
		arguments = arguments[1:]
	}
	return len(arguments) == 1 && arguments[0] != "" ||
		len(arguments) == 3 && arguments[0] != "" && strings.EqualFold(arguments[1], "AS") && arguments[2] != ""
}

func shellNeutralWorkdir(value string) bool {
	if value == "/" {
		return true
	}
	if !strings.HasPrefix(value, "/") {
		return false
	}
	for _, segment := range strings.Split(value[1:], "/") {
		if segment == "" {
			return false
		}
		for _, character := range segment {
			if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
				character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func cloneMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func finiteDurationSeconds(value float64) bool {
	return finitePositive(value) && value*float64(time.Second) < math.Exp2(63)
}

func checkedDurationSeconds(seconds float64) (time.Duration, error) {
	if !finiteDurationSeconds(seconds) {
		return 0, errors.New("must be finite, positive, and convert to nanoseconds below 2^63")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func validateEnvironmentMap(name string, values map[string]string) error {
	for key, value := range values {
		if !validEnvironmentName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("%s contains invalid variable %q", name, key)
		}
	}
	return nil
}

func validEnvironmentName(value string) bool {
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

func safeTaskID(id string) bool {
	if id == "" || len(id) > 128 {
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

func safeExecutionTaskID(logicalID, id string) bool {
	if len(id) > 149 {
		return false
	}
	if !safeIdentity(id) || !strings.HasPrefix(id, logicalID+"-") {
		return false
	}
	suffix := strings.TrimPrefix(id, logicalID+"-")
	if len(suffix) < 3 {
		return false
	}
	index, err := strconv.ParseUint(suffix, 10, 64)
	return err == nil && index > 0
}

func safeIdentity(id string) bool {
	if id == "" {
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

// VerifyRevision confirms that root is the exact clean pinned checkout.
func VerifyRevision(ctx context.Context, root, revision string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify sweatlas checkout %q: %w: %s", root, err, output)
	}
	got := strings.TrimSpace(string(output))
	if got != revision {
		return fmt.Errorf("sweatlas checkout %q is revision %q; want pinned %q", root, got, revision)
	}
	status := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=all")
	output, err = status.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect sweatlas checkout %q: %w: %s", root, err, output)
	}
	if len(output) != 0 {
		return fmt.Errorf("sweatlas checkout %q has local changes; the pinned dataset must be clean", root)
	}
	return nil
}
