//go:build integration

package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const fixtureImage = "docker.io/library/busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94"

const (
	integrationRunID  = "integration-run"
	integrationTaskID = "integration-task"
)

func TestDockerSandboxRealLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cli := execRunner{binary: defaultDockerBinary}

	if _, err := runChecked(ctx, cli, nil, "info"); err != nil {
		t.Fatalf("Docker daemon is required for integration tests: %v", err)
	}
	ensureFixtureImage(t, ctx, cli)
	assertEmptyM3Inventory(t, ctx, cli)

	outputDir := t.TempDir()
	helperPath := os.Getenv("ARIES_EXEC_HELPER")
	if helperPath == "" {
		t.Fatal("ARIES_EXEC_HELPER must name the statically built integration helper")
	}
	manager, err := New(Options{
		OutputDir:      outputDir,
		ExecHelperPath: helperPath,
		CleanupTimeout: 20 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Start(ctx, core.SandboxRequest{
		RunID:  integrationRunID,
		TaskID: integrationTaskID,
		Environment: core.Environment{
			Image:        fixtureImage,
			Workdir:      "/work",
			CPU:          0.5,
			MemoryMB:     32,
			StorageMB:    64,
			AllowNetwork: false,
			Env:          map[string]string{"TASK_ENV": "task-value"},
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	sandbox := live.(*Sandbox)
	runtimeDir := sandbox.runtimeDir
	t.Cleanup(func() {
		cleanupDockerTest(t, cli, sandbox)
	})

	inspection, err := inspectContainer(ctx, cli, sandbox.ContainerID())
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.State.Running || inspection.Config.WorkingDir != "/work" {
		t.Fatalf("unexpected live inspection: %#v", inspection)
	}
	if inspection.Config.Labels["aries.milestone"] != "m3" || inspection.Config.Labels["aries.kind"] != "task-container" ||
		inspection.Config.Labels["aries.run"] != integrationRunID || inspection.Config.Labels["aries.task"] != integrationTaskID {
		t.Fatalf("missing ARIES container labels: %#v", inspection.Config.Labels)
	}
	network, err := runChecked(ctx, cli, nil, "network", "inspect", "--format", "{{.Internal}} {{index .Labels \"aries.milestone\"}} {{index .Labels \"aries.run\"}} {{index .Labels \"aries.task\"}}", sandbox.NetworkName())
	if err != nil || strings.TrimSpace(string(network.stdout)) != "true m3 "+integrationRunID+" "+integrationTaskID {
		t.Fatalf("network isolation = %q, %v", network.stdout, err)
	}
	hostConfig, err := runChecked(ctx, cli, nil, "container", "inspect", "--format", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{index .HostConfig.StorageOpt \"size\"}} {{len .Mounts}}", sandbox.ContainerID())
	if err != nil || strings.TrimSpace(string(hostConfig.stdout)) != "500000000 33554432 64m 2" {
		t.Fatalf("resource inspection = %q, %v", hostConfig.stdout, err)
	}
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/pwd"}, 0, "/work\n", "")
	environment := execForTest(t, ctx, sandbox, core.Command{
		Path: "/usr/bin/env",
		Env:  map[string]string{"EXEC_ENV": "exec-value"},
	})
	if !strings.Contains(environment.Stdout, "TASK_ENV=task-value\n") || !strings.Contains(environment.Stdout, "EXEC_ENV=exec-value\n") {
		t.Fatalf("exec environment = %q", environment.Stdout)
	}
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/cat", Stdin: []byte("stdin-bytes")}, 0, "stdin-bytes", "")
	assertExec(t, ctx, sandbox, core.Command{
		Path: "/bin/sh",
		Args: []string{"-c", "printf stdout; printf stderr >&2; exit 7"},
	}, 7, "stdout", "stderr")

	uploadSource := filepath.Join(t.TempDir(), "upload.bin")
	wantBytes := []byte{0, 1, 2, 3, 255, '\n'}
	if err := os.WriteFile(uploadSource, wantBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Upload(ctx, uploadSource, "/work/upload.bin"); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	uploaded := execForTest(t, ctx, sandbox, core.Command{Path: "/bin/cat", Args: []string{"/work/upload.bin"}})
	if uploaded.Stdout != string(wantBytes) {
		t.Fatalf("uploaded bytes = %v, want %v", []byte(uploaded.Stdout), wantBytes)
	}
	if got := execForTest(t, ctx, sandbox, core.Command{
		Path:  "/bin/sh",
		Args:  []string{"-c", "cat > /work/download.bin"},
		Stdin: wantBytes,
	}); got.ExitCode != 0 {
		t.Fatalf("write download fixture = %#v", got)
	}
	downloadDestination := filepath.Join(outputDir, "evaluation", "download.bin")
	if err := sandbox.Download(ctx, "/work/download.bin", downloadDestination); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	downloaded, err := os.ReadFile(downloadDestination)
	if err != nil || string(downloaded) != string(wantBytes) {
		t.Fatalf("downloaded bytes = %v, %v", downloaded, err)
	}

	assertExec(t, ctx, sandbox, core.Command{
		Path: "/bin/sh",
		Args: []string{"-c", "printf preserved > /work/restart-state"},
	}, 0, "", "")
	containerIDBeforeRestart := sandbox.ContainerID()
	_, err = sandbox.Exec(ctx, core.Command{Path: "/bin/sleep", Args: []string{"10"}, Timeout: 500 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("timed-out Exec() error = %v, want deadline", err)
	}
	if sandbox.ContainerID() != containerIDBeforeRestart {
		t.Fatalf("exec cleanup replaced container %q with %q", containerIDBeforeRestart, sandbox.ContainerID())
	}
	inspection, err = inspectContainer(ctx, cli, sandbox.ContainerID())
	if err != nil || !inspection.State.Running {
		t.Fatalf("restarted container inspection = %#v, %v", inspection, err)
	}
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/cat", Args: []string{"/work/restart-state"}}, 0, "preserved", "")
	processes := execForTest(t, ctx, sandbox, core.Command{Path: "/bin/ps"})
	if strings.Contains(processes.Stdout, "sleep 10") {
		t.Fatalf("timed-out Docker exec process survived:\n%s", processes.Stdout)
	}
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/true"}, 0, "", "")

	const stopCallers = 8
	var wait sync.WaitGroup
	errorsByCaller := make(chan error, stopCallers)
	for range stopCallers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByCaller <- sandbox.Stop(ctx)
		}()
	}
	wait.Wait()
	close(errorsByCaller)
	for stopErr := range errorsByCaller {
		if stopErr != nil {
			t.Fatalf("concurrent Stop() error = %v", stopErr)
		}
	}
	if err := sandbox.Stop(ctx); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
	if _, err := os.Lstat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private exec runtime still exists: %v", err)
	}
	for _, name := range []string{"container.stdout.log", "container.stderr.log"} {
		info, err := os.Stat(filepath.Join(sandbox.artifactDir, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("log %q mode = %v, %v", name, info, err)
		}
	}
	assertEmptyM3Inventory(t, ctx, cli)
}

func ensureFixtureImage(t *testing.T, ctx context.Context, cli commandRunner) {
	t.Helper()
	if _, err := runChecked(ctx, cli, nil, "image", "inspect", fixtureImage); err == nil {
		return
	}
	if _, err := runChecked(ctx, cli, nil, "pull", fixtureImage); err != nil {
		t.Fatalf("pull digest-pinned integration fixture: %v", err)
	}
}

func assertEmptyM3Inventory(t *testing.T, ctx context.Context, cli commandRunner) {
	t.Helper()
	for _, inventory := range [][]string{
		{"container", "ls", "--all", "--quiet", "--filter", "label=aries.milestone=m3"},
		{"network", "ls", "--quiet", "--filter", "label=aries.milestone=m3"},
	} {
		result, err := runChecked(ctx, cli, nil, inventory...)
		if err != nil {
			t.Fatalf("inventory %v: %v", inventory[:2], err)
		}
		if strings.TrimSpace(string(result.stdout)) != "" {
			t.Fatalf("leaked M3 resources for %v: %s", inventory[:2], result.stdout)
		}
	}
}

func cleanupDockerTest(t *testing.T, cli commandRunner, sandbox *Sandbox) {
	t.Helper()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
	stopErr := sandbox.Stop(cleanupCtx)
	cleanupCancel()
	if stopErr != nil {
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, containerErr := runChecked(recoveryCtx, cli, nil, "container", "rm", "--force", sandbox.ContainerID())
		if isNotFoundError(containerErr) {
			containerErr = nil
		}
		_, networkErr := runChecked(recoveryCtx, cli, nil, "network", "rm", sandbox.NetworkName())
		if isNotFoundError(networkErr) {
			networkErr = nil
		}
		recoveryCancel()
		_ = os.RemoveAll(sandbox.runtimeDir)
		if containerErr != nil || networkErr != nil {
			t.Errorf("emergency cleanup after Stop failure: container=%v network=%v", containerErr, networkErr)
		}
		t.Errorf("cleanup Stop() error = %v", stopErr)
	}
	inventoryCtx, inventoryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer inventoryCancel()
	assertEmptyM3Inventory(t, inventoryCtx, cli)
}

func execForTest(t *testing.T, ctx context.Context, sandbox *Sandbox, command core.Command) core.CommandResult {
	t.Helper()
	result, err := sandbox.Exec(ctx, command)
	if err != nil {
		t.Fatalf("Exec(%s) error = %v", command.Path, err)
	}
	return result
}

func assertExec(t *testing.T, ctx context.Context, sandbox *Sandbox, command core.Command, exitCode int, stdout, stderr string) {
	t.Helper()
	result := execForTest(t, ctx, sandbox, command)
	if result.ExitCode != exitCode || result.Stdout != stdout || result.Stderr != stderr || result.Duration <= 0 {
		t.Fatalf("Exec(%s) = %#v, want exit=%d stdout=%q stderr=%q", command.Path, result, exitCode, stdout, stderr)
	}
}

func TestFixtureReferenceIsImmutable(t *testing.T) {
	if err := validateImmutableImage(fixtureImage); err != nil {
		t.Fatalf("fixture image is not immutable: %v", err)
	}
	if !strings.Contains(fixtureImage, "@sha256:") {
		t.Fatalf("fixture image lacks a digest: %s", fixtureImage)
	}
}
