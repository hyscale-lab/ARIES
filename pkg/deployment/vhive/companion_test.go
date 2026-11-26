package vhive

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/oci"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/hyscale-lab/aries/pkg/deployment"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

func TestCompanionAllocationAndCleanup(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "prepared"
		if partial {
			name = "lost-companion-snapshot-response"
		}
		t.Run(name, func(t *testing.T) {
			const id = "sandbox"
			otherID := id + "-companion"
			root := t.TempDir()
			mainTask, otherTask := &fakeTask{}, &fakeTask{}
			main := &fakeContainer{task: mainTask}
			other := &fakeContainer{task: otherTask}
			if partial {
				other.updateErr = errors.New("lost companion snapshot response")
			}
			client := &fakeClient{containers: map[string]*fakeContainer{id: main, otherID: other}}
			vm := &fakeVM{}
			image := "docker.io/library/example@sha256:" + strings.Repeat("a", 64)
			m := &Manager{client: client, vm: vm, options: Options{Namespace: "aries", Snapshotter: "devmapper", IODir: root,
				CheckpointDir: root, IdleMode: "checkpoint", NativeCheckpoint: true, GuestTaskV3: true, CompanionImage: image, Logger: logrus.New()}}
			r := deployment.Request{Name: id, Image: "example:test", Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: ownedContainer().info.Labels}
			got, err := m.Create(t.Context(), r)
			if got != id || (err != nil) != partial {
				t.Fatalf("create=%q error=%v", got, err)
			}
			if main.info.Labels[companionLabel] != image || other.info.Labels[ownerLabel] != id || other.info.Image != image {
				t.Fatalf("main=%#v companion=%#v", main.info, other.info)
			}
			if partial {
				if vm.request != nil || !errors.Is(err, other.updateErr) {
					t.Fatalf("VM booted after partial allocation: %v, %v", vm.request, err)
				}
			} else {
				if vm.request == nil || vm.request.VMID != id || vm.request.ContainerCount != 2 || vm.request.ExitAfterAllTasksDeleted || mainTask.starts != 1 || otherTask.starts != 1 {
					t.Fatalf("VM=%v, main starts=%d, companion starts=%d", vm.request, mainTask.starts, otherTask.starts)
				}
				state, err := m.loadCheckpoint(id)
				if err != nil || state.Phase != "running" || state.Request.ContainerCount != 2 {
					t.Fatalf("state=%#v err=%v", state, err)
				}
			}

			other.deleteErr = errors.New("companion metadata deletion failed")
			if err := m.Stop(t.Context(), id); !errors.Is(err, other.deleteErr) || main.removed {
				t.Fatalf("lost primary ownership after partial removal: %v", err)
			}
			other.deleteErr = nil
			if err := m.Stop(t.Context(), id); err != nil || !main.removed || !other.removed {
				t.Fatalf("retry failed: %v", err)
			}
			if err := m.Stop(t.Context(), id); err != nil || !reflect.DeepEqual(client.snapshots.keys, []string{otherID, otherID, id}) {
				t.Fatalf("removed snapshots=%v, repeated cleanup=%v", client.snapshots.keys, err)
			}
		})
	}
}

func TestCompanionSpecKeepsDistinctCgroupsAndSharedNetwork(t *testing.T) {
	const id = "sandbox"
	m := &Manager{options: Options{Namespace: "aries"}}
	r := deployment.Request{Name: id, Entrypoint: []string{"/bin/sleep"}}
	ids := []string{id, companionID(id)}
	for i, opts := range [][]oci.SpecOpts{sandboxSpecOptions(r), companionSpecOptions(id)} {
		taskID := ids[i]
		spec, err := oci.GenerateSpec(m.ctx(t.Context()), nil, &containers.Container{ID: taskID}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Annotations[vmAnnotation] != id || spec.Linux.CgroupsPath != filepath.Join("/aries", taskID) {
			t.Fatalf("task %q: vm=%q cgroup=%q", taskID, spec.Annotations[vmAnnotation], spec.Linux.CgroupsPath)
		}
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type == specs.NetworkNamespace {
				t.Fatalf("task %q has a private network namespace", taskID)
			}
		}
	}
}

type companionGuest struct {
	taskAPI.TaskService
	status map[string]tasktypes.Status
}

func (g companionGuest) State(_ context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	return &taskAPI.StateResponse{Status: g.status[r.ID]}, nil
}

func TestGuestReadinessRequiresBothPairedTasks(t *testing.T) {
	g := companionGuest{status: map[string]tasktypes.Status{"sandbox": tasktypes.Status_RUNNING}}
	request := &proto.CreateVMRequest{VMID: "sandbox"}
	if ready, err := guestTasksReady(t.Context(), g, request); err != nil || !ready {
		t.Fatalf("single task ready=%v err=%v", ready, err)
	}
	request.ContainerCount = 2
	if ready, err := guestTasksReady(t.Context(), g, request); err != nil || ready {
		t.Fatalf("missing companion ready=%v err=%v", ready, err)
	}
	g.status["sandbox-companion"] = tasktypes.Status_RUNNING
	if ready, err := guestTasksReady(t.Context(), g, request); err != nil || !ready {
		t.Fatalf("paired tasks ready=%v err=%v", ready, err)
	}
}
