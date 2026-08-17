package swebenchpro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	evaluationContainerPath                = privateContainerPath + "/evaluation"
	candidateRawContainerPath              = evaluationContainerPath + "/candidate.raw.patch"
	candidateEffectiveContainerPath        = evaluationContainerPath + "/candidate.patch"
	verifierSnapshotContainerPath          = evaluationContainerPath + "/verifier-tests.tar"
	evaluationIgnoredSnapshotContainerPath = evaluationContainerPath + "/ignored-baseline.tar"
	evaluationGitSnapshotContainerPath     = evaluationContainerPath + "/git-baseline.tar"
	runScriptContainerPath                 = evaluationContainerPath + "/run_script.sh"
	parserContainerPath                    = evaluationContainerPath + "/parser.py"
	verifierStdoutContainerPath            = evaluationContainerPath + "/stdout.log"
	verifierStderrContainerPath            = evaluationContainerPath + "/stderr.log"
	parserOutputContainerPath              = evaluationContainerPath + "/output.json"
	quiesceAgentPredicate                  = `target=$1; attempts=0; while :; do found=0; for status in /proc/[0-9]*/status; do [ -r "$status" ] || continue; uid=; while IFS=' ' read -r key value rest; do [ "$key" = "Uid:" ] || continue; uid=$value; break; done <"$status"; [ "$uid" = "$target" ] || continue; pid=${status#/proc/}; pid=${pid%/status}; case "$pid" in ''|*[!0-9]*|0|1) exit 71;; esac; /bin/kill -KILL "$pid" 2>/dev/null || :; found=1; done; [ "$found" -eq 0 ] && exit 0; attempts=$((attempts+1)); [ "$attempts" -lt 100 ] || exit 70; done`
	restoreAgentWorktreePredicate          = `root=$1; agent=$2; /bin/chown -R -- "$agent" "$root" || exit 1; /bin/chown -R -- 0:0 "$root/.git" || exit 1; /bin/chmod -R go-w "$root/.git" || exit 1; /bin/chown 0:0 "$root" || exit 1; /bin/chmod 1777 "$root" || exit 1`
	installVerifierPredicate               = `root=$1; shift; for relative do current=$root; remaining=$relative; while [ "$remaining" != "${remaining#*/}" ]; do component=${remaining%%/*}; remaining=${remaining#*/}; current=$current/$component; if [ -e "$current" ] || [ -L "$current" ]; then [ -d "$current" ] && [ ! -L "$current" ] || exit 1; else /bin/mkdir -m 0755 -- "$current" || exit 1; fi; done; target=$current/$remaining; /bin/rm -rf -- "$target" || exit 1; done`
	secureVerifierPredicate                = `root=$1; shift; /bin/chown 0:0 "$root" || exit 1; /bin/chmod 1777 "$root" || exit 1; for relative do current=$root; remaining=$relative; while [ "$remaining" != "${remaining#*/}" ]; do component=${remaining%%/*}; remaining=${remaining#*/}; current=$current/$component; [ -d "$current" ] && [ ! -L "$current" ] || exit 1; /bin/chown 0:0 "$current" || exit 1; /bin/chmod 1777 "$current" || exit 1; done; target=$current/$remaining; [ -f "$target" ] && [ ! -L "$target" ] || exit 1; /bin/chown 0:0 "$target" || exit 1; /bin/chmod 0444 "$target" || exit 1; done`
	maxCandidatePatchSize                  = 16 << 20
	maxParserOutputSize                    = 16 << 20
	maxVerifierLogSize                     = 256 << 20
)

var evaluationArtifactNames = []string{
	"candidate.raw.patch",
	"candidate.patch",
	"stdout.log",
	"stderr.log",
	"output.json",
	"reason.txt",
}

// Evaluate captures the agent's patch before restoring the pinned base, then
// injects the private verifier snapshot and pinned evaluator only after the
// Runner has stopped the harness and revoked its bridge.
func (b *Benchmark) Evaluate(ctx context.Context, task core.Task, sandbox runner.Sandbox) (core.Evaluation, error) {
	started := time.Now()
	evaluation := core.Evaluation{Status: core.StatusFailed, VerifierStatus: core.StatusFailed}
	privateStaged := false
	finish := func(err error) (core.Evaluation, error) {
		finishErr := err
		if privateStaged {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), preparationCleanupTimeout)
			cleanupErr := removeAndProvePrivatePathAbsent(cleanupCtx, sandbox)
			cancel()
			if cleanupErr != nil {
				finishErr = errors.Join(finishErr, fmt.Errorf("scrub SWE-bench Pro evaluator staging: %w", cleanupErr))
			}
		}
		evaluation.Duration = time.Since(started)
		if finishErr != nil {
			evaluation.Error = finishErr.Error()
		}
		return evaluation, finishErr
	}

	if sandbox == nil {
		return finish(errors.New("SWE-bench Pro evaluator requires a live sandbox"))
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
	if _, ok := sandbox.(runner.LimitedDownloader); !ok {
		return finish(errors.New("SWE-bench Pro evaluation requires bounded sandbox downloads"))
	}
	streamer, ok := sandbox.(runner.StreamExecutor)
	if !ok {
		return finish(errors.New("SWE-bench Pro evaluation requires streaming sandbox execution"))
	}
	if details.snapshot == "" {
		return finish(fmt.Errorf("SWE-bench Pro task %q was not prepared before evaluation", task.ID))
	}
	if err := requireRegularPrivateFileSize(details.snapshot, "private verifier snapshot", maxVerifierSnapshotSize); err != nil {
		return finish(err)
	}
	if details.ignoredSnapshot == "" {
		return finish(fmt.Errorf("SWE-bench Pro task %q has no prepared ignored baseline", task.ID))
	}
	if err := requireRegularPrivateFileSize(details.ignoredSnapshot, "ignored baseline snapshot", maxIgnoredSnapshotSize); err != nil {
		return finish(err)
	}
	if details.gitSnapshot == "" {
		return finish(fmt.Errorf("SWE-bench Pro task %q has no prepared Git baseline", task.ID))
	}
	if err := requireRegularPrivateFileSize(details.gitSnapshot, "Git baseline snapshot", maxGitSnapshotSize); err != nil {
		return finish(err)
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

	if err := quiesceAgentProcesses(ctx, sandbox); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "remove agent-controlled SWE-bench Pro staging root", core.Command{
		Path: "/bin/rm", Args: []string{"-rf", "--", privateContainerPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "confirm agent-controlled staging root absent", core.Command{
		Path: "/bin/sh", Args: []string{"-c", absencePredicate, "aries-swebenchpro-absence", privateContainerPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "create SWE-bench Pro evaluator path", core.Command{
		Path: "/bin/mkdir", Args: []string{"-m", "0700", "-p", "--", evaluationContainerPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	privateStaged = true
	if err := sandbox.Upload(ctx, details.gitSnapshot, evaluationGitSnapshotContainerPath); err != nil {
		return finish(fmt.Errorf("upload Git baseline snapshot: %w", err))
	}
	if _, err := execOK(ctx, sandbox, "remove agent-controlled Git metadata", core.Command{
		Path: "/bin/rm", Args: []string{"-rf", "--", repositoryPath + "/.git"}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "restore sanitized Git baseline", core.Command{
		Path: "/bin/tar", Args: []string{"-xf", evaluationGitSnapshotContainerPath, "-C", repositoryPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "restore trusted repository-root ownership", core.Command{
		Path: "/bin/chown", Args: []string{"0:0", repositoryPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if err := proveRepositoryAtBase(ctx, sandbox, details.baseCommit); err != nil {
		return finish(err)
	}
	if err := proveRepositoryHistoryIsolated(ctx, sandbox, details.goldCommit); err != nil {
		return finish(err)
	}

	if _, err := execOK(ctx, sandbox, "stage candidate worktree", gitCommand("add", "-A")); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "capture candidate patch", gitCommand(
		"diff", "--cached", "--no-ext-diff", "--binary", "--output="+candidateRawContainerPath, details.baseCommit,
	)); err != nil {
		return finish(err)
	}
	if err := downloadLimited(ctx, sandbox, candidateRawContainerPath, rawPatchPath, maxCandidatePatchSize); err != nil {
		return finish(fmt.Errorf("download raw candidate patch: %w", err))
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
	if len(effectivePatch) > maxCandidatePatchSize {
		return finish(fmt.Errorf("effective candidate patch exceeds %d bytes", maxCandidatePatchSize))
	}
	if err := writePrivateArtifact(effectivePatchPath, effectivePatch); err != nil {
		return finish(fmt.Errorf("write effective candidate patch: %w", err))
	}

	if _, err := execOK(ctx, sandbox, "restore base before evaluation", gitCommand("reset", "--hard", details.baseCommit)); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "clean candidate worktree before evaluation", gitCommand("clean", "-ffd")); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "remove candidate ignored artifacts", gitCommand("clean", "-ffdX")); err != nil {
		return finish(err)
	}
	if err := sandbox.Upload(ctx, details.ignoredSnapshot, evaluationIgnoredSnapshotContainerPath); err != nil {
		return finish(fmt.Errorf("upload ignored baseline snapshot: %w", err))
	}
	if _, err := execOK(ctx, sandbox, "restore ignored build baseline", core.Command{
		Path: "/bin/tar", Args: []string{"-xf", evaluationIgnoredSnapshotContainerPath, "-C", repositoryPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if err := sandbox.Upload(ctx, effectivePatchPath, candidateEffectiveContainerPath); err != nil {
		return finish(fmt.Errorf("upload effective candidate patch: %w", err))
	}
	applyResult, applyErr := sandbox.Exec(ctx, gitCommand("apply", candidateEffectiveContainerPath))
	if applyErr != nil {
		return finish(fmt.Errorf("apply effective candidate patch: %w", applyErr))
	}
	if applyResult.ExitCode != 0 {
		for _, artifact := range []string{stdoutPath, stderrPath, outputPath} {
			if err := writePrivateArtifact(artifact, nil); err != nil {
				return finish(fmt.Errorf("initialize skipped verifier artifact: %w", err))
			}
		}
		reason := fmt.Sprintf("candidate patch did not apply: exit code %d", applyResult.ExitCode)
		if strings.TrimSpace(applyResult.Stderr) != "" {
			reason += ": " + strings.TrimSpace(applyResult.Stderr)
		}
		if err := writePrivateArtifact(reasonPath, []byte(reason+"\n")); err != nil {
			return finish(fmt.Errorf("write candidate rejection reason: %w", err))
		}
		return finish(nil)
	}

	if _, err := execOK(ctx, sandbox, "restore non-root verifier worktree access", core.Command{
		Path: "/bin/sh", Args: []string{"-c", restoreAgentWorktreePredicate, "aries-swebenchpro-agent-worktree", repositoryPath, agentExecUser}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}

	for _, file := range []struct {
		name        string
		source      string
		destination string
	}{
		{name: "private verifier snapshot", source: details.snapshot, destination: verifierSnapshotContainerPath},
		{name: "pinned run script", source: details.runScript, destination: runScriptContainerPath},
		{name: "pinned parser", source: details.parser, destination: parserContainerPath},
	} {
		if err := sandbox.Upload(ctx, file.source, file.destination); err != nil {
			return finish(fmt.Errorf("upload %s: %w", file.name, err))
		}
	}
	if _, err := execOK(ctx, sandbox, "prepare symlink-safe verifier destinations", core.Command{
		Path: "/bin/sh", Args: append([]string{"-c", installVerifierPredicate, "aries-swebenchpro-install-verifier", repositoryPath}, details.verifierFiles...), User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "extract private verifier snapshot", core.Command{
		Path: "/bin/tar", Args: []string{"-xf", verifierSnapshotContainerPath, "-C", repositoryPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "protect verifier files from candidate mutation", core.Command{
		Path: "/bin/sh", Args: append([]string{"-c", secureVerifierPredicate, "aries-swebenchpro-protect-verifier", repositoryPath}, details.verifierFiles...), User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "expose only executable evaluator inputs", core.Command{
		Path: "/bin/chmod", Args: []string{"0711", privateContainerPath, evaluationContainerPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "make verifier script read-only executable", core.Command{
		Path: "/bin/chmod", Args: []string{"0555", runScriptContainerPath}, User: rootExecUser,
	}); err != nil {
		return finish(err)
	}
	if _, err := execOK(ctx, sandbox, "make parser root-only", core.Command{
		Path: "/bin/chmod", Args: []string{"0500", parserContainerPath}, User: rootExecUser,
	}); err != nil {
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
		Dir: repositoryPath, Env: map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}, Timeout: defaultVerifierTimeout, User: agentExecUser,
		OutputLimitBytes: maxVerifierLogSize,
	}, nil, stdoutFile, stderrFile)
	testErr = errors.Join(testErr, stdoutFile.Close(), stderrFile.Close())
	quiesceErr := quiesceAgentProcesses(ctx, sandbox)
	if testErr != nil || quiesceErr != nil {
		return finish(fmt.Errorf("run SWE-bench Pro verifier: %w", errors.Join(testErr, quiesceErr)))
	}
	if err := sandbox.Upload(ctx, stdoutPath, verifierStdoutContainerPath); err != nil {
		return finish(fmt.Errorf("upload verifier stdout for parser: %w", err))
	}
	if err := sandbox.Upload(ctx, stderrPath, verifierStderrContainerPath); err != nil {
		return finish(fmt.Errorf("upload verifier stderr for parser: %w", err))
	}
	parserResult, parserErr := sandbox.Exec(ctx, core.Command{
		Path: "/usr/bin/env",
		Args: []string{"-i", "PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONNOUSERSITE=1", "python", "-I", parserContainerPath, verifierStdoutContainerPath, verifierStderrContainerPath, parserOutputContainerPath},
		Dir:  "/", Timeout: defaultVerifierTimeout, User: rootExecUser,
	})
	if parserErr != nil {
		return finish(fmt.Errorf("run SWE-bench Pro parser: %w", parserErr))
	}
	if parserResult.ExitCode != 0 {
		return finish(fmt.Errorf("run SWE-bench Pro parser: exit code %d", parserResult.ExitCode))
	}
	if err := downloadLimited(ctx, sandbox, parserOutputContainerPath, outputPath, maxParserOutputSize); err != nil {
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
		if err := writePrivateArtifact(reasonPath, []byte("unresolved: missing required passing tests: "+strings.Join(missing, ", ")+"\n")); err != nil {
			return finish(fmt.Errorf("write unresolved reason: %w", err))
		}
		return finish(nil)
	}
	if err := writePrivateArtifact(reasonPath, []byte("resolved: all FAIL_TO_PASS and PASS_TO_PASS tests passed\n")); err != nil {
		return finish(fmt.Errorf("write resolved reason: %w", err))
	}
	evaluation.Score = 1
	evaluation.Reward = 1
	evaluation.Status = core.StatusSucceeded
	evaluation.VerifierStatus = core.StatusSucceeded
	return finish(nil)
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

func requireRegularPrivateFileSize(path, name string, limit int64) error {
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
