//go:build apptainer

package apptainer

// These tests run real Apptainer instances. Build them where Go is installed
// and run the binary on the target host, which needs only apptainer:
//
//	go test -c -tags apptainer -o apptainer.test ./pkg/deployment/apptainer
//	ARIES_APPTAINER_TEST_IMAGE=/path/to/debian.sif ./apptainer.test -test.v -test.run Real

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const realImage = "aries.test/apptainer:1"

// awkwardValue holds every character a shell might reinterpret.
const awkwardValue = "yes it's \"q\" $HOME \\ `x` *"

func newRealManager(t *testing.T) *Manager {
	t.Helper()
	sif := os.Getenv("ARIES_APPTAINER_TEST_IMAGE")
	if sif == "" {
		t.Skip("ARIES_APPTAINER_TEST_IMAGE names no SIF image")
	}
	images := t.TempDir()
	if err := PullImages(context.Background(), "", images, []string{realImage}, map[string]string{realImage: sif}); err != nil {
		t.Fatalf("unpack the test image: %v", err)
	}
	manager, err := New(Options{StateDir: t.TempDir(), ImageDir: images})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func realRequest(t *testing.T) deployment.Request {
	t.Helper()
	nonce, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	return deployment.Request{
		Name: "aries-test-" + nonce, Image: realImage, Workdir: "/",
		Env: []string{"ARIES_VISIBLE=" + awkwardValue}, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"},
		Labels:    map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run-1", "aries.task": "task-1"},
		Placement: core.RuntimePlacement{AttachmentID: "test"}, Init: true, NoNewPrivileges: true,
	}
}

func TestRealLifecycleExecAndTransfer(t *testing.T) {
	manager := newRealManager(t)
	t.Setenv("ARIES_HOST_ONLY", "must-not-enter")
	ctx := context.Background()
	request := realRequest(t)
	id, err := manager.Create(ctx, request)
	t.Cleanup(func() {
		if err := manager.Stop(context.Background(), id); err != nil {
			t.Errorf("cleanup Stop: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Validate(ctx, id, request, [][]byte{[]byte("must-not-enter")}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := manager.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if running, err := manager.Running(ctx, id); err != nil || !running {
		t.Fatalf("Running = %v, %v", running, err)
	}

	result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `id -u; printf '%s\n' "$ARIES_VISIBLE" "${ARIES_HOST_ONLY:-absent}" "$EXTRA"; pwd`}, Dir: "/tmp", Env: map[string]string{"EXTRA": "x y"}})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Exec = %+v, %v", result, err)
	}
	if want := "0\n" + awkwardValue + "\nabsent\nx y\n/tmp\n"; result.Stdout != want {
		t.Fatalf("Exec stdout = %q, want %q (stderr %q)", result.Stdout, want, result.Stderr)
	}
	if home, err := os.UserHomeDir(); err == nil {
		result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `ls -A "$1" 2>/dev/null | wc -l`, "sh", home}})
		if err != nil || strings.TrimSpace(result.Stdout) != "0" {
			t.Fatalf("host home visible inside: %+v, %v", result, err)
		}
	}
	if result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", "exit 7"}}); err != nil || result.ExitCode != 7 {
		t.Fatalf("Exec exit 7 = %+v, %v", result, err)
	}

	started := time.Now()
	_, err = manager.Exec(ctx, id, core.Command{Path: "/bin/sleep", Args: []string{"300"}, Timeout: time.Second})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 30*time.Second {
		t.Fatalf("timed-out Exec = %v after %s", err, time.Since(started))
	}
	result, err = manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' <"$f"; echo; done | grep -c '^/bin/sleep 300' || true`}})
	if err != nil || strings.TrimSpace(result.Stdout) != "0" {
		t.Fatalf("timed-out command survived: %+v, %v", result, err)
	}

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	content := []byte("staged\n")
	if err := writer.WriteHeader(&tar.Header{Name: "staged.txt", Mode: 0o640, Size: int64(len(content)), Uid: 1000, Gid: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2"}}); err != nil || result.Stdout != "out\n" || result.Stderr != "err\n" {
		t.Fatalf("Exec output = %+v, %v", result, err)
	}
	if err := manager.UploadArchive(ctx, id, "/opt", &archive); err != nil {
		t.Fatalf("UploadArchive: %v", err)
	}
	stream, info, err := manager.DownloadArchive(ctx, id, "/opt/staged.txt")
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	reader := tar.NewReader(stream)
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(reader)
	_ = stream.Close()
	if header.Name != "staged.txt" || !bytes.Equal(downloaded, content) || info.Size != int64(len(content)) || info.Mode.Perm() != 0o640 {
		t.Fatalf("download = %q %q, info %+v", header.Name, downloaded, info)
	}
	if _, _, err := manager.DownloadArchive(ctx, id, "/opt/missing"); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("missing source = %v", err)
	}

	if err := manager.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, found, err := manager.instance(ctx, id); err != nil || found {
		t.Fatalf("instance after Stop: found=%v, %v", found, err)
	}
	if running, err := manager.Running(ctx, id); err != nil || running {
		t.Fatalf("Running after Stop = %v, %v", running, err)
	}
}

func TestRealStorageBoundHolds(t *testing.T) {
	manager := newRealManager(t)
	ctx := context.Background()
	request := realRequest(t)
	request.StorageMB = 64
	id, err := manager.Create(ctx, request)
	t.Cleanup(func() {
		if err := manager.Stop(context.Background(), id); err != nil {
			t.Errorf("cleanup Stop: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", "dd if=/dev/zero of=/opt/fill bs=1M count=128 2>&1; echo status=$?"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Stdout, "No space left") || strings.Contains(result.Stdout, "status=0") {
		t.Fatalf("a 64 MB overlay took 128 MB: %q", result.Stdout)
	}
}
