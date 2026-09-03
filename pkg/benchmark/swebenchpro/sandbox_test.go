package swebenchpro

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	t               *testing.T
	calls           []prepareCall
	downloadAt      int
	downloadSources []string
	download        func(string, string) error
	uploads         int
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
	return errors.New("PrepareSandbox must not upload private verifier material")
}

func (s *prepareSandboxFake) Download(_ context.Context, source, destination string) error {
	s.t.Helper()
	s.downloadAt++
	s.downloadSources = append(s.downloadSources, source)
	if s.download == nil {
		s.t.Fatal("unexpected Download")
	}
	return s.download(source, destination)
}

func (s *prepareSandboxFake) DownloadLimit(ctx context.Context, source, destination string, _ int64) error {
	return s.Download(ctx, source, destination)
}

func TestPrepareSandboxSnapshotsVerifierThenPurgesPrivateStateAndHistory(t *testing.T) {
	const (
		taskID     = "instance-001"
		baseCommit = "1111111111111111111111111111111111111111"
		goldCommit = "2222222222222222222222222222222222222222"
		testPatch  = "private verifier patch\n"
	)
	selected := []string{"TestDatabase", "TestUserEmails"}
	verifierFiles := []string{"tests/a_test.py", "tests/new test.py"}
	outputDir := filepath.Join(t.TempDir(), "runs")
	benchmark := &Benchmark{
		outputDir: outputDir,
		details: map[string]taskDetails{taskID: {
			baseCommit: baseCommit, goldCommit: goldCommit, testPatch: testPatch,
			selectedTests: selected, verifierFiles: verifierFiles,
		}},
	}
	snapshotContainer := privateContainerPath + "/verifier-tests.tar"
	snapshotHost := filepath.Join(outputDir, taskID, "private", "verifier-tests.tar")
	ignoredContainer := privateContainerPath + "/ignored-baseline.tar"
	ignoredHost := filepath.Join(outputDir, taskID, "private", "ignored-baseline.tar")
	gitContainer := privateContainerPath + "/git-baseline.tar"
	gitHost := filepath.Join(outputDir, taskID, "private", "git-baseline.tar")
	sandbox := &prepareSandboxFake{t: t, calls: successfulPrepareCalls(baseCommit, goldCommit, testPatch, verifierFiles, snapshotContainer)}
	sandbox.download = func(source, destination string) error {
		switch source {
		case ignoredContainer:
			if destination != ignoredHost {
				t.Fatalf("ignored Download destination = %q, want %q", destination, ignoredHost)
			}
			return os.WriteFile(destination, []byte("ignored build baseline tar"), 0o666)
		case snapshotContainer:
			if destination != snapshotHost {
				t.Fatalf("verifier Download destination = %q, want %q", destination, snapshotHost)
			}
			return os.WriteFile(destination, []byte("private tar"), 0o666)
		case gitContainer:
			if destination != gitHost {
				t.Fatalf("Git Download destination = %q, want %q", destination, gitHost)
			}
			return os.WriteFile(destination, []byte("sanitized git tar"), 0o666)
		default:
			t.Fatalf("unexpected Download source %q", source)
		}
		return nil
	}

	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: taskID}, sandbox); err != nil {
		t.Fatalf("PrepareSandbox() error = %v", err)
	}
	if len(sandbox.calls) != 0 {
		t.Fatalf("%d scripted calls were not consumed", len(sandbox.calls))
	}
	if sandbox.uploads != 0 {
		t.Fatalf("PrepareSandbox made %d uploads", sandbox.uploads)
	}
	if sandbox.downloadAt != 3 {
		t.Fatalf("Download calls = %d, want 3", sandbox.downloadAt)
	}
	if !reflect.DeepEqual(sandbox.downloadSources, []string{prepareIgnoredSnapshotContainerPath, snapshotContainer, prepareGitSnapshotContainerPath}) {
		t.Fatalf("Download sources = %v", sandbox.downloadSources)
	}
	for _, path := range []string{snapshotHost, ignoredHost, gitHost} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Lstat(%q) error = %v", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("snapshot %q mode = %v, want regular 0600", path, info.Mode())
		}
	}
	directoryInfo, err := os.Stat(filepath.Dir(snapshotHost))
	if err != nil {
		t.Fatalf("Stat(private directory) error = %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("private directory mode = %o, want 0700", directoryInfo.Mode().Perm())
	}
	benchmark.mu.RLock()
	gotDetails := benchmark.details[taskID]
	benchmark.mu.RUnlock()
	if gotDetails.snapshot != snapshotHost || gotDetails.ignoredSnapshot != ignoredHost || gotDetails.gitSnapshot != gitHost {
		t.Fatalf("details snapshots = %q / %q / %q", gotDetails.snapshot, gotDetails.ignoredSnapshot, gotDetails.gitSnapshot)
	}
}

func TestPrepareSandboxFailsClosedAndScrubsAfterPrivatePatch(t *testing.T) {
	const (
		taskID     = "instance-001"
		baseCommit = "1111111111111111111111111111111111111111"
		goldCommit = "2222222222222222222222222222222222222222"
		testPatch  = "private verifier patch\n"
	)
	benchmark := &Benchmark{
		outputDir: t.TempDir(),
		details: map[string]taskDetails{taskID: {
			baseCommit: baseCommit, goldCommit: goldCommit, testPatch: testPatch,
			selectedTests: []string{"TestDatabase"}, verifierFiles: []string{"tests/a_test.py"},
		}},
	}
	ignoredHost := filepath.Join(benchmark.outputDir, taskID, "private", ignoredSnapshotFileName)
	sandbox := &prepareSandboxFake{t: t, calls: []prepareCall{
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "reset", "--hard", baseCommit}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "clean", "-fd"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "checkout", "--detach", baseCommit}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "status", "--porcelain=v1"}),
		commandCall("/bin/rm", []string{"-rf", "--", privateContainerPath}),
		commandCall("/bin/sh", []string{"-c", absencePredicate, "aries-swebenchpro-absence", privateContainerPath}),
		commandCall("/bin/mkdir", []string{"-m", "0700", "-p", "--", privateContainerPath}),
		commandCall("/bin/bash", []string{"-o", "pipefail", "-c", ignoredSnapshotPipeline, "aries-swebenchpro-ignored-baseline"}, repositoryPath),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "checkout", goldCommit, "--", "tests/a_test.py"}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "diff", "--name-only", "-z", "HEAD"}, "tests/wrong.py\x00", "", 0),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "ls-files", "--others", "--exclude-standard", "-z"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "reset", "--hard", baseCommit}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "clean", "-ffd"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "clean", "-ffdX"}),
		commandCall("/bin/tar", []string{"-xf", prepareIgnoredSnapshotContainerPath, "-C", repositoryPath}),
		commandCall("/bin/rm", []string{"-rf", "--", privateContainerPath}),
		commandCall("/bin/sh", []string{"-c", absencePredicate, "aries-swebenchpro-absence", privateContainerPath}),
	}}
	sandbox.download = func(source, destination string) error {
		if source != prepareIgnoredSnapshotContainerPath || destination != ignoredHost {
			t.Fatalf("Download(%q, %q)", source, destination)
		}
		return os.WriteFile(destination, []byte("ignored build baseline tar"), 0o600)
	}

	err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: taskID}, sandbox)
	if err == nil || !strings.Contains(err.Error(), "selected verifier files") {
		t.Fatalf("PrepareSandbox() error = %v, want selected verifier files failure", err)
	}
	if len(sandbox.calls) != 0 {
		t.Fatalf("%d scripted cleanup calls were not consumed", len(sandbox.calls))
	}
	benchmark.mu.RLock()
	gotDetails := benchmark.details[taskID]
	benchmark.mu.RUnlock()
	if gotDetails.snapshot != "" || gotDetails.ignoredSnapshot != "" {
		t.Fatalf("failed preparation retained snapshots %q / %q", gotDetails.snapshot, gotDetails.ignoredSnapshot)
	}
	if _, err := os.Lstat(ignoredHost); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed preparation retained ignored baseline: %v", err)
	}
}

func TestParseNULPaths(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []string
		bad    bool
	}{
		{name: "empty", output: ""},
		{name: "paths including space", output: "tests/a.py\x00tests/b test.py\x00", want: []string{"tests/a.py", "tests/b test.py"}},
		{name: "missing final NUL", output: "tests/a.py", bad: true},
		{name: "unsafe path", output: "../secret\x00", bad: true},
		{name: "duplicate", output: "tests/a.py\x00tests/a.py\x00", bad: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseNULPaths(test.output)
			if test.bad {
				if err == nil {
					t.Fatalf("parseNULPaths(%q) = %#v, want error", test.output, got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseNULPaths(%q) = %#v, %v; want %#v", test.output, got, err, test.want)
			}
		})
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

func successfulPrepareCalls(baseCommit, goldCommit, testPatch string, verifierFiles []string, snapshot string) []prepareCall {
	calls := []prepareCall{
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "reset", "--hard", baseCommit}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "clean", "-fd"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "checkout", "--detach", baseCommit}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "status", "--porcelain=v1"}),
		commandCall("/bin/rm", []string{"-rf", "--", privateContainerPath}),
		commandCall("/bin/sh", []string{"-c", absencePredicate, "aries-swebenchpro-absence", privateContainerPath}),
		commandCall("/bin/mkdir", []string{"-m", "0700", "-p", "--", privateContainerPath}),
		commandCall("/bin/bash", []string{"-o", "pipefail", "-c", ignoredSnapshotPipeline, "aries-swebenchpro-ignored-baseline"}, repositoryPath),
		commandCall("/usr/bin/git", append([]string{"-C", repositoryPath, "checkout", goldCommit, "--"}, verifierFiles...)),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "diff", "--name-only", "-z", "HEAD"}, verifierFiles[0]+"\x00", "", 0),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "ls-files", "--others", "--exclude-standard", "-z"}, verifierFiles[1]+"\x00", "", 0),
		commandCall("/bin/sh", append([]string{"-c", verifierFilePredicate, "aries-swebenchpro-verifier-files", repositoryPath}, verifierFiles...)),
		commandCall("/bin/tar", append([]string{"-cf", snapshot, "--"}, verifierFiles...), repositoryPath),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "reset", "--hard", baseCommit}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "clean", "-ffd"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "clean", "-ffdX"}),
		commandCall("/bin/tar", []string{"-xf", prepareIgnoredSnapshotContainerPath, "-C", repositoryPath}),
		commandCall("/bin/rm", []string{"-rf", "--", privateContainerPath}),
		commandCall("/bin/sh", []string{"-c", absencePredicate, "aries-swebenchpro-absence", privateContainerPath}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "remote"}, "origin\n", "", 0),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "remote", "remove", "origin"}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "for-each-ref", "--format=%(refname)"}, "refs/heads/main\nrefs/tags/gold\n", "", 0),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "update-ref", "-d", "refs/heads/main"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "update-ref", "-d", "refs/tags/gold"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "gc", "--prune=now"}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "rev-parse", "--verify", "HEAD"}, baseCommit+"\n", "", 0),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "status", "--porcelain=v1"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "remote"}),
		commandCall("/usr/bin/git", []string{"-C", repositoryPath, "for-each-ref", "--format=%(refname)"}),
		resultCommandCall("/usr/bin/git", []string{"-C", repositoryPath, "cat-file", "-e", goldCommit + "^{commit}"}, "", "", 1),
		commandCall("/bin/mkdir", []string{"-m", "0700", "-p", "--", privateContainerPath}),
		commandCall("/bin/tar", []string{"-cf", prepareGitSnapshotContainerPath, "-C", repositoryPath, ".git"}),
		commandCall("/bin/rm", []string{"-rf", "--", privateContainerPath}),
		commandCall("/bin/sh", []string{"-c", absencePredicate, "aries-swebenchpro-absence", privateContainerPath}),
		commandCall("/bin/chown", []string{"-R", "--", agentExecUser, repositoryPath}),
		{command: core.Command{Path: "/bin/sh", Args: append([]string{"-c", agentBoundaryPredicate, "aries-swebenchpro-agent-boundary", repositoryPath}, trustedRuntimePaths...), User: agentExecUser}},
	}
	return calls
}

func commandCall(path string, args []string, dir ...string) prepareCall {
	command := core.Command{Path: path, Args: args, User: rootExecUser}
	if len(dir) != 0 {
		command.Dir = dir[0]
	}
	return prepareCall{command: command}
}

func stdinCommandCall(path string, args []string, stdin string) prepareCall {
	return prepareCall{command: core.Command{Path: path, Args: args, Stdin: []byte(stdin), User: rootExecUser}}
}

func resultCommandCall(path string, args []string, stdout, stderr string, exitCode int) prepareCall {
	return prepareCall{command: core.Command{Path: path, Args: args, User: rootExecUser}, result: core.CommandResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}}
}
