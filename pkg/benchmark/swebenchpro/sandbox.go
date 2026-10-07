package swebenchpro

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const agentBoundaryPredicate = `workdir=$1; shift; [ -w "$workdir" ] || exit 1; for path do [ ! -e "$path" ] || [ ! -w "$path" ] || exit 1; done`

var trustedRuntimePaths = []string{"/usr/bin/git", "/bin/tar", "/bin/bash", "/bin/sh", "/usr/bin/env", "/usr/bin/python", "/usr/bin/python3", "/usr/local/bin/python", "/usr/local/bin/python3"}

// PrepareSandbox restores the row's base worktree with the first three
// commands of before_repo_set_cmd, then makes the gold commit unreachable
// before the harness can receive bridge access. The image's tracked, ignored,
// and submodule state is left as published, as upstream evaluation leaves it.
// Verifier files never enter the agent sandbox; Evaluate checks them out in a
// fresh sandbox.
func (b *Benchmark) PrepareSandbox(ctx context.Context, task core.Task, sandbox runner.Sandbox) error {
	if sandbox == nil {
		return errors.New("SWE-bench Pro preparation requires a live sandbox")
	}
	b.mu.RLock()
	details, loaded := b.details[task.ID]
	b.mu.RUnlock()
	if !loaded {
		return fmt.Errorf("SWE-bench Pro task %q was not loaded by Tasks", task.ID)
	}

	for _, step := range []struct {
		name    string
		command core.Command
	}{
		{"reset repository to base commit", gitCommand("reset", "--hard", details.baseCommit)},
		{"clean repository at base commit", gitCommand("clean", "-fd")},
		{"detach repository at base commit", gitCommand("checkout", "--detach", details.baseCommit)},
	} {
		if _, err := execOK(ctx, sandbox, step.name, step.command); err != nil {
			return err
		}
	}
	if err := purgeRepositoryHistory(ctx, sandbox); err != nil {
		return err
	}
	if err := proveRepositoryAtBase(ctx, sandbox, details.baseCommit); err != nil {
		return err
	}
	if err := proveRepositoryHistoryIsolated(ctx, sandbox, details.goldCommit); err != nil {
		return err
	}
	if _, err := execOK(ctx, sandbox, "transfer repository ownership to agent", core.Command{
		Path: "/bin/chown", Args: []string{"-R", "--", agentExecUser, repositoryPath}, User: rootExecUser,
	}); err != nil {
		return err
	}
	_, err := execOK(ctx, sandbox, "prove non-root agent boundary", core.Command{
		Path: "/bin/sh", Args: append([]string{"-c", agentBoundaryPredicate, "aries-swebenchpro-agent-boundary", repositoryPath}, trustedRuntimePaths...), User: agentExecUser,
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

func proveRepositoryAtBase(ctx context.Context, sandbox runner.Sandbox, baseCommit string) error {
	head, err := execOK(ctx, sandbox, "confirm repository HEAD", gitCommand("rev-parse", "--verify", "HEAD"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(head.Stdout) != baseCommit {
		return fmt.Errorf("repository HEAD = %q, want base commit %s", strings.TrimSpace(head.Stdout), baseCommit)
	}
	return nil
}

func proveRepositoryHistoryIsolated(ctx context.Context, sandbox runner.Sandbox, goldCommit string) error {
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

func gitCommand(args ...string) core.Command {
	return core.Command{Path: "/usr/bin/git", Args: append([]string{"-C", repositoryPath}, args...), User: rootExecUser}
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
