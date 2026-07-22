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

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/client"
)

const fixtureImage = "docker.io/library/busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94"

func TestDockerSandboxRealLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	api, err := client.New(client.FromEnv, client.WithUserAgent("aries-sandbox-integration-test/1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Docker daemon is required for integration tests: %v", err)
	}
	ensureFixtureImage(t, ctx, api)

	outputDir := t.TempDir()
	manager, err := New(Options{
		OutputDir: outputDir, CleanupTimeout: 20 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Start(ctx, core.SandboxRequest{
		RunID: "integration-run", TaskID: "integration-task",
		Environment: core.Environment{
			Image: fixtureImage, Workdir: "/work", CPU: 0.5, MemoryMB: 32,
			StorageMB: 64, Env: map[string]string{"TASK_ENV": "task-value"},
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	sandbox := live.(*Sandbox)
	t.Cleanup(func() { _ = manager.Stop(context.Background(), live) })

	inspection, err := api.ContainerInspect(ctx, sandbox.ContainerID(), client.ContainerInspectOptions{})
	if err != nil || inspection.Container.State == nil || !inspection.Container.State.Running {
		t.Fatalf("container inspection = %#v, %v", inspection.Container, err)
	}
	if inspection.Container.Config.WorkingDir != "/work" || inspection.Container.HostConfig.NanoCPUs != 500_000_000 || inspection.Container.HostConfig.Memory != 32<<20 {
		t.Fatalf("container configuration = %#v / %#v", inspection.Container.Config, inspection.Container.HostConfig)
	}
	networkInspection, err := api.NetworkInspect(ctx, sandbox.NetworkName(), client.NetworkInspectOptions{})
	if err != nil || !networkInspection.Network.Internal || networkInspection.Network.Labels["aries.task"] != "integration-task" {
		t.Fatalf("network inspection = %#v, %v", networkInspection.Network, err)
	}
	if gateway, err := sandbox.NetworkGateway(ctx); err != nil || gateway == "" {
		t.Fatalf("NetworkGateway() = %q, %v", gateway, err)
	}

	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/pwd"}, 0, "/work\n", "")
	environment := execForTest(t, ctx, sandbox, core.Command{Path: "/usr/bin/env", Env: map[string]string{"EXEC_ENV": "exec-value", "ARIES_VALID": "value"}})
	if !strings.Contains(environment.Stdout, "TASK_ENV=task-value\n") || !strings.Contains(environment.Stdout, "EXEC_ENV=exec-value\n") || !strings.Contains(environment.Stdout, "ARIES_VALID=value\n") {
		t.Fatalf("exec environment = %q", environment.Stdout)
	}
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/cat", Stdin: []byte("stdin-bytes")}, 0, "stdin-bytes", "")
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/sh", Args: []string{"-c", "printf stdout; printf stderr >&2; exit 7"}}, 7, "stdout", "stderr")

	uploadSource := filepath.Join(t.TempDir(), "upload.bin")
	wantBytes := []byte{0, 1, 2, 3, 255, '\n'}
	if err := os.WriteFile(uploadSource, wantBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Upload(ctx, uploadSource, "/work/upload.bin"); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if got := execForTest(t, ctx, sandbox, core.Command{Path: "/bin/cat", Args: []string{"/work/upload.bin"}}).Stdout; got != string(wantBytes) {
		t.Fatalf("uploaded bytes = %v", []byte(got))
	}
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/sh", Args: []string{"-c", "cat > /work/download.bin"}, Stdin: wantBytes}, 0, "", "")
	downloadDestination := filepath.Join(outputDir, "evaluation", "download.bin")
	if err := sandbox.Download(ctx, "/work/download.bin", downloadDestination); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if downloaded, err := os.ReadFile(downloadDestination); err != nil || string(downloaded) != string(wantBytes) {
		t.Fatalf("downloaded bytes = %v, %v", downloaded, err)
	}

	const callers = 8
	var wait sync.WaitGroup
	errorsByCaller := make(chan error, callers)
	for range callers {
		wait.Add(1)
		go func() { defer wait.Done(); errorsByCaller <- manager.Stop(ctx, live) }()
	}
	wait.Wait()
	close(errorsByCaller)
	for stopErr := range errorsByCaller {
		if stopErr != nil {
			t.Fatalf("concurrent Stop() error = %v", stopErr)
		}
	}
	if _, err := api.ContainerInspect(ctx, sandbox.ContainerID(), client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("container remains after Stop: %v", err)
	}
	if _, err := api.NetworkInspect(ctx, sandbox.NetworkName(), client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("network remains after Stop: %v", err)
	}
	for _, name := range []string{"container.stdout.log", "container.stderr.log"} {
		if info, err := os.Stat(filepath.Join(sandbox.artifactDir, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("log %q = %v, %v", name, info, err)
		}
	}
}

func TestExecCancellationKillsOnlyItsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	api, err := client.New(client.FromEnv, client.WithUserAgent("aries-sandbox-integration-test/1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Docker daemon is required for integration tests: %v", err)
	}
	ensureFixtureImage(t, ctx, api)

	manager, err := New(Options{
		OutputDir: t.TempDir(), CleanupTimeout: 8 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Start(ctx, core.SandboxRequest{
		RunID: "cancel-run", TaskID: "cancel-task",
		Environment: core.Environment{Image: fixtureImage, Workdir: "/work", CPU: 0.5, MemoryMB: 32, StorageMB: 64},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	sandbox := live.(*Sandbox)
	t.Cleanup(func() { _ = manager.Stop(context.Background(), live) })

	before, err := api.ContainerInspect(ctx, sandbox.ContainerID(), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/sh", Args: []string{"-c", "setsid sleep 60 >/dev/null 2>&1 & echo $! > /work/unrelated.pid; printf preserved > /work/unrelated.state"}}, 0, "", "")
	unrelatedPID := strings.TrimSpace(execForTest(t, ctx, sandbox, core.Command{Path: "/bin/cat", Args: []string{"/work/unrelated.pid"}}).Stdout)

	started := time.Now()
	_, err = sandbox.Exec(ctx, core.Command{
		Path:    "/bin/sh",
		Args:    []string{"-c", "echo $$ > /work/canceled.pid; trap '' TERM; while :; do sleep 1; done"},
		Timeout: 400 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out Exec() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("timed-out Exec() returned after %s: %v", elapsed, err)
	}

	after, err := api.ContainerInspect(ctx, sandbox.ContainerID(), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Container.ID != before.Container.ID || after.Container.State.StartedAt != before.Container.State.StartedAt || !after.Container.State.Running {
		t.Fatalf("task container changed across exec cancellation: before=%s/%s after=%s/%s running=%v", before.Container.ID, before.Container.State.StartedAt, after.Container.ID, after.Container.State.StartedAt, after.Container.State.Running)
	}
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/cat", Args: []string{"/work/unrelated.state"}}, 0, "preserved", "")
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/sh", Args: []string{"-c", "test -d /proc/$1", "aries-check", unrelatedPID}}, 0, "", "")
	canceledPID := strings.TrimSpace(execForTest(t, ctx, sandbox, core.Command{Path: "/bin/cat", Args: []string{"/work/canceled.pid"}}).Stdout)
	assertExec(t, ctx, sandbox, core.Command{Path: "/bin/sh", Args: []string{"-c", "test ! -d /proc/$1", "aries-check", canceledPID}}, 0, "", "")

	if err := manager.Stop(ctx, live); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := api.ContainerInspect(ctx, sandbox.ContainerID(), client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("container remains after Stop: %v", err)
	}
	if _, err := api.NetworkInspect(ctx, sandbox.NetworkName(), client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("network remains after Stop: %v", err)
	}
}

func ensureFixtureImage(t *testing.T, ctx context.Context, api *client.Client) {
	t.Helper()
	if _, err := api.ImageInspect(ctx, fixtureImage); err == nil {
		return
	}
	pull, err := api.ImagePull(ctx, fixtureImage, client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull digest-pinned fixture: %v", err)
	}
	defer pull.Close()
	if err := pull.Wait(ctx); err != nil {
		t.Fatalf("wait for fixture pull: %v", err)
	}
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
		t.Fatalf("Exec(%s) = %#v", command.Path, result)
	}
}

func TestFixtureReferenceIsImmutable(t *testing.T) {
	if err := containerimage.Validate(fixtureImage); err != nil {
		t.Fatalf("fixture image is not immutable: %v", err)
	}
}
