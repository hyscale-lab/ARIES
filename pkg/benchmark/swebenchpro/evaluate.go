package swebenchpro

import (
	"bytes"
	"context"
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

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	workspaceContainerPath = "/workspace"
	patchContainerPath     = workspaceContainerPath + "/patch.diff"
	runScriptContainerPath = workspaceContainerPath + "/run_script.sh"
	parserContainerPath    = workspaceContainerPath + "/parser.py"
	stdoutContainerPath    = workspaceContainerPath + "/stdout.log"
	stderrContainerPath    = workspaceContainerPath + "/stderr.log"
	outputContainerPath    = workspaceContainerPath + "/output.json"
	quiesceAgentPredicate  = `target=$1; attempts=0; while :; do found=0; for status in /proc/[0-9]*/status; do [ -r "$status" ] || continue; uid=; while IFS=' ' read -r key value rest; do [ "$key" = "Uid:" ] || continue; uid=$value; break; done <"$status"; [ "$uid" = "$target" ] || continue; pid=${status#/proc/}; pid=${pid%/status}; case "$pid" in ''|*[!0-9]*|0|1) exit 71;; esac; /bin/kill -KILL "$pid" 2>/dev/null || :; found=1; done; [ "$found" -eq 0 ] && exit 0; attempts=$((attempts+1)); [ "$attempts" -lt 100 ] || exit 70; done`
	maxCandidatePatchSize  = 16 << 20
	maxParserOutputSize    = 16 << 20
	maxVerifierLogSize     = 256 << 20
)

var evaluationArtifactNames = []string{
	"candidate.raw.patch",
	"candidate.patch",
	"stdout.log",
	"stderr.log",
	"output.json",
	"reason.txt",
}

// Evaluate follows the upstream SWE-bench Pro evaluator. After the Runner has
// stopped the harness and revoked its bridge, it captures the agent's patch
// from the task sandbox, then runs upstream's entry script in a fresh sandbox
// from the same image: reset and check out the base commit, apply the patch,
// check out the gold verifier files, run the pinned script, and parse its
// output. The agent sandbox contributes nothing but the patch.
func (b *Benchmark) Evaluate(ctx context.Context, task core.Task, sandbox runner.Sandbox, sandboxes runner.EvaluationSandboxes) (core.Evaluation, error) {
	started := time.Now()
	evaluation := core.Evaluation{Status: core.StatusFailed, VerifierStatus: core.StatusFailed}
	finish := func(err error) (core.Evaluation, error) {
		evaluation.Duration = time.Since(started)
		if err != nil {
			evaluation.Error = err.Error()
		}
		return evaluation, err
	}

	if sandbox == nil {
		return finish(errors.New("SWE-bench Pro evaluator requires the task sandbox"))
	}
	if sandboxes == nil {
		return finish(errors.New("SWE-bench Pro evaluator requires fresh evaluation sandboxes"))
	}
	b.mu.RLock()
	details, loaded := b.details[task.ID]
	b.mu.RUnlock()
	if !loaded {
		return finish(fmt.Errorf("SWE-bench Pro task %q was not loaded by Tasks", task.ID))
	}
	if err := b.verifySources(ctx); err != nil {
		return finish(fmt.Errorf("reverify SWE-bench Pro sources before evaluation: %w", err))
	}
	agentStreamer, ok := sandbox.(runner.StreamExecutor)
	if !ok {
		return finish(errors.New("SWE-bench Pro patch capture requires streaming sandbox execution"))
	}
	for name, source := range map[string]string{"pinned run script": details.runScript, "pinned parser": details.parser} {
		if err := requireRegularPrivateFile(source, name); err != nil {
			return finish(err)
		}
	}

	artifactDir := filepath.Join(b.outputDir, task.ID, "evaluation")
	if err := prepareEvaluationArtifacts(artifactDir); err != nil {
		return finish(err)
	}
	paths := make([]string, len(evaluationArtifactNames))
	for index, name := range evaluationArtifactNames {
		paths[index] = filepath.Join(artifactDir, name)
	}
	evaluation.LogPaths = paths
	rawPatchPath, effectivePatchPath := paths[0], paths[1]
	stdoutPath, stderrPath, outputPath, reasonPath := paths[2], paths[3], paths[4], paths[5]
	var reasons []string

	if err := quiesceAgentProcesses(ctx, sandbox); err != nil {
		return finish(err)
	}
	captureNote, err := captureCandidatePatch(ctx, sandbox, agentStreamer, details.baseCommit, rawPatchPath)
	if err != nil {
		return finish(err)
	}
	if captureNote != "" {
		reasons = append(reasons, captureNote)
	}
	if err := secureDownloadedFile(rawPatchPath, maxCandidatePatchSize, "raw candidate patch"); err != nil {
		return finish(err)
	}
	rawPatch, err := os.ReadFile(rawPatchPath)
	if err != nil {
		return finish(fmt.Errorf("read raw candidate patch: %w", err))
	}
	effectivePatch, err := stripBinaryPatchSections(rawPatch)
	if err != nil {
		return finish(fmt.Errorf("sanitize candidate patch: %w", err))
	}
	if err := writePrivateArtifact(effectivePatchPath, effectivePatch); err != nil {
		return finish(fmt.Errorf("write effective candidate patch: %w", err))
	}

	fresh, err := sandboxes.Start(ctx, evaluationEnvironment(task.Environment))
	if err != nil {
		return finish(fmt.Errorf("start fresh SWE-bench Pro evaluation sandbox: %w", err))
	}
	if _, ok := fresh.(runner.LimitedDownloader); !ok {
		return finish(errors.New("SWE-bench Pro evaluation requires bounded sandbox downloads"))
	}
	streamer, ok := fresh.(runner.StreamExecutor)
	if !ok {
		return finish(errors.New("SWE-bench Pro evaluation requires streaming sandbox execution"))
	}
	if _, err := execOK(ctx, fresh, "create evaluator workspace", core.Command{
		Path: "/bin/mkdir", Args: []string{"-p", "--", workspaceContainerPath},
	}); err != nil {
		return finish(err)
	}
	for _, file := range []struct {
		name        string
		source      string
		destination string
	}{
		{name: "candidate patch", source: effectivePatchPath, destination: patchContainerPath},
		{name: "pinned run script", source: details.runScript, destination: runScriptContainerPath},
		{name: "pinned parser", source: details.parser, destination: parserContainerPath},
	} {
		if err := fresh.Upload(ctx, file.source, file.destination); err != nil {
			return finish(fmt.Errorf("upload %s: %w", file.name, err))
		}
	}

	if _, err := execOK(ctx, fresh, "reset repository to base commit", evaluatorGitCommand("reset", "--hard", details.baseCommit)); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, fresh, "check out base commit", evaluatorGitCommand("checkout", details.baseCommit)); err != nil {
		return finish(err)
	}
	applyResult, err := fresh.Exec(ctx, evaluatorGitCommand("apply", "-v", patchContainerPath))
	if err != nil {
		return finish(fmt.Errorf("apply candidate patch: %w", err))
	}
	if applyResult.ExitCode != 0 {
		reason := fmt.Sprintf("candidate patch did not apply: exit code %d", applyResult.ExitCode)
		if detail := strings.TrimSpace(applyResult.Stderr); detail != "" {
			reason += ": " + detail
		}
		reasons = append(reasons, reason)
	}
	checkoutArgs := append([]string{"checkout", details.goldCommit, "--"}, details.verifierFiles...)
	if _, err := execOK(ctx, fresh, "check out verifier files", evaluatorGitCommand(checkoutArgs...)); err != nil {
		return finish(err)
	}

	stdoutFile, err := openPrivateArtifact(stdoutPath)
	if err != nil {
		return finish(fmt.Errorf("open verifier stdout: %w", err))
	}
	stderrFile, err := openPrivateArtifact(stderrPath)
	if err != nil {
		_ = stdoutFile.Close()
		return finish(fmt.Errorf("open verifier stderr: %w", err))
	}
	_, testErr := streamer.ExecStream(ctx, core.Command{
		Path: "/bin/bash", Args: []string{runScriptContainerPath, strings.Join(details.selectedTests, ",")},
		Dir: repositoryPath, Timeout: defaultVerifierTimeout, OutputLimitBytes: maxVerifierLogSize,
	}, nil, stdoutFile, stderrFile)
	if testErr = errors.Join(testErr, stdoutFile.Close(), stderrFile.Close()); testErr != nil {
		return finish(fmt.Errorf("run SWE-bench Pro verifier: %w", testErr))
	}
	if err := fresh.Upload(ctx, stdoutPath, stdoutContainerPath); err != nil {
		return finish(fmt.Errorf("upload verifier stdout for parser: %w", err))
	}
	if err := fresh.Upload(ctx, stderrPath, stderrContainerPath); err != nil {
		return finish(fmt.Errorf("upload verifier stderr for parser: %w", err))
	}
	if _, err := execOK(ctx, fresh, "run SWE-bench Pro parser", core.Command{
		Path: "/usr/bin/env", Args: []string{"python", parserContainerPath, stdoutContainerPath, stderrContainerPath, outputContainerPath},
		Dir: repositoryPath, Timeout: defaultVerifierTimeout,
	}); err != nil {
		return finish(err)
	}
	if err := downloadLimited(ctx, fresh, outputContainerPath, outputPath, maxParserOutputSize); err != nil {
		return finish(fmt.Errorf("download SWE-bench Pro parser output: %w", err))
	}
	if err := secureDownloadedFile(outputPath, maxParserOutputSize, "SWE-bench Pro parser output"); err != nil {
		return finish(err)
	}
	passed, err := parsePassedTests(outputPath)
	if err != nil {
		return finish(err)
	}
	missing := missingRequiredTests(details, passed)
	if len(missing) != 0 {
		reasons = append(reasons, "unresolved: missing required passing tests: "+strings.Join(missing, ", "))
		if err := writePrivateArtifact(reasonPath, []byte(strings.Join(reasons, "\n")+"\n")); err != nil {
			return finish(fmt.Errorf("write unresolved reason: %w", err))
		}
		return finish(nil)
	}
	reasons = append(reasons, "resolved: all FAIL_TO_PASS and PASS_TO_PASS tests passed")
	if err := writePrivateArtifact(reasonPath, []byte(strings.Join(reasons, "\n")+"\n")); err != nil {
		return finish(fmt.Errorf("write resolved reason: %w", err))
	}
	evaluation.Score = 1
	evaluation.Reward = 1
	evaluation.Status = core.StatusSucceeded
	evaluation.VerifierStatus = core.StatusSucceeded
	return finish(nil)
}

// captureCandidatePatch stages the agent's worktree and streams its diff
// against the base commit to the host, as the agent user: the repository and
// its Git metadata are agent-controlled, so no privileged Git runs on them. A
// Git failure leaves an empty patch, as an upstream agent run that produced no
// patch would; the returned note records why.
func captureCandidatePatch(ctx context.Context, sandbox runner.Sandbox, streamer runner.StreamExecutor, baseCommit, rawPatchPath string) (string, error) {
	rawFile, err := openPrivateArtifact(rawPatchPath)
	if err != nil {
		return "", fmt.Errorf("open raw candidate patch: %w", err)
	}
	stage, err := sandbox.Exec(ctx, agentGitCommand("add", "-A"))
	if err != nil {
		_ = rawFile.Close()
		return "", fmt.Errorf("stage candidate worktree: %w", err)
	}
	if stage.ExitCode != 0 {
		return candidateCaptureNote("stage", stage.ExitCode, stage.Stderr), rawFile.Close()
	}
	var stderr bytes.Buffer
	command := agentGitCommand("diff", "--cached", "--no-ext-diff", "--binary", baseCommit)
	command.OutputLimitBytes = maxCandidatePatchSize
	diff, err := streamer.ExecStream(ctx, command, nil, rawFile, &stderr)
	if err = errors.Join(err, rawFile.Close()); err != nil {
		return "", fmt.Errorf("capture candidate patch: %w", err)
	}
	if diff.ExitCode != 0 {
		if err := writePrivateArtifact(rawPatchPath, nil); err != nil {
			return "", fmt.Errorf("discard partial candidate patch: %w", err)
		}
		return candidateCaptureNote("diff", diff.ExitCode, stderr.String()), nil
	}
	return "", nil
}

func candidateCaptureNote(step string, exitCode int, stderr string) string {
	note := fmt.Sprintf("candidate patch could not be captured (git %s exit code %d); evaluating an empty patch", step, exitCode)
	if detail := strings.TrimSpace(stderr); detail != "" {
		note += ": " + detail
	}
	return note
}

// evaluationEnvironment is the task environment without the agent identity:
// upstream runs its entry script as the image's default user.
func evaluationEnvironment(environment core.Environment) core.Environment {
	environment.Env = maps.Clone(environment.Env)
	environment.ExecUser = ""
	return environment
}

func agentGitCommand(args ...string) core.Command {
	return core.Command{Path: "/usr/bin/git", Args: append([]string{"-C", repositoryPath}, args...), User: agentExecUser}
}

func evaluatorGitCommand(args ...string) core.Command {
	return core.Command{Path: "/usr/bin/git", Args: args, Dir: repositoryPath}
}

func downloadLimited(ctx context.Context, sandbox runner.Sandbox, source, destination string, limit int64) error {
	downloader, ok := sandbox.(runner.LimitedDownloader)
	if !ok {
		return errors.New("sandbox does not support bounded downloads")
	}
	return downloader.DownloadLimit(ctx, source, destination, limit)
}

func quiesceAgentProcesses(ctx context.Context, sandbox runner.Sandbox) error {
	_, err := execOK(ctx, sandbox, "quiesce non-root agent processes", core.Command{
		Path: "/bin/sh", Args: []string{"-c", quiesceAgentPredicate, "aries-swebenchpro-quiesce", agentUID}, User: rootExecUser,
	})
	return err
}

func openPrivateArtifact(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func prepareEvaluationArtifacts(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create evaluator artifact directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect evaluator artifact directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("evaluator artifact path is not a real directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure evaluator artifact directory: %w", err)
	}
	for _, name := range evaluationArtifactNames {
		artifact := filepath.Join(directory, name)
		if err := os.Remove(artifact); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale evaluator artifact %q: %w", artifact, err)
		}
	}
	return nil
}

func writePrivateArtifact(path string, content []byte) error {
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func requireRegularPrivateFile(path, name string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a regular file", name)
	}
	return nil
}

func secureDownloadedFile(path string, limit int64, name string) error {
	if err := requireRegularPrivateFile(path, name); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s size: %w", name, err)
	}
	if info.Size() > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure %s: %w", name, err)
	}
	return nil
}

func stripBinaryPatchSections(patch []byte) ([]byte, error) {
	if len(patch) > maxCandidatePatchSize {
		return nil, fmt.Errorf("candidate patch exceeds %d bytes", maxCandidatePatchSize)
	}
	starts := []int{}
	if bytes.HasPrefix(patch, []byte("diff --git ")) {
		starts = append(starts, 0)
	}
	for offset := 0; ; {
		index := bytes.Index(patch[offset:], []byte("\ndiff --git "))
		if index < 0 {
			break
		}
		offset += index + 1
		starts = append(starts, offset)
	}
	if len(starts) == 0 {
		return slices.Clone(patch), nil
	}

	var result bytes.Buffer
	if starts[0] != 0 {
		result.Write(patch[:starts[0]])
	}
	for index, start := range starts {
		end := len(patch)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		section := patch[start:end]
		if binaryPatchSection(section) {
			continue
		}
		result.Write(section)
	}
	return result.Bytes(), nil
}

func binaryPatchSection(section []byte) bool {
	for _, line := range bytes.Split(section, []byte{'\n'}) {
		if bytes.Equal(line, []byte("GIT binary patch")) || bytes.HasPrefix(line, []byte("Binary files ")) && bytes.HasSuffix(line, []byte(" differ")) {
			return true
		}
	}
	return false
}

func parsePassedTests(path string) (map[string]struct{}, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open SWE-bench Pro parser output: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxParserOutputSize+1))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("decode SWE-bench Pro parser output: %w", err)
	}
	if object == nil {
		return nil, errors.New("SWE-bench Pro parser output must be a JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	rawTests, ok := object["tests"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawTests), []byte("null")) {
		return nil, errors.New("SWE-bench Pro parser output must contain a tests array")
	}
	var tests []map[string]json.RawMessage
	if err := json.Unmarshal(rawTests, &tests); err != nil {
		return nil, fmt.Errorf("decode SWE-bench Pro parser tests: %w", err)
	}
	passed := make(map[string]struct{}, len(tests))
	for index, test := range tests {
		var name, status string
		rawName, hasName := test["name"]
		rawStatus, hasStatus := test["status"]
		if !hasName || !hasStatus || json.Unmarshal(rawName, &name) != nil || json.Unmarshal(rawStatus, &status) != nil || name == "" {
			return nil, fmt.Errorf("SWE-bench Pro parser test %d must contain string name and status fields", index)
		}
		if status == "PASSED" {
			passed[name] = struct{}{}
		}
	}
	return passed, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing SWE-bench Pro parser output: %w", err)
	}
	return errors.New("SWE-bench Pro parser output contains trailing JSON")
}

func missingRequiredTests(details taskDetails, passed map[string]struct{}) []string {
	missing := make([]string, 0)
	seen := make(map[string]struct{}, len(details.failToPass)+len(details.passToPass))
	for _, name := range append(slices.Clone(details.failToPass), details.passToPass...) {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := passed[name]; !ok {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}
