//go:build integration

package vhive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// This test uses a real worker, no model API. The image must already be imported
// into ARIES's containerd namespace and provide sh, sleep, stat, cat and tar.
func TestMicroVMLifecycleIntegration(t *testing.T) {
	if os.Getenv("ARIES_VHIVE_INTEGRATION") != "1" {
		t.Skip("set ARIES_VHIVE_INTEGRATION=1 on a prepared vHive worker")
	}
	image := os.Getenv("ARIES_VHIVE_IMAGE")
	if image == "" {
		t.Fatal("ARIES_VHIVE_IMAGE must name a prepared containerd image")
	}
	namespace := os.Getenv("ARIES_VHIVE_NAMESPACE")
	if namespace == "" {
		namespace = "aries"
	}
	storageMB := 10240
	if value := os.Getenv("ARIES_VHIVE_STORAGE_MB"); value != "" {
		var err error
		storageMB, err = strconv.Atoi(value)
		if err != nil || storageMB <= 0 {
			t.Fatal("ARIES_VHIVE_STORAGE_MB must be a positive size matching devmapper")
		}
	}
	m, err := New(Options{Socket: os.Getenv("ARIES_VHIVE_SOCKET"), Namespace: namespace, IODir: os.Getenv("ARIES_VHIVE_IO_DIR")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	name := "aries-vhive-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	request := deployment.Request{Name: name, Image: image, Workdir: "/", StorageMB: storageMB, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "integration", "aries.task": "microvm"}}
	create := func(r deployment.Request) string {
		t.Helper()
		id, err := m.Create(ctx, r)
		if id != "" {
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := m.Stop(cleanup, id); err != nil {
					t.Error(err)
				}
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = m.Validate(ctx, id, r, nil); err != nil {
			t.Fatal(err)
		}
		if err = m.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := create(request)
	if err = m.ValidateBridgeTarget(ctx, core.BridgeTarget{Backend: "vhive", RuntimeID: id, SandboxID: id, RunID: "integration", TaskID: "microvm"}); err != nil {
		t.Fatal(err)
	}
	// Write an archive through guest exec, then read its mode/content back.
	var input bytes.Buffer
	tw := tar.NewWriter(&input)
	payload := []byte("persistent candidate state\n")
	if err = tw.WriteHeader(&tar.Header{Name: "aries-state", Mode: 0o640, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err = tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = m.UploadArchive(ctx, id, "/tmp", &input); err != nil {
		t.Fatal(err)
	}
	stream, info, err := m.DownloadArchive(ctx, id, "/tmp/aries-state")
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(stream)
	if _, err = tr.Next(); err != nil {
		_ = stream.Close()
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(tr)
	closeErr := stream.Close()
	if err = errors.Join(readErr, closeErr); err != nil || !bytes.Equal(got, payload) || info.Mode.Perm() != 0o640 {
		t.Fatalf("archive mode=%v content=%q error=%v", info.Mode, got, err)
	}
	result, err := m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `printf '%s' "$ARIES_LITERAL"; printf 'err' >&2; exit 17`}, Env: map[string]string{"ARIES_LITERAL": "a; $(literal)"}})
	if err != nil || result.ExitCode != 17 || result.Stdout != "a; $(literal)" || result.Stderr != "err" {
		t.Fatalf("guest exec=%#v error=%v", result, err)
	}
	// Closing the transport must leave the submitted command alive. Confirm its
	// eventual write from a later, independent guest command.
	commandCtx, cancelCommand := context.WithCancel(ctx)
	defer cancelCommand()
	commandDone := make(chan error, 1)
	go func() {
		_, err := m.Exec(commandCtx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `touch /tmp/aries-started; sleep 2; echo alive >/tmp/aries-after-cancel`}})
		commandDone <- err
	}()
	result, err = m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `for i in 1 2 3 4 5; do test -e /tmp/aries-started && exit 0; sleep 0.1; done; exit 1`}})
	cancelCommand()
	commandErr := <-commandDone
	if err != nil || result.ExitCode != 0 || !errors.Is(commandErr, context.Canceled) {
		t.Fatalf("cancel result=%#v error=%v command error=%v", result, err, commandErr)
	}
	result, err = m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `for i in 1 2 3 4 5; do test -e /tmp/aries-after-cancel && exec cat /tmp/aries-after-cancel; sleep 1; done; exit 1`}})
	if err != nil || result.ExitCode != 0 || result.Stdout != "alive\n" {
		t.Fatalf("canceled command did not survive: %#v %v", result, err)
	}
	// Evaluation gets a fresh VM and image snapshot, not the candidate's writes.
	eval := request
	eval.Name = name + "-eval"
	eval.Labels = map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "evaluation-sandbox", "aries.run": "integration", "aries.task": "microvm"}
	evalID := create(eval)
	if _, _, err = m.DownloadArchive(ctx, evalID, "/tmp/aries-state"); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("evaluation VM inherited candidate state: %v", err)
	}
	if err = m.Stop(ctx, evalID); err != nil {
		t.Fatal(err)
	}
	if err = m.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = m.client.LoadContainer(m.ctx(ctx), id); err == nil {
		t.Fatal(fmt.Sprintf("runtime %s leaked", id))
	}
}
