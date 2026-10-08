package swebenchpro

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

type prepareCall struct {
	command core.Command
	result  core.CommandResult
	err     error
}

type prepareSandboxFake struct {
	t         *testing.T
	calls     []prepareCall
	uploads   int
	downloads int
}

func (s *prepareSandboxFake) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	s.t.Helper()
	if len(s.calls) == 0 {
		s.t.Fatalf("unexpected Exec: %#v", command)
	}
	call := s.calls[0]
	s.calls = s.calls[1:]
	if !reflect.DeepEqual(command, call.command) {
		s.t.Fatalf("Exec command = %#v, want %#v", command, call.command)
	}
	return call.result, call.err
}

func (s *prepareSandboxFake) Upload(context.Context, string, string) error {
	s.uploads++
	return errors.New("PrepareSandbox must not upload verifier material")
}

func (s *prepareSandboxFake) Download(context.Context, string, string) error {
	s.downloads++
	return errors.New("PrepareSandbox must not download from the agent sandbox")
}

func (s *prepareSandboxFake) Connectivity() core.HarnessConnectivity {
	return core.HarnessConnectivity{Placement: core.RuntimePlacement{AttachmentID: "test-network"}}
}

const (
	prepareTaskID     = "instance-001"
	prepareBaseCommit = "1111111111111111111111111111111111111111"
	prepareGoldCommit = "2222222222222222222222222222222222222222"
)

func prepareBenchmark() *Benchmark {
	return &Benchmark{details: map[string]taskDetails{prepareTaskID: {
		baseCommit: prepareBaseCommit, goldCommit: prepareGoldCommit,
		selectedTests: []string{"TestDatabase"}, verifierFiles: []string{"tests/a_test.py"},
	}}}
}

func TestPrepareSandboxRestoresBaseAndPurgesHistoryWithoutVerifierMaterial(t *testing.T) {
	benchmark := prepareBenchmark()
	sandbox := &prepareSandboxFake{t: t, calls: successfulPrepareCalls(prepareBaseCommit, prepareGoldCommit)}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: prepareTaskID}, sandbox); err != nil {
		t.Fatalf("PrepareSandbox() error = %v", err)
	}
	if len(sandbox.calls) != 0 {
		t.Fatalf("%d scripted calls were not consumed", len(sandbox.calls))
	}
	if sandbox.uploads != 0 || sandbox.downloads != 0 {
		t.Fatalf("PrepareSandbox transferred files: uploads=%d downloads=%d", sandbox.uploads, sandbox.downloads)
	}
}

func TestPrepareSandboxLeavesImageWorktreeStateUnchecked(t *testing.T) {
	// A dirty submodule or ignored build output is part of the published image;
	// preparation must neither inspect nor archive it.
	for _, call := range successfulPrepareCalls(prepareBaseCommit, prepareGoldCommit) {
		args := strings.Join(call.command.Args, " ")
		if strings.Contains(args, "status") || strings.Contains(args, "--ignored") || call.command.Path == "/bin/tar" {
			t.Fatalf("preparation inspects or archives worktree state: %#v", call.command)
		}
	}
}

func TestPrepareSandboxFailsWhenGoldCommitRemainsReachable(t *testing.T) {
	benchmark := prepareBenchmark()
	calls := successfulPrepareCalls(prepareBaseCommit, prepareGoldCommit)
	calls = calls[:len(calls)-2]
	calls[len(calls)-1].result.ExitCode = 0
	sandbox := &prepareSandboxFake{t: t, calls: calls}
	err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: prepareTaskID}, sandbox)
	if err == nil || !strings.Contains(err.Error(), "gold commit remains reachable") {
		t.Fatalf("PrepareSandbox() error = %v, want reachable gold commit failure", err)
	}
	if len(sandbox.calls) != 0 {
		t.Fatalf("%d scripted calls were not consumed", len(sandbox.calls))
	}
}

func TestPrepareSandboxReportsTheFailingStep(t *testing.T) {
	benchmark := prepareBenchmark()
	calls := successfulPrepareCalls(prepareBaseCommit, prepareGoldCommit)[:1]
	calls[0].result.ExitCode = 128
	sandbox := &prepareSandboxFake{t: t, calls: calls}
	err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: prepareTaskID}, sandbox)
	if err == nil || err.Error() != "reset repository to base commit: exit code 128" {
		t.Fatalf("PrepareSandbox() error = %v", err)
	}
}

func TestPrepareSandboxRequiresLoadedTaskAndLiveSandbox(t *testing.T) {
	benchmark := &Benchmark{details: map[string]taskDetails{}}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "missing"}, nil); err == nil {
		t.Fatal("PrepareSandbox accepted a nil sandbox")
	}
	sandbox := &prepareSandboxFake{t: t}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "missing"}, sandbox); err == nil {
		t.Fatal("PrepareSandbox accepted a task not loaded by Tasks")
	}
}

func successfulPrepareCalls(baseCommit, goldCommit string) []prepareCall {
	return []prepareCall{
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "reset", "--hard", baseCommit}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "clean", "-fd"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "checkout", "--detach", baseCommit}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "remote"}, "origin\n", "", 0),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "remote", "remove", "origin"}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "for-each-ref", "--format=%(refname)"}, "refs/heads/main\nrefs/tags/gold\n", "", 0),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "update-ref", "-d", "refs/heads/main"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "update-ref", "-d", "refs/tags/gold"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "gc", "--prune=now"}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "rev-parse", "--verify", "HEAD"}, baseCommit+"\n", "", 0),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "remote"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "for-each-ref", "--format=%(refname)"}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "cat-file", "-e", goldCommit + "^{commit}"}, "", "", 1),
		commandCall("/bin/chown", []string{"-R", "--", agentExecUser, repositoryPath}),
		{command: core.Command{Path: "/bin/sh", Args: append([]string{"-c", agentBoundaryPredicate, "aries-swebenchpro-agent-boundary", repositoryPath}, trustedRuntimePaths...), User: agentExecUser}},
	}
}

func commandCall(path string, args []string) prepareCall {
	return prepareCall{command: core.Command{Path: path, Args: args, User: rootExecUser}}
}

func resultCommandCall(path string, args []string, stdout, stderr string, exitCode int) prepareCall {
	return prepareCall{command: core.Command{Path: path, Args: args, User: rootExecUser}, result: core.CommandResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}}
}
