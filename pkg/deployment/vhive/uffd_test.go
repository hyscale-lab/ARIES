package vhive

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/firecracker-microvm/firecracker-containerd/proto"
)

func TestUFFDRestoreOptionsRequireCheckpointAndSocketPath(t *testing.T) {
	base := Options{Socket: "/run/containerd.sock", IODir: "/run/io", Namespace: "aries", Snapshotter: "devmapper", VCPUs: 2, MemoryMB: 4096, IdleMode: "checkpoint", CheckpointDir: "/var/checkpoints", ShimDir: "/var/shims", RestoreMemorySocket: "/run/page-handler.sock"}
	if err := validateOptions(base); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, path string }{{"persistent", "/run/page-handler.sock"}, {"", "/run/page-handler.sock"}, {"checkpoint", "relative.sock"}, {"checkpoint", "/"}, {"checkpoint", "/run/../page-handler.sock"}, {"checkpoint", "/run/handler\x00.sock"}} {
		t.Run(tc.mode+tc.path, func(t *testing.T) {
			o := base
			o.IdleMode, o.RestoreMemorySocket = tc.mode, tc.path
			if err := validateOptions(o); err == nil {
				t.Fatal("invalid UFFD configuration accepted")
			}
		})
	}
	base.RestoreMemorySocket = ""
	base.IdleMode = "persistent"
	if err := validateOptions(base); err != nil {
		t.Fatalf("default File backend regressed: %v", err)
	}
}

type recordRestoreBackend struct {
	vmClient
	request *proto.CreateVMRequest
	calls   int
}

func (v *recordRestoreBackend) CreateVM(_ context.Context, r *proto.CreateVMRequest) (*proto.CreateVMResponse, error) {
	v.request = r
	v.calls++
	return nil, errors.New("deliberate native restore failure")
}

func TestUFFDNativeRestoreSubmissionAndFailedOwnership(t *testing.T) {
	for _, socket := range []string{"", "/run/owned-handler.sock"} {
		t.Run(socket, func(t *testing.T) {
			m := checkpointFixture(t)
			m.options.RestoreMemorySocket = socket
			m.client = checkpointClient{}
			v := &recordRestoreBackend{}
			m.vm = v
			start, err := processStart(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			state := &checkpointState{Phase: "suspended", Request: &proto.CreateVMRequest{VMID: "sandbox"}, Memory: "/private/memory", ContainerDiskSource: "/dev/mapper/test", Snapshot: "/private/state", Processes: []checkpointProcess{{os.Getpid(), start + "old"}, {os.Getpid(), start + "old"}}}
			if err = m.saveCheckpoint("sandbox", state); err != nil {
				t.Fatal(err)
			}
			if err = m.Resume(t.Context(), "sandbox"); err == nil {
				t.Fatal("failed restore reported success")
			}
			if v.request == nil {
				t.Fatal("native restore was not submitted")
			}
			wantType, wantPath := "File", state.Memory
			if socket != "" {
				wantType, wantPath = "Uffd", socket
			}
			if v.request.MemBackend.GetBackendType() != wantType || v.request.MemBackend.GetBackendPath() != wantPath {
				t.Fatalf("wrong native memory backend: %v", v.request.MemBackend)
			}
			if !v.request.LoadSnapshot || v.request.SnapshotPath != state.Snapshot || v.request.ContainerSnapshotPath != "/dev/mapper/test" {
				t.Fatalf("native restore lost snapshot/disk identity: %v", v.request)
			}
			if err = m.Resume(t.Context(), "sandbox"); err == nil || v.calls != 1 {
				t.Fatalf("ambiguous restore retried: calls=%d err=%v", v.calls, err)
			}
			retained, err := m.loadCheckpoint("sandbox")
			if err != nil || retained.Phase != "restoring" || retained.Memory != state.Memory || retained.Snapshot != state.Snapshot {
				t.Fatalf("lost cleanup ownership: %+v %v", retained, err)
			}
		})
	}
}

func TestUFFDRejectsMissingOrChangedRetainedDiskBeforeSubmission(t *testing.T) {
	for _, source := range []string{"", "/dev/mapper/different"} {
		t.Run(source, func(t *testing.T) {
			m := checkpointFixture(t)
			m.options.RestoreMemorySocket = "/run/owned-handler.sock"
			m.client = checkpointClient{}
			v := &recordRestoreBackend{}
			m.vm = v
			start, err := processStart(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			state := &checkpointState{Phase: "suspended", Request: &proto.CreateVMRequest{VMID: "sandbox"}, Memory: "/private/memory", Snapshot: "/private/state", ContainerDiskSource: source, Processes: []checkpointProcess{{os.Getpid(), start + "old"}, {os.Getpid(), start + "old"}}}
			if err := m.saveCheckpoint("sandbox", state); err != nil {
				t.Fatal(err)
			}
			if err := m.Resume(t.Context(), "sandbox"); err == nil {
				t.Fatal("unsafe disk identity accepted")
			}
			if v.calls != 0 {
				t.Fatal("submitted restore with unverified disk identity")
			}
			retained, err := m.loadCheckpoint("sandbox")
			if err != nil || retained.Phase != "suspended" {
				t.Fatalf("changed ownership before validation: %+v %v", retained, err)
			}
		})
	}
}
