package vhive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"golang.org/x/sys/unix"
)

func checkpointFixture(t *testing.T) *Manager {
	t.Helper()
	m := &Manager{options: Options{Namespace: "test", CheckpointDir: t.TempDir(), IdleMode: "checkpoint"}}
	if err := os.MkdirAll(m.runtimeDir("sandbox"), 0700); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCheckpointLocksPreserveConcurrentAdmission(t *testing.T) {
	m := checkpointFixture(t)
	first, err := m.checkpointLock(t.Context(), "sandbox", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockCheckpoint(first)
	second, err := m.checkpointLock(t.Context(), "sandbox", false, true)
	if err != nil {
		t.Fatalf("concurrent reader denied: %v", err)
	}
	defer unlockCheckpoint(second)
	if f, err := m.checkpointLock(t.Context(), "sandbox", true, true); !errors.Is(err, unix.EWOULDBLOCK) {
		unlockCheckpoint(f)
		t.Fatalf("exclusive admission with readers: %v", err)
	}
	unlockCheckpoint(first)
	first = nil
	if f, err := m.checkpointLock(t.Context(), "sandbox", true, true); !errors.Is(err, unix.EWOULDBLOCK) {
		unlockCheckpoint(f)
		t.Fatalf("exclusive admission with remaining reader: %v", err)
	}
	unlockCheckpoint(second)
	second = nil
	exclusive, err := m.checkpointLock(t.Context(), "sandbox", true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockCheckpoint(exclusive)
	if f, err := m.checkpointLock(t.Context(), "sandbox", false, true); !errors.Is(err, unix.EWOULDBLOCK) {
		unlockCheckpoint(f)
		t.Fatalf("reader admitted during checkpoint: %v", err)
	}
}

func TestCheckpointLockWaitHonorsCancellation(t *testing.T) {
	m := checkpointFixture(t)
	held, err := m.checkpointLock(t.Context(), "sandbox", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockCheckpoint(held)
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("tool operation canceled")
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(entered)
		f, err := m.checkpointLock(ctx, "sandbox", false, false)
		unlockCheckpoint(f)
		done <- err
	}()
	<-entered
	cancel(cause)
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lock wait ignored cancellation")
	}
}

func TestCheckpointStateRetainsIdentityAndPrivatePermissions(t *testing.T) {
	m := checkpointFixture(t)
	s := &checkpointState{Phase: "suspended", Request: &proto.CreateVMRequest{VMID: "sandbox"}, Memory: "/private/memory", Snapshot: "/private/state", Processes: []checkpointProcess{{123, "start-one"}, {456, "start-two"}}}
	if err := m.saveCheckpoint("sandbox", s); err != nil {
		t.Fatal(err)
	}
	got, err := m.loadCheckpoint("sandbox")
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("state=%+v error=%v", got, err)
	}
	info, err := os.Stat(filepath.Join(m.runtimeDir("sandbox"), "state.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private state permissions=%v error=%v", info, err)
	}
	s.Request.VMID = "foreign"
	if err := m.saveCheckpoint("sandbox", s); err != nil {
		t.Fatal(err)
	}
	if _, err := m.loadCheckpoint("sandbox"); err == nil {
		t.Fatal("accepted checkpoint for a different VM")
	}
	if _, err := m.loadCheckpoint("../sandbox"); err == nil {
		t.Fatal("accepted traversal identity")
	}
}

func TestCheckpointIncompleteTransitionsCannotRestoreOrExecute(t *testing.T) {
	for _, phase := range []string{"reserved", "pausing", "paused", "stopping", "restoring", "removed", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			m := checkpointFixture(t)
			calls := 0
			m.newVMClient = func() (vmClient, error) { calls++; return nil, errors.New("unexpected native submission") }
			if err := m.saveCheckpoint("sandbox", &checkpointState{Phase: phase, Request: &proto.CreateVMRequest{VMID: "sandbox"}}); err != nil {
				t.Fatal(err)
			}
			if err := m.Resume(t.Context(), "sandbox"); err == nil {
				t.Fatal("incomplete transition restored")
			}
			f, err := m.acquireCheckpoint(t.Context(), "sandbox")
			unlockCheckpoint(f)
			if err == nil {
				t.Fatal("incomplete transition admitted execution")
			}
			if calls != 0 {
				t.Fatalf("submitted %d native calls for incomplete transition", calls)
			}
		})
	}
}

type checkpointMounts struct{ snapshots.Snapshotter }

func (checkpointMounts) Mounts(context.Context, string) ([]mount.Mount, error) {
	return []mount.Mount{{Source: "/dev/mapper/test"}}, nil
}

type checkpointClient struct{ containerClient }

func (checkpointClient) SnapshotService(string) snapshots.Snapshotter { return checkpointMounts{} }

type failedRestoreVM struct {
	vmClient
	calls int
}

func (v *failedRestoreVM) CreateVM(context.Context, *proto.CreateVMRequest) (*proto.CreateVMResponse, error) {
	v.calls++
	return nil, errors.New("response lost")
}

func TestCheckpointAmbiguousRestoreIsNeverReplayed(t *testing.T) {
	m := checkpointFixture(t)
	m.client = checkpointClient{}
	v := &failedRestoreVM{}
	m.vm = v
	// Different recorded start times positively exclude these old identities.
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	s := &checkpointState{Phase: "suspended", Request: &proto.CreateVMRequest{VMID: "sandbox"}, Memory: "memory", Snapshot: "state", Processes: []checkpointProcess{{os.Getpid(), start + "old"}, {os.Getpid(), start + "old"}}}
	if err := m.saveCheckpoint("sandbox", s); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(t.Context(), "sandbox"); err == nil {
		t.Fatal("ambiguous restore succeeded")
	}
	if err := m.Resume(t.Context(), "sandbox"); err == nil {
		t.Fatal("ambiguous restore retried")
	}
	if v.calls != 1 {
		t.Fatalf("restore submissions=%d; want one", v.calls)
	}
	saved, err := m.loadCheckpoint("sandbox")
	if err != nil || saved.Phase != "restoring" {
		t.Fatalf("lost ambiguous ownership: %+v %v", saved, err)
	}
	if len(saved.Processes) != 0 {
		t.Fatalf("failed restore retained stale exited process identities: %+v", saved.Processes)
	}
}

func TestCheckpointProcessStartRejectsInvisibleHostPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if _, err := processStart(pid); err == nil {
			t.Fatalf("accepted invisible host PID %d", pid)
		}
	}
}

func TestCheckpointProcessStartHandlesParenthesisInCommandName(t *testing.T) {
	// Read the kernel start time before renaming the process, then leave the
	// renamed process alive so processStart must parse the complete comm field.
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `awk '{ print $22 }' /proc/$$/stat; printf 'has) space' > /proc/$$/comm; printf 'ready\n'; read stop`)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() {
		t.Fatal("child did not report start time")
	}
	want := lines.Text()
	if !lines.Scan() || lines.Text() != "ready" {
		t.Fatal("child did not rename its command")
	}
	got, err := processStart(cmd.Process.Pid)
	if err != nil || got != want {
		t.Fatalf("renamed process start=%q want=%q error=%v", got, want, err)
	}
}

func TestCheckpointStopFinishesDirectoryRemovalAfterMetadataDeletion(t *testing.T) {
	m := checkpointFixture(t)
	m.client = &fakeClient{}
	if err := m.saveCheckpoint("sandbox", &checkpointState{Phase: "removed", Request: &proto.CreateVMRequest{VMID: "sandbox"}}); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(m.runtimeDir("sandbox"), "snapshot-last")
	if err := os.Mkdir(leftover, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "memory"), []byte("private task state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(t.Context(), "sandbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.runtimeDir("sandbox")); !os.IsNotExist(err) {
		t.Fatalf("checkpoint directory remains after confirmed removal: %v", err)
	}
	if err := m.Stop(t.Context(), "sandbox"); err != nil {
		t.Fatalf("repeat cleanup=%v", err)
	}
}

func TestCheckpointStopPreservesStateWithoutOwnershipMetadata(t *testing.T) {
	for _, phase := range []string{"starting", "running", "suspended", "restoring", "stopping"} {
		t.Run(phase, func(t *testing.T) {
			m := checkpointFixture(t)
			m.client = &fakeClient{}
			if err := m.saveCheckpoint("sandbox", &checkpointState{Phase: phase, Request: &proto.CreateVMRequest{VMID: "sandbox"}}); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(m.runtimeDir("sandbox"), "state.json")
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Stop(t.Context(), "sandbox"); err == nil {
				t.Fatal("removed checkpoint without ownership metadata or confirmed runtime absence")
			}
			after, err := os.ReadFile(statePath)
			if err != nil || string(before) != string(after) {
				t.Fatalf("cleanup lost recovery state: %v", err)
			}
		})
	}
}

type checkpointMissingVM struct {
	*fakeVM
	infos int
}

func (v *checkpointMissingVM) GetVMInfo(context.Context, *proto.GetVMInfoRequest) (*proto.GetVMInfoResponse, error) {
	v.infos++
	return nil, errors.New("native VM info socket unavailable")
}

func TestCheckpointStopFailedBootRequiresNativeConfirmation(t *testing.T) {
	for _, stopFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("stopFails=%v", stopFails), func(t *testing.T) {
			m := checkpointFixture(t)
			m.options.Snapshotter = "devmapper"
			c := ownedContainer()
			snapshots := &fakeSnapshots{}
			m.client = &fakeClient{container: c, snapshots: snapshots}
			vm := &checkpointMissingVM{fakeVM: &fakeVM{}}
			if stopFails {
				vm.stopErr = errors.New("native stop unconfirmed")
			}
			m.vm = vm
			if err := m.saveCheckpoint("sandbox", &checkpointState{Phase: "starting", Request: &proto.CreateVMRequest{VMID: "sandbox"}}); err != nil {
				t.Fatal(err)
			}
			err := m.Stop(t.Context(), "sandbox")
			if vm.infos != 1 || vm.stops != 1 {
				t.Fatalf("native submissions info=%d stop=%d", vm.infos, vm.stops)
			}
			if stopFails {
				if err == nil || c.removed || snapshots.removed {
					t.Fatalf("unconfirmed stop discarded runtime ownership: error=%v metadata=%v disk=%v", err, c.removed, snapshots.removed)
				}
				state, loadErr := m.loadCheckpoint("sandbox")
				if loadErr != nil || state.Phase != "starting" {
					t.Fatalf("lost recovery state: %+v %v", state, loadErr)
				}
				return
			}
			if err != nil || !c.removed || !snapshots.removed {
				t.Fatalf("confirmed stop cleanup failed: error=%v metadata=%v disk=%v", err, c.removed, snapshots.removed)
			}
			if _, err := os.Stat(m.runtimeDir("sandbox")); !os.IsNotExist(err) {
				t.Fatalf("confirmed stop left checkpoint directory: %v", err)
			}
			if err := m.Stop(t.Context(), "sandbox"); err != nil {
				t.Fatal(err)
			}
			if vm.infos != 1 || vm.stops != 1 {
				t.Fatalf("repeat cleanup replayed completed native calls: info=%d stop=%d", vm.infos, vm.stops)
			}
		})
	}
}
