//go:build integration

package vhive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

func TestMicroVMCheckpointIntegration(t *testing.T) {
	if os.Getenv("ARIES_VHIVE_INTEGRATION") != "1" {
		t.Skip("requires prepared native worker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	checkpointDir := os.Getenv("ARIES_VHIVE_CHECKPOINT_DIR")
	if checkpointDir == "" {
		t.Fatal("ARIES_VHIVE_CHECKPOINT_DIR is required")
	}
	m, err := New(Options{Socket: os.Getenv("ARIES_VHIVE_SOCKET"), Namespace: os.Getenv("ARIES_VHIVE_NAMESPACE"), IODir: os.Getenv("ARIES_VHIVE_IO_DIR"), IdleMode: "checkpoint", CheckpointDir: checkpointDir, ShimDir: os.Getenv("ARIES_VHIVE_SHIM_DIR"), CheckpointAfterExec: true,
		CNINetwork: os.Getenv("ARIES_VHIVE_CNI_NETWORK"), CNIBinPaths: filepath.SplitList(os.Getenv("ARIES_VHIVE_CNI_BIN_PATHS")), CNIConfDir: os.Getenv("ARIES_VHIVE_CNI_CONF_DIR"), CNICacheDir: os.Getenv("ARIES_VHIVE_CNI_CACHE_DIR")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	name := fmt.Sprintf("aries-checkpoint-%d", time.Now().UnixNano())
	r := deployment.Request{Name: name, Image: os.Getenv("ARIES_VHIVE_IMAGE"), StorageMB: 10240, Workdir: "/", Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, AllowNetwork: m.options.CNINetwork != "", Labels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "checkpoint-integration", "aries.task": "microvm"}}
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
	command := func(body string) string {
		t.Helper()
		result, err := m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", body}, Timeout: 30 * time.Second})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("exec result=%#v error=%v", result, err)
		}
		return result.Stdout
	}
	first := command(`cat /proc/sys/kernel/random/boot_id; sleep 600 >/dev/null 2>&1 & echo $! >/tmp/aries-background; echo preserved >/tmp/aries-value; cat /tmp/aries-background; cat /sys/class/net/*/address; ip -4 addr show; ip route`)
	t.Logf("first call guest identity: %s", first)
	state, err := m.loadCheckpoint(id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "suspended" {
		t.Fatalf("VM did not suspend: %s", state.Phase)
	}
	if err = confirmCheckpointProcesses(state.Processes); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{state.Memory, state.Snapshot} {
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 || info.Mode().Perm() != 0600 {
			t.Fatalf("checkpoint file %s: %v %v", path, info, err)
		}
	}
	running, err := m.Running(ctx, id)
	if err != nil || !running {
		t.Fatalf("logical sandbox lost: %v %v", running, err)
	}
	if err = m.ValidateBridgeTarget(ctx, core.BridgeTarget{Backend: "vhive", RuntimeID: id, SandboxID: id, RunID: "checkpoint-integration", TaskID: "microvm"}); err != nil {
		t.Fatal(err)
	}
	second := command(`cat /proc/sys/kernel/random/boot_id; kill -0 $(cat /tmp/aries-background) || exit; test "$(cat /tmp/aries-value)" = preserved || exit; cat /tmp/aries-background; cat /sys/class/net/*/address; ip -4 addr show; ip route`)
	if second != first {
		t.Fatalf("restored identity differs:\nfirst=%s\nsecond=%s", first, second)
	}
	t.Logf("restored call guest identity: %s", second)
	// A third cycle verifies the memory backing of a restored VM can be replaced.
	if got := command(`cat /tmp/aries-value`); strings.TrimSpace(got) != "preserved" {
		t.Fatalf("lost state: %q", got)
	}
	entries, err := os.ReadDir(m.runtimeDir(id))
	if err != nil {
		t.Fatal(err)
	}
	generations := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "snapshot-") {
			generations++
		}
	}
	if generations != 1 {
		t.Fatalf("retained %d checkpoint generations", generations)
	}
	if err = m.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(m.runtimeDir(id)); !os.IsNotExist(err) {
		t.Fatalf("checkpoint files remain: %v", err)
	}

	// A live VMM is not enough to report a usable logical container.
	exited := r
	exited.Name = name + "-exited"
	exited.Args = []string{"1"}
	exitedID, err := m.Create(ctx, exited)
	if exitedID != "" {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := m.Stop(cleanup, exitedID); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		running, err := m.Running(ctx, exitedID)
		if err != nil {
			t.Fatal(err)
		}
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exited guest task reported running")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = m.Suspend(ctx, exitedID); err != nil {
		t.Fatal(err)
	}
	if running, err := m.Running(ctx, exitedID); err != nil || running {
		t.Fatalf("checkpoint resurrected exited task: running=%v error=%v", running, err)
	}
	if err = m.Stop(ctx, exitedID); err != nil {
		t.Fatal(err)
	}
}
