package vhive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/identifiers"
	"github.com/containerd/containerd/oci"
	"github.com/distribution/reference"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const companionsLabel = "aries.vhive.companions"

// CompanionTask is a trusted provider configuration, outside the guest tool
// contract. An empty Command uses the digest-qualified image command.
type CompanionTask struct {
	Role       string
	Image      string
	Command    []string
	Hostname   string // Empty uses the guest's default UTS hostname.
	ShmSizeMiB int64  // Zero keeps containerd's default /dev/shm size.
}

type companionBinding struct {
	Role  string `json:"role"`
	Image string `json:"image"`
}

func (b companionBinding) id(vmID string) string { return vmID + "-" + b.Role }

func configuredCompanions(o Options) []CompanionTask {
	if o.CompanionImage != "" {
		return []CompanionTask{{Role: "companion", Image: o.CompanionImage}}
	}
	return o.Companions
}

func validateCompanionOptions(o Options) error {
	if o.CompanionImage != "" && len(o.Companions) != 0 {
		return errors.New("use either the legacy vHive companion image or explicit companion tasks")
	}
	tasks := configuredCompanions(o)
	if len(tasks) == 0 {
		return nil
	}
	if !o.NativeCheckpoint || !o.GuestTaskV3 {
		return errors.New("vHive companions require native checkpoints and guest Task v3")
	}
	_, err := companionBindings(tasks)
	return err
}

func companionBindings(tasks []CompanionTask) ([]companionBinding, error) {
	var bindings []companionBinding
	for _, task := range tasks {
		bindings = append(bindings, companionBinding{Role: task.Role, Image: task.Image})
		if task.ShmSizeMiB < 0 || task.ShmSizeMiB > math.MaxInt64/(1024*1024) {
			return nil, fmt.Errorf("vHive companion %q has an invalid /dev/shm size", task.Role)
		}
		if task.Hostname != "" && (len(task.Hostname) > 64 || identifiers.Validate(task.Hostname) != nil) {
			return nil, fmt.Errorf("vHive companion %q has an invalid hostname", task.Role)
		}
		if len(task.Command) != 0 && task.Command[0] == "" {
			return nil, fmt.Errorf("vHive companion %q has an empty command", task.Role)
		}
		for _, arg := range task.Command {
			if strings.ContainsRune(arg, 0) {
				return nil, fmt.Errorf("vHive companion %q command contains NUL", task.Role)
			}
		}
	}
	if err := normalizeCompanionBindings(bindings); err != nil {
		return nil, err
	}
	return bindings, nil
}

func normalizeCompanionBindings(bindings []companionBinding) error {
	seen := map[string]bool{}
	for i, binding := range bindings {
		if err := identifiers.Validate(binding.Role); err != nil || seen[binding.Role] {
			return fmt.Errorf("invalid or repeated vHive companion role %q", binding.Role)
		}
		seen[binding.Role] = true
		ref, err := imageReference(binding.Image)
		if _, ok := ref.(reference.Canonical); err != nil || !ok {
			return fmt.Errorf("vHive companion %q requires a digest-qualified image", binding.Role)
		}
		bindings[i].Image = ref.String()
	}
	return nil
}

func storedCompanions(info containers.Container) ([]companionBinding, error) {
	legacy, many := info.Labels[companionLabel], info.Labels[companionsLabel]
	var bindings []companionBinding
	switch {
	case legacy != "" && many != "":
		return nil, errors.New("conflicting vHive companion ownership labels")
	case legacy != "":
		bindings = []companionBinding{{Role: "companion", Image: legacy}}
	case many != "":
		if err := json.Unmarshal([]byte(many), &bindings); err != nil || len(bindings) == 0 {
			return nil, errors.New("invalid vHive companion ownership list")
		}
	}
	if err := normalizeCompanionBindings(bindings); err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if err := identifiers.Validate(binding.id(info.ID)); err != nil {
			return nil, fmt.Errorf("invalid vHive companion identity: %w", err)
		}
	}
	return bindings, nil
}

func (m *Manager) prepareCompanion(ctx context.Context, vmID string, binding companionBinding, task CompanionTask, image containerd.Image, labels map[string]string) (containerd.Container, error) {
	id := binding.id(vmID)
	owner := map[string]string{
		ownerLabel: vmID, "aries.managed": "true", "aries.kind": "task-container", "aries.component": "companion",
		"aries.run": labels["aries.run"], "aries.task": labels["aries.task"],
	}
	container, err := m.client.NewContainer(ctx, id, containerd.WithImage(image), containerd.WithContainerLabels(owner),
		containerd.WithSnapshotter(m.options.Snapshotter), containerd.WithRuntime(runtimeName, nil))
	if err != nil {
		return nil, fmt.Errorf("reserve guest companion %s: %w", binding.Role, err)
	}
	opts := companionSpecOptions(vmID, task.ShmSizeMiB)
	if len(task.Command) > 0 {
		opts = append(opts, oci.WithProcessArgs(task.Command...))
	}
	if task.Hostname != "" {
		opts = append(opts, oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.UTSNamespace}), oci.WithHostname(task.Hostname))
	}
	if err = prepareTask(ctx, container, id, image, opts); err != nil {
		return nil, fmt.Errorf("prepare guest companion %s: %w", binding.Role, err)
	}
	return container, nil
}

func validateCompanionShm(spec *specs.Spec, sizeMiB int64) error {
	if sizeMiB == 0 {
		return nil
	}
	want := fmt.Sprintf("size=%dk", sizeMiB*1024)
	for _, mount := range spec.Mounts {
		if mount.Destination == "/dev/shm" && mount.Type == "tmpfs" && mount.Source == "shm" && slices.Contains(mount.Options, want) {
			return nil
		}
	}
	return fmt.Errorf("vHive companion /dev/shm differs from configured size %d MiB", sizeMiB)
}

func (m *Manager) ownedCompanion(ctx context.Context, main containers.Container, binding companionBinding) (containerd.Container, containers.Container, error) {
	id := binding.id(main.ID)
	c, err := m.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, containers.Container{}, err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return nil, containers.Container{}, err
	}
	if info.ID != id || info.Runtime.Name != runtimeName || info.Snapshotter != m.options.Snapshotter ||
		(info.SnapshotKey != "" && info.SnapshotKey != id) || info.Image != binding.Image ||
		info.Labels[ownerLabel] != main.ID || info.Labels["aries.managed"] != "true" ||
		info.Labels["aries.kind"] != "task-container" || info.Labels["aries.component"] != "companion" ||
		info.Labels["aries.run"] != main.Labels["aries.run"] || info.Labels["aries.task"] != main.Labels["aries.task"] {
		return nil, containers.Container{}, fmt.Errorf("vHive companion %s ownership differs", binding.Role)
	}
	return c, info, nil
}

func cloneCompanionTasks(tasks []CompanionTask) []CompanionTask {
	out := slices.Clone(tasks)
	for i := range out {
		out[i].Command = slices.Clone(out[i].Command)
	}
	return out
}
