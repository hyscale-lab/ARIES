package swebenchpro

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	absencePredicate                          = `for path do [ ! -e "$path" ] && [ ! -L "$path" ] || exit 1; done`
	snapshotFileName                          = "verifier-tests.tar"
	ignoredSnapshotFileName                   = "ignored-baseline.tar"
	gitSnapshotFileName                       = "git-baseline.tar"
	prepareIgnoredSnapshotContainerPath       = privateContainerPath + "/" + ignoredSnapshotFileName
	prepareGitSnapshotContainerPath           = privateContainerPath + "/" + gitSnapshotFileName
	ignoredSnapshotPipeline                   = `/usr/bin/git ls-files --others --ignored --exclude-standard -z | /bin/tar --null --files-from=- -cf /tmp/aries-swebenchpro/ignored-baseline.tar`
	verifierFilePredicate                     = `root=$1; shift; for relative do current=$root; remaining=$relative; while [ "$remaining" != "${remaining#*/}" ]; do component=${remaining%%/*}; remaining=${remaining#*/}; current=$current/$component; [ -d "$current" ] && [ ! -L "$current" ] || exit 1; done; target=$current/$remaining; [ -f "$target" ] && [ ! -L "$target" ] || exit 1; done`
	agentBoundaryPredicate                    = `workdir=$1; shift; [ -w "$workdir" ] || exit 1; for path do [ ! -e "$path" ] || [ ! -w "$path" ] || exit 1; done`
	maxVerifierSnapshotSize                   = 64 << 20
	maxIgnoredSnapshotSize              int64 = 8 << 30
	maxGitSnapshotSize                  int64 = 8 << 30
)

var trustedRuntimePaths = []string{"/usr/bin/git", "/bin/tar", "/bin/bash", "/bin/sh", "/usr/bin/env", "/usr/bin/python", "/usr/bin/python3", "/usr/local/bin/python", "/usr/local/bin/python3"}

const preparationCleanupTimeout = 30 * time.Second

// PrepareSandbox captures the pinned verifier files privately, restores the
// base worktree, and makes the gold commit unreachable before the harness can
// receive bridge access. Private verifier bytes only cross the sandbox
// boundary through Download; they are never uploaded while the harness runs.
func (b *Benchmark) PrepareSandbox(ctx context.Context, task core.Task, sandbox runner.Sandbox) (returnErr error) {
	if sandbox == nil {
		return errors.New("SWE-bench Pro preparation requires a live sandbox")
	}
	b.mu.RLock()
	details, loaded := b.details[task.ID]
	b.mu.RUnlock()
	if !loaded {
		return fmt.Errorf("SWE-bench Pro task %q was not loaded by Tasks", task.ID)
	}
	if _, ok := sandbox.(runner.LimitedDownloader); !ok {
		return errors.New("SWE-bench Pro preparation requires bounded sandbox downloads")
	}

	snapshotPath, err := filepath.Abs(filepath.Join(b.outputDir, task.ID, "private", snapshotFileName))
	if err != nil {
		return fmt.Errorf("resolve private verifier snapshot path: %w", err)
	}
	ignoredSnapshotPath, err := filepath.Abs(filepath.Join(b.outputDir, task.ID, "private", ignoredSnapshotFileName))
	if err != nil {
		return fmt.Errorf("resolve ignored baseline snapshot path: %w", err)
	}
	gitSnapshotPath, err := filepath.Abs(filepath.Join(b.outputDir, task.ID, "private", gitSnapshotFileName))
	if err != nil {
		return fmt.Errorf("resolve Git baseline snapshot path: %w", err)
	}
	if err := preparePrivateDirectory(filepath.Dir(snapshotPath), snapshotPath, ignoredSnapshotPath, gitSnapshotPath); err != nil {
		return err
	}

	for _, step := range []struct {
		name    string
		command core.Command
	}{
		{"reset repository to base commit", gitCommand("reset", "--hard", details.baseCommit)},
		{"clean repository before verifier capture", gitCommand("clean", "-fd")},
		{"detach repository at base commit", gitCommand("checkout", "--detach", details.baseCommit)},
	} {
		if _, err := execOK(ctx, sandbox, step.name, step.command); err != nil {
			return err
		}
	}
	if err := proveCleanWorktree(ctx, sandbox, "confirm clean base worktree"); err != nil {
		return err
	}
	if err := removeAndProvePrivatePathAbsent(ctx, sandbox); err != nil {
		return err
	}

	needsScrub := true
	privatePresent := false
	defer func() {
		if needsScrub {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), preparationCleanupTimeout)
			cleanupErr := scrubPrivateVerifier(cleanupCtx, sandbox, details.baseCommit)
			cancel()
			if cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("fail-closed verifier scrub: %w", cleanupErr))
			}
		} else if privatePresent {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), preparationCleanupTimeout)
			cleanupErr := removeAndProvePrivatePathAbsent(cleanupCtx, sandbox)
			cancel()
			if cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("fail-closed private staging scrub: %w", cleanupErr))
			}
		}
		if returnErr != nil {
			for name, path := range map[string]string{"private verifier snapshot": snapshotPath, "ignored baseline snapshot": ignoredSnapshotPath, "Git baseline snapshot": gitSnapshotPath} {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					returnErr = errors.Join(returnErr, fmt.Errorf("remove unusable %s: %w", name, err))
				}
			}
		}
	}()

	if _, err := execOK(ctx, sandbox, "create private verifier staging directory", core.Command{
		Path: "/bin/mkdir", Args: []string{"-m", "0700", "-p", "--", privateContainerPath}, User: rootExecUser,
	}); err != nil {
		return err
	}
	privatePresent = true
	if _, err := execOK(ctx, sandbox, "archive ignored build baseline", core.Command{
		Path: "/bin/bash", Args: []string{"-o", "pipefail", "-c", ignoredSnapshotPipeline, "aries-swebenchpro-ignored-baseline"}, Dir: repositoryPath, User: rootExecUser,
	}); err != nil {
		return err
	}
	if err := downloadLimited(ctx, sandbox, prepareIgnoredSnapshotContainerPath, ignoredSnapshotPath, maxIgnoredSnapshotSize); err != nil {
		return fmt.Errorf("download ignored baseline snapshot: %w", err)
	}
	if err := securePrivateSnapshot(ignoredSnapshotPath, "ignored baseline snapshot", maxIgnoredSnapshotSize); err != nil {
		return err
	}

	checkoutArgs := append([]string{"checkout", details.goldCommit, "--"}, details.verifierFiles...)
	if _, err := execOK(ctx, sandbox, "checkout private verifier files", gitCommand(checkoutArgs...)); err != nil {
		return err
	}
	changed, err := execOK(ctx, sandbox, "list tracked private verifier files", gitCommand("diff", "--name-only", "-z", "HEAD"))
	if err != nil {
		return err
	}
	changedPaths, err := parseNULPaths(changed.Stdout)
	if err != nil {
		return fmt.Errorf("validate selected verifier files: %w", err)
	}
	untracked, err := execOK(ctx, sandbox, "list untracked private verifier files", gitCommand("ls-files", "--others", "--exclude-standard", "-z"))
	if err != nil {
		return err
	}
	untrackedPaths, err := parseNULPaths(untracked.Stdout)
	if err != nil {
		return fmt.Errorf("validate selected verifier files: %w", err)
	}
	changedPaths = append(changedPaths, untrackedPaths...)
	if !samePathSet(changedPaths, details.verifierFiles) {
		return fmt.Errorf("selected verifier files changed %v; want exactly %v", changedPaths, details.verifierFiles)
	}

	if _, err := execOK(ctx, sandbox, "prove private verifier files regular", core.Command{
		Path: "/bin/sh", Args: append([]string{"-c", verifierFilePredicate, "aries-swebenchpro-verifier-files", repositoryPath}, details.verifierFiles...), User: rootExecUser,
	}); err != nil {
		return err
	}
	containerSnapshot := privateContainerPath + "/" + snapshotFileName
	if _, err := execOK(ctx, sandbox, "archive private verifier files", core.Command{
		Path: "/bin/tar", Args: append([]string{"-cf", containerSnapshot, "--"}, details.verifierFiles...), Dir: repositoryPath, User: rootExecUser,
	}); err != nil {
		return err
	}
	if err := downloadLimited(ctx, sandbox, containerSnapshot, snapshotPath, maxVerifierSnapshotSize); err != nil {
		return fmt.Errorf("download private verifier snapshot: %w", err)
	}
	if err := securePrivateSnapshot(snapshotPath, "private verifier snapshot", maxVerifierSnapshotSize); err != nil {
		return err
	}

	if err := scrubPrivateVerifier(ctx, sandbox, details.baseCommit); err != nil {
		return err
	}
	needsScrub = false
	privatePresent = false
	if err := purgeRepositoryHistory(ctx, sandbox); err != nil {
		return err
	}
	if err := proveSanitizedRepository(ctx, sandbox, details.baseCommit, details.goldCommit); err != nil {
		return err
	}
	if _, err := execOK(ctx, sandbox, "create private Git baseline staging directory", core.Command{
		Path: "/bin/mkdir", Args: []string{"-m", "0700", "-p", "--", privateContainerPath}, User: rootExecUser,
	}); err != nil {
		return err
	}
	privatePresent = true
	if _, err := execOK(ctx, sandbox, "archive sanitized Git baseline", core.Command{
		Path: "/bin/tar", Args: []string{"-cf", prepareGitSnapshotContainerPath, "-C", repositoryPath, ".git"}, User: rootExecUser,
	}); err != nil {
		return err
	}
	if err := downloadLimited(ctx, sandbox, prepareGitSnapshotContainerPath, gitSnapshotPath, maxGitSnapshotSize); err != nil {
		return fmt.Errorf("download Git baseline snapshot: %w", err)
	}
	if err := securePrivateSnapshot(gitSnapshotPath, "Git baseline snapshot", maxGitSnapshotSize); err != nil {
		return err
	}
	if err := removeAndProvePrivatePathAbsent(ctx, sandbox); err != nil {
		return err
	}
	privatePresent = false
	if _, err := execOK(ctx, sandbox, "transfer repository ownership to agent", core.Command{
		Path: "/bin/chown", Args: []string{"-R", "--", agentExecUser, repositoryPath}, User: rootExecUser,
	}); err != nil {
		return err
	}
	if _, err := execOK(ctx, sandbox, "prove non-root agent boundary", core.Command{
		Path: "/bin/sh", Args: append([]string{"-c", agentBoundaryPredicate, "aries-swebenchpro-agent-boundary", repositoryPath}, trustedRuntimePaths...), User: agentExecUser,
	}); err != nil {
		return err
	}

	b.mu.Lock()
	current, stillLoaded := b.details[task.ID]
	if stillLoaded {
		current.snapshot = snapshotPath
		current.ignoredSnapshot = ignoredSnapshotPath
		current.gitSnapshot = gitSnapshotPath
		b.details[task.ID] = current
	}
	b.mu.Unlock()
	if !stillLoaded {
		return fmt.Errorf("SWE-bench Pro task %q disappeared during preparation", task.ID)
	}
	return nil
}

func samePathSet(left, right []string) bool {
	left = slices.Clone(left)
	right = slices.Clone(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func preparePrivateDirectory(directory string, snapshots ...string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create private verifier artifact directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect private verifier artifact directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private verifier artifact path is not a real directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure private verifier artifact directory: %w", err)
	}
	for _, snapshot := range snapshots {
		if err := os.Remove(snapshot); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale private snapshot %q: %w", snapshot, err)
		}
	}
	return nil
}

func securePrivateSnapshot(snapshot, name string, limit int64) error {
	info, err := os.Lstat(snapshot)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a regular file", name)
	}
	if info.Size() <= 0 || info.Size() > limit {
		return fmt.Errorf("%s must be nonempty and no larger than %d bytes", name, limit)
	}
	if err := os.Chmod(snapshot, 0o600); err != nil {
		return fmt.Errorf("secure %s: %w", name, err)
	}
	return nil
}

func scrubPrivateVerifier(ctx context.Context, sandbox runner.Sandbox, baseCommit string) error {
	var cleanupErrors []error
	for _, step := range []struct {
		name    string
		command core.Command
	}{
		{"restore repository after verifier capture", gitCommand("reset", "--hard", baseCommit)},
		{"clean repository after verifier capture", gitCommand("clean", "-ffd")},
		{"remove ignored changes after verifier capture", gitCommand("clean", "-ffdX")},
		{"restore ignored build baseline", core.Command{Path: "/bin/tar", Args: []string{"-xf", prepareIgnoredSnapshotContainerPath, "-C", repositoryPath}, User: rootExecUser}},
	} {
		if _, err := execOK(ctx, sandbox, step.name, step.command); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if err := removeAndProvePrivatePathAbsent(ctx, sandbox); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func removeAndProvePrivatePathAbsent(ctx context.Context, sandbox runner.Sandbox) error {
	if _, err := execOK(ctx, sandbox, "remove private verifier staging path", core.Command{
		Path: "/bin/rm", Args: []string{"-rf", "--", privateContainerPath}, User: rootExecUser,
	}); err != nil {
		return err
	}
	_, err := execOK(ctx, sandbox, "confirm private verifier staging path absent", core.Command{
		Path: "/bin/sh", Args: []string{"-c", absencePredicate, "aries-swebenchpro-absence", privateContainerPath}, User: rootExecUser,
	})
	return err
}

func purgeRepositoryHistory(ctx context.Context, sandbox runner.Sandbox) error {
	remotes, err := execOK(ctx, sandbox, "list repository remotes", gitCommand("remote"))
	if err != nil {
		return err
	}
	remoteNames, err := parseLines(remotes.Stdout, "remote")
	if err != nil {
		return err
	}
	for _, remote := range remoteNames {
		if _, err := execOK(ctx, sandbox, "remove repository remote", gitCommand("remote", "remove", remote)); err != nil {
			return err
		}
	}

	refs, err := execOK(ctx, sandbox, "list repository refs", gitCommand("for-each-ref", "--format=%(refname)"))
	if err != nil {
		return err
	}
	refNames, err := parseLines(refs.Stdout, "ref")
	if err != nil {
		return err
	}
	for _, ref := range refNames {
		if !strings.HasPrefix(ref, "refs/") {
			return fmt.Errorf("repository emitted unsafe ref %q", ref)
		}
		if _, err := execOK(ctx, sandbox, "delete repository ref", gitCommand("update-ref", "-d", ref)); err != nil {
			return err
		}
	}
	if _, err := execOK(ctx, sandbox, "expire repository reflogs", gitCommand("reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all")); err != nil {
		return err
	}
	_, err = execOK(ctx, sandbox, "prune unreachable repository objects", gitCommand("gc", "--prune=now"))
	return err
}

func proveSanitizedRepository(ctx context.Context, sandbox runner.Sandbox, baseCommit, goldCommit string) error {
	head, err := execOK(ctx, sandbox, "confirm repository HEAD", gitCommand("rev-parse", "--verify", "HEAD"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(head.Stdout) != baseCommit {
		return fmt.Errorf("repository HEAD = %q, want base commit %s", strings.TrimSpace(head.Stdout), baseCommit)
	}
	if err := proveCleanWorktree(ctx, sandbox, "confirm sanitized worktree clean"); err != nil {
		return err
	}
	for _, proof := range []struct {
		name    string
		command core.Command
	}{
		{"confirm repository remotes absent", gitCommand("remote")},
		{"confirm repository refs absent", gitCommand("for-each-ref", "--format=%(refname)")},
	} {
		result, err := execOK(ctx, sandbox, proof.name, proof.command)
		if err != nil {
			return err
		}
		if result.Stdout != "" {
			return fmt.Errorf("%s: unexpected output %q", proof.name, result.Stdout)
		}
	}
	result, err := sandbox.Exec(ctx, gitCommand("cat-file", "-e", goldCommit+"^{commit}"))
	if err != nil {
		return fmt.Errorf("prove gold commit unreachable: %w", err)
	}
	if result.ExitCode == 0 {
		return errors.New("gold commit remains reachable after repository sanitization")
	}
	return nil
}

func proveCleanWorktree(ctx context.Context, sandbox runner.Sandbox, name string) error {
	result, err := execOK(ctx, sandbox, name, gitCommand("status", "--porcelain=v1"))
	if err != nil {
		return err
	}
	if result.Stdout != "" {
		return fmt.Errorf("%s: unexpected changes %q", name, result.Stdout)
	}
	return nil
}

func gitCommand(args ...string) core.Command {
	return core.Command{Path: "/usr/bin/git", Args: append([]string{"-C", repositoryPath}, args...), User: rootExecUser}
}

func downloadLimited(ctx context.Context, sandbox runner.Sandbox, source, destination string, limit int64) error {
	downloader, ok := sandbox.(runner.LimitedDownloader)
	if !ok {
		return errors.New("sandbox does not support bounded downloads")
	}
	return downloader.DownloadLimit(ctx, source, destination, limit)
}

func execOK(ctx context.Context, sandbox runner.Sandbox, name string, command core.Command) (core.CommandResult, error) {
	result, err := sandbox.Exec(ctx, command)
	if err != nil {
		return result, fmt.Errorf("%s: %w", name, err)
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("%s: exit code %d", name, result.ExitCode)
	}
	return result, nil
}

func parseNULPaths(output string) ([]string, error) {
	if output == "" {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, errors.New("Git path output lacks its final NUL")
	}
	parts := strings.Split(output[:len(output)-1], "\x00")
	seen := make(map[string]struct{}, len(parts))
	for _, value := range parts {
		if !safeRepositoryPath(value) {
			return nil, fmt.Errorf("Git emitted unsafe path %q", value)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("Git emitted duplicate path %q", value)
		}
		seen[value] = struct{}{}
	}
	return parts, nil
}

func parseLines(output, kind string) ([]string, error) {
	if output == "" {
		return nil, nil
	}
	if !strings.HasSuffix(output, "\n") {
		return nil, fmt.Errorf("Git %s output lacks its final newline", kind)
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		if line == "" || strings.IndexFunc(line, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
			return nil, fmt.Errorf("Git emitted unsafe %s %q", kind, line)
		}
		if _, duplicate := seen[line]; duplicate {
			return nil, fmt.Errorf("Git emitted duplicate %s %q", kind, line)
		}
		seen[line] = struct{}{}
	}
	return lines, nil
}
