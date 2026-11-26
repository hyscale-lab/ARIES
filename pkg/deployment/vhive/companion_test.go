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

func TestCompanionSpecKeepsDistinctCgroupsAndPrivateShm(t *testing.T) {
	const id = "sandbox"
	m := &Manager{options: Options{Namespace: "aries"}}
	r := deployment.Request{Name: id, Entrypoint: []string{"/bin/sleep"}}
	for _, task := range []struct {
		id   string
		opts []oci.SpecOpts
		size string
	}{
		{id, sandboxSpecOptions(r), "size=65536k"},
		{companionID(id), companionSpecOptions(id, 0), "size=65536k"},
		{id + "-browser", companionSpecOptions(id, 1024), "size=1048576k"},
	} {
		spec, err := oci.GenerateSpec(m.ctx(t.Context()), nil, &containers.Container{ID: task.id}, task.opts...)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Annotations[vmAnnotation] != id || spec.Linux.CgroupsPath != filepath.Join("/aries", task.id) {
			t.Fatalf("task %q: vm=%q cgroup=%q", task.id, spec.Annotations[vmAnnotation], spec.Linux.CgroupsPath)
		}
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type == specs.NetworkNamespace {
				t.Fatalf("task %q has a private network namespace", task.id)
			}
		}
		var shm []specs.Mount
		for _, mount := range spec.Mounts {
			if mount.Destination == "/dev/shm" {
				shm = append(shm, mount)
			}
		}
		want := []specs.Mount{{Destination: "/dev/shm", Source: "shm", Type: "tmpfs",
			Options: []string{"nosuid", "noexec", "nodev", "mode=1777", task.size}}}
		if !reflect.DeepEqual(shm, want) {
			t.Fatalf("task %q /dev/shm mounts = %#v, want %#v", task.id, shm, want)
		}
		if err := validateCompanionShm(spec, 1024); (err == nil) != (task.id == id+"-browser") {
			t.Fatalf("task %q: unexpected custom /dev/shm validation result %v", task.id, err)
		}
	}
}

type companionGuest struct {
	taskAPI.TaskService
	status map[string]tasktypes.Status
}

func (g companionGuest) State(_ context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	return &taskAPI.StateResponse{ID: r.ID, Status: g.status[r.ID]}, nil
}

func TestGuestReadinessRequiresBothPairedTasks(t *testing.T) {
	g := companionGuest{status: map[string]tasktypes.Status{"sandbox": tasktypes.Status_RUNNING}}
	request := &proto.CreateVMRequest{VMID: "sandbox"}
	if ready, err := guestTasksReady(t.Context(), g, &checkpointState{Request: request}); err != nil || !ready {
		t.Fatalf("single task ready=%v err=%v", ready, err)
	}
	request.ContainerCount = 2
	if ready, err := guestTasksReady(t.Context(), g, &checkpointState{Request: request}); err != nil || ready {
		t.Fatalf("missing companion ready=%v err=%v", ready, err)
	}
	g.status["sandbox-companion"] = tasktypes.Status_RUNNING
	if ready, err := guestTasksReady(t.Context(), g, &checkpointState{Request: request}); err != nil || !ready {
		t.Fatalf("paired tasks ready=%v err=%v", ready, err)
	}
}

func TestExplicitCompanionOptions(t *testing.T) {
	image := "docker.io/library/example@sha256:" + strings.Repeat("a", 64)
	base := Options{NativeCheckpoint: true, GuestTaskV3: true}
	for _, trial := range []struct {
		name   string
		tasks  []CompanionTask
		legacy string
		valid  bool
	}{
		{"two named companions", []CompanionTask{{Role: "warehouse", Image: image}, {Role: "health", Image: image, Command: []string{"node", "-e", "0"}}}, "", true},
		{"duplicate role", []CompanionTask{{Role: "warehouse", Image: image}, {Role: "warehouse", Image: image}}, "", false},
		{"invalid role", []CompanionTask{{Role: "../outside", Image: image}}, "", false},
		{"mutable tag", []CompanionTask{{Role: "health", Image: "example:latest"}}, "", false},
		{"empty argv zero", []CompanionTask{{Role: "health", Image: image, Command: []string{""}}}, "", false},
		{"negative shared memory size", []CompanionTask{{Role: "browser", Image: image, ShmSizeMiB: -1}}, "", false},
		{"conflicting legacy", []CompanionTask{{Role: "health", Image: image}}, image, false},
	} {
		t.Run(trial.name, func(t *testing.T) {
			o := base
			o.Companions, o.CompanionImage = trial.tasks, trial.legacy
			if err := validateCompanionOptions(o); (err == nil) != trial.valid {
				t.Fatalf("expected valid=%v, got %v", trial.valid, err)
			}
		})
	}
}

func TestExplicitCompanionsRetainOwnershipAcrossPartialFailure(t *testing.T) {
	for _, failPreparation := range []bool{false, true} {
		name := "second cleanup fails and retries"
		if failPreparation {
			name = "second snapshot update fails"
		}
		t.Run(name, func(t *testing.T) {
			const id = "sandbox"
			const firstName = id + "-warehouse"
			const secondName = id + "-health"
			image := "docker.io/library/example@sha256:" + strings.Repeat("a", 64)
			mainTask, firstTask, secondTask := &fakeTask{}, &fakeTask{}, &fakeTask{}
			main, first, second := &fakeContainer{task: mainTask}, &fakeContainer{task: firstTask}, &fakeContainer{task: secondTask}
			if failPreparation {
				second.updateErr = errors.New("second snapshot response lost")
			}
			client := &fakeClient{containers: map[string]*fakeContainer{id: main, firstName: first, secondName: second}}
			vm := &fakeVM{}
			root := t.TempDir()
			options := Options{Namespace: "aries", Snapshotter: "devmapper", IODir: root, CheckpointDir: root, IdleMode: "checkpoint", NativeCheckpoint: true,
				GuestTaskV3: true, Logger: logrus.New(), Companions: []CompanionTask{{Role: "warehouse", Image: image}, {Role: "health", Image: image}}}
			m := &Manager{client: client, vm: vm, options: options}
			req := deployment.Request{Name: id, Image: "example:test", Entrypoint: []string{"/bin/sleep"},
				Args: []string{"infinity"}, Labels: ownedContainer().info.Labels}
			runtime, err := m.Create(t.Context(), req)
			if runtime != id || (err != nil) != failPreparation {
				t.Fatalf("create returned %q, %v", runtime, err)
			}
			bindings, readErr := storedCompanions(main.info)
			if readErr != nil || len(bindings) != 2 || first.info.Labels[ownerLabel] != id || second.info.Labels[ownerLabel] != id {
				t.Fatalf("ownership was not reserved for both companions: %v, %v", bindings, readErr)
			}
			if failPreparation {
				if !errors.Is(err, second.updateErr) || vm.request != nil {
					t.Fatalf("VM booted after failed allocation: %v, %v", vm.request, err)
				}
			} else {
				state, err := m.loadCheckpoint(id)
				if err != nil || vm.request.ContainerCount != 3 || mainTask.starts != 1 || firstTask.starts != 1 || secondTask.starts != 1 ||
					!reflect.DeepEqual(state.CompanionIDs, []string{firstName, secondName}) {
					t.Fatalf("three running tasks not persisted: %v, %v", state, err)
				}
			}

			// Removal uses the original allocation labels after a manager restart.
			options.Companions = nil
			restarted := &Manager{client: client, vm: vm, options: options}
			second.info.Labels[ownerLabel] = "different-VM"
			if err := restarted.Stop(t.Context(), id); err == nil || first.removed || main.removed || vm.stops != 0 {
				t.Fatalf("partial cleanup crossed an ownership boundary: %v", err)
			}
			second.info.Labels[ownerLabel] = id
			second.deleteErr = errors.New("second companion metadata deletion failed")
			if err := restarted.Stop(t.Context(), id); !errors.Is(err, second.deleteErr) || !first.removed || main.removed {
				t.Fatalf("partial cleanup did not retain main ownership: %v", err)
			}
			second.deleteErr = nil
			if err := restarted.Stop(t.Context(), id); err != nil || !second.removed || !main.removed {
				t.Fatalf("retry did not finish cleanup: %v", err)
			}
			if err := restarted.Stop(t.Context(), id); err != nil ||
				!reflect.DeepEqual(client.snapshots.keys, []string{firstName, secondName, secondName, id}) {
				t.Fatalf("repeated cleanup=%v, removed snapshots=%v", err, client.snapshots.keys)
			}
		})
	}
}

func TestGuestReadinessRequiresAllExplicitTasks(t *testing.T) {
	checkpoint := &checkpointState{Request: &proto.CreateVMRequest{VMID: "sandbox", ContainerCount: 3},
		CompanionIDs: []string{"sandbox-warehouse", "sandbox-health"}}
	guest := companionGuest{status: map[string]tasktypes.Status{"sandbox": tasktypes.Status_RUNNING, "sandbox-warehouse": tasktypes.Status_RUNNING}}
	if ready, err := guestTasksReady(t.Context(), guest, checkpoint); err != nil || ready {
		t.Fatalf("checkpoint accepted only two running tasks: %v %v", ready, err)
	}
	guest.status["sandbox-health"] = tasktypes.Status_RUNNING
	if ready, err := guestTasksReady(t.Context(), guest, checkpoint); err != nil || !ready {
		t.Fatalf("three running tasks not ready: %v %v", ready, err)
	}
	checkpoint.CompanionIDs[1] = "sandbox-warehouse"
	if ready, err := guestTasksReady(t.Context(), guest, checkpoint); err == nil || ready {
		t.Fatalf("checkpoint accepted a duplicate: %v %v", ready, err)
	}
}
