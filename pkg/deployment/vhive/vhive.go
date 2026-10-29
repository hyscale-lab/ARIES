// Package vhive runs tool sandboxes using vHive's Firecracker/containerd stack.
// It uses the native VM control and containerd APIs, rather than Knative's
// placeholder container (whose exec endpoint is outside the guest).
package vhive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	fcclient "github.com/firecracker-microvm/firecracker-containerd/firecracker-control/client"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/firecracker-microvm/firecracker-containerd/runtime/firecrackeroci"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	vmAnnotation = "aws.firecracker.vm.id"
	configLabel  = "aries.vhive.request"
	stateLabel   = "aries.vhive.vm-state"
	runtimeName  = "aws.firecracker"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

type Options struct {
	Socket, Namespace, Snapshotter, IODir, CNINetwork string
	CNIBinPaths                                       []string
	CNIConfDir, CNICacheDir                           string
	VCPUs, MemoryMB                                   int
	// ImageArchive supplies the prepared image (including local benchmark builds).
	// The provider imports it into its own containerd namespace.
	ImageArchive func(context.Context, string) (io.ReadCloser, error)
	Logger       *logrus.Logger
}

// ExecutionOptions is the non-secret connection contract handed to the bridge.
type ExecutionOptions struct{ Socket, Namespace, IODir string }

type vmClient interface {
	CreateVM(context.Context, *proto.CreateVMRequest) (*proto.CreateVMResponse, error)
	GetVMInfo(context.Context, *proto.GetVMInfoRequest) (*proto.GetVMInfoResponse, error)
	StopVM(context.Context, *proto.StopVMRequest) (*emptypb.Empty, error)
	Close() error
}

type containerClient interface {
	NewContainer(context.Context, string, ...containerd.NewContainerOpts) (containerd.Container, error)
	LoadContainer(context.Context, string) (containerd.Container, error)
	GetImage(context.Context, string) (containerd.Image, error)
	Import(context.Context, io.Reader, ...containerd.ImportOpt) ([]images.Image, error)
	SnapshotService(string) snapshots.Snapshotter
	WithLease(context.Context, ...leases.Opt) (context.Context, func(context.Context) error, error)
	Close() error
}

type Manager struct {
	options     Options
	client      containerClient
	mu          sync.Mutex
	vm          vmClient
	newVMClient func() (vmClient, error)
}

var _ deployment.Deployment = (*Manager)(nil)

// New constructs containerd transport. VM control is connected on first use;
// the bridge's borrowed executor needs only containerd and the shared FIFO path.
func New(options Options) (*Manager, error) {
	if options.Socket == "" {
		options.Socket = "/run/firecracker-containerd/containerd.sock"
	}
	if options.Namespace == "" {
		options.Namespace = "aries"
	}
	if options.Snapshotter == "" {
		options.Snapshotter = "devmapper"
	}
	if options.IODir == "" {
		options.IODir = "/run/firecracker-containerd/aries-io"
	}
	if options.VCPUs == 0 {
		options.VCPUs = 1
	}
	if options.MemoryMB == 0 {
		options.MemoryMB = 1024
	}
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	client, err := containerd.New(options.Socket, containerd.WithDefaultNamespace(options.Namespace))
	if err != nil {
		return nil, err
	}
	return &Manager{options: options, client: client, newVMClient: func() (vmClient, error) {
		return fcclient.New(options.Socket + ".ttrpc")
	}}, nil
}

func validateOptions(o Options) error {
	for _, p := range []string{o.Socket, o.IODir} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsRune(p, 0) {
			return errors.New("vHive socket and IO directory must be clean absolute paths")
		}
	}
	if !identifier.MatchString(o.Namespace) || o.CNINetwork != "" && !identifier.MatchString(o.CNINetwork) {
		return errors.New("vHive namespace and CNI network must be identifiers")
	}
	for _, value := range append(append([]string{}, o.CNIBinPaths...), o.CNIConfDir, o.CNICacheDir) {
		if value != "" && (!filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" || strings.ContainsRune(value, 0)) {
			return errors.New("vHive CNI paths must be clean absolute paths")
		}
	}
	if o.Snapshotter != "devmapper" {
		return errors.New("vHive supports the devmapper snapshotter only")
	}
	if o.VCPUs < 1 || o.VCPUs > 32 || o.MemoryMB < 128 || uint64(o.MemoryMB) > 1<<32-1 {
		return errors.New("invalid vHive VM capacity")
	}
	return nil
}

func (m *Manager) vmControl() (vmClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vm == nil {
		var err error
		m.vm, err = m.newVMClient()
		if err != nil {
			return nil, fmt.Errorf("connect vHive VM control: %w", err)
		}
	}
	return m.vm, nil
}

func (m *Manager) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, m.options.Namespace)
}

func (m *Manager) validateRequest(r deployment.Request) error {
	if !identifier.MatchString(r.Name) || r.Image == "" {
		return errors.New("vHive requires a runtime name and image")
	}
	if r.Labels["aries.managed"] != "true" || r.Labels["aries.kind"] != "task-container" || r.Labels["aries.run"] == "" || r.Labels["aries.task"] == "" {
		return errors.New("vHive requires sandbox ownership labels")
	}
	if len(r.Mounts) != 0 || len(r.NetworkAliases) != 0 || r.ServicePort != 0 || r.InternalPort != 0 || r.GPUs != 0 || r.StorageMB < 0 {
		return errors.New("vHive does not support host mounts, aliases, published services or GPUs; storage must be nonnegative")
	}
	if r.AllowNetwork && m.options.CNINetwork == "" {
		return errors.New("network-enabled vHive task requires cni_network")
	}
	if r.CPU != nil && (*r.CPU < 0.01 || math.IsNaN(*r.CPU) || math.IsInf(*r.CPU, 0) || *r.CPU > 32) {
		return errors.New("vHive CPU must be finite and in [0.01,32] for the native CFS quota")
	}
	if r.MemoryMB != nil && (*r.MemoryMB < 128 || uint64(*r.MemoryMB) > 1<<32-1) {
		return errors.New("vHive memory must be at least 128 MiB")
	}
	if len(r.Entrypoint)+len(r.Args) == 0 {
		return errors.New("vHive sandbox requires an explicit command")
	}
	for _, env := range r.Env {
		key, value, ok := strings.Cut(env, "=")
		if !ok || !validEnv(key) || strings.ContainsRune(value, 0) {
			return errors.New("invalid vHive environment entry")
		}
	}
	return nil
}

func (m *Manager) vmRequest(r deployment.Request) *proto.CreateVMRequest {
	cpu, memory := uint32(m.options.VCPUs), uint32(m.options.MemoryMB)
	if r.CPU != nil {
		cpu = uint32(math.Ceil(*r.CPU))
	}
	if r.MemoryMB != nil {
		memory = uint32(*r.MemoryMB)
	}
	request := &proto.CreateVMRequest{VMID: r.Name, TimeoutSeconds: 120,
		MachineCfg: &proto.FirecrackerMachineConfiguration{VcpuCount: cpu, MemSizeMib: memory},
		// Explicit StopVM owns teardown, including partial task creation.
		ExitAfterAllTasksDeleted: false,
	}
	if r.AllowNetwork {
		request.NetworkInterfaces = []*proto.FirecrackerNetworkInterface{{CNIConfig: &proto.CNIConfiguration{NetworkName: m.options.CNINetwork, InterfaceName: "veth0", BinPath: m.options.CNIBinPaths, ConfDir: m.options.CNIConfDir, CacheDir: m.options.CNICacheDir}}}
	}
	return request
}

// Create reserves persistent ownership before booting the VM. Every subsequent
// error returns that identity, so Runner retains responsibility for cleanup.
func (m *Manager) Create(ctx context.Context, r deployment.Request) (_ string, retErr error) {
	if err := m.validateRequest(r); err != nil {
		return "", err
	}
	ctx = m.ctx(ctx)
	image, err := m.image(ctx, r.Image)
	if err != nil {
		return "", err
	}
	ctx, release, err := m.client.WithLease(ctx)
	if err != nil {
		return "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, release(cleanup))
	}()
	encoded, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	labels := maps.Clone(r.Labels)
	labels[configLabel] = string(encoded)
	labels[stateLabel] = "reserved"
	container, err := m.client.NewContainer(ctx, r.Name, containerd.WithImage(image), containerd.WithContainerLabels(labels),
		containerd.WithSnapshotter(m.options.Snapshotter), containerd.WithRuntime(runtimeName, nil))
	if err != nil {
		return m.recoverCreate(ctx, r, err)
	}
	// Reserve metadata before any snapshot side effects. A failed native spec
	// option or lost snapshot response still has an owned cleanup identity.
	if err = container.Update(ctx, containerd.UpdateContainerOpts(containerd.WithNewSnapshot(r.Name, image))); err != nil {
		return r.Name, err
	}
	opts := append([]oci.SpecOpts{oci.WithImageConfig(image)}, sandboxSpecOptions(r)...)
	if err = container.Update(ctx, containerd.UpdateContainerOpts(containerd.WithNewSpec(opts...))); err != nil {
		return r.Name, err
	}
	if err = m.validateStorage(ctx, r.Name, r.StorageMB); err != nil {
		return r.Name, err
	}
	control, err := m.vmControl()
	if err != nil {
		return r.Name, err
	}
	if _, err = container.SetLabels(ctx, map[string]string{stateLabel: "starting"}); err != nil {
		return r.Name, err
	}
	if _, err = control.CreateVM(ctx, m.vmRequest(r)); err != nil {
		return r.Name, fmt.Errorf("boot vHive microVM: %w", err)
	}
	if err = os.MkdirAll(m.options.IODir, 0o700); err != nil {
		return r.Name, err
	}
	// Only the sandbox's idle process runs here; guest exec requires a running
	// container. No harness or bridge processes are deployed by this provider.
	task, err := container.NewTask(ctx, cio.NullIO)
	if err != nil {
		return r.Name, fmt.Errorf("create guest task: %w", err)
	}
	if err = task.Start(ctx); err != nil {
		return r.Name, fmt.Errorf("start idle guest task: %w", err)
	}
	m.options.Logger.WithContext(ctx).WithField("vm", r.Name).Info("vHive microVM created")
	return r.Name, nil
}

func sandboxSpecOptions(r deployment.Request) []oci.SpecOpts {
	opts := []oci.SpecOpts{oci.WithEnv(r.Env),
		oci.WithProcessArgs(append(append([]string{}, r.Entrypoint...), r.Args...)...),
		oci.WithAnnotations(map[string]string{vmAnnotation: r.Name}),
		// "Host" here means the guest VM, never the worker host.
		firecrackeroci.WithVMNetwork,
	}
	if r.Workdir != "" {
		opts = append(opts, oci.WithProcessCwd(r.Workdir))
	}
	if r.NoNewPrivileges {
		opts = append(opts, oci.WithNoNewPrivileges)
	} else {
		opts = append(opts, oci.WithNewPrivileges)
	}
	if r.CPU != nil {
		opts = append(opts, oci.WithCPUCFS(int64(*r.CPU*100000), 100000))
	}
	return opts
}

func (m *Manager) recoverCreate(ctx context.Context, r deployment.Request, cause error) (string, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	c, err := m.client.LoadContainer(cleanup, r.Name)
	if errdefs.IsNotFound(err) {
		return "", cause
	}
	if err != nil {
		return "", errors.Join(cause, deployment.ErrAllocationUnconfirmed, err)
	}
	info, err := c.Info(cleanup)
	if err != nil {
		return "", errors.Join(cause, deployment.ErrAllocationUnconfirmed, err)
	}
	for k, v := range r.Labels {
		if info.Labels[k] != v {
			return "", errors.Join(cause, deployment.ErrAllocationUnconfirmed)
		}
	}
	return r.Name, cause
}

func (m *Manager) image(ctx context.Context, name string) (containerd.Image, error) {
	named, err := imageReference(name)
	if err != nil {
		return nil, err
	}
	// Always import the prepared Docker image when supplied, so a rebuilt local
	// benchmark tag cannot silently use stale containerd content.
	if m.options.ImageArchive != nil {
		archive, err := m.options.ImageArchive(ctx, name)
		if err != nil {
			return nil, err
		}
		_, importErr := m.client.Import(ctx, archive, containerd.WithDigestRef(func(d digest.Digest) string {
			return reference.TrimNamed(named).Name() + "@" + d.String()
		}))
		if err = errors.Join(importErr, archive.Close()); err != nil {
			return nil, fmt.Errorf("import sandbox image: %w", err)
		}
	}
	image, err := m.client.GetImage(ctx, named.String())
	if err != nil {
		return nil, fmt.Errorf("get prepared vHive image %s: %w", name, err)
	}
	if err = image.Unpack(ctx, m.options.Snapshotter); err != nil {
		return nil, fmt.Errorf("unpack vHive image: %w", err)
	}
	return image, nil
}

func imageReference(name string) (reference.Named, error) {
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return nil, err
	}
	if canonical, ok := named.(reference.Canonical); ok {
		return reference.WithDigest(reference.TrimNamed(named), canonical.Digest())
	}
	return reference.TagNameOnly(named), nil
}

// Devmapper uses a fixed device size configured on the worker. Validate the
// actual native block device instead of silently dropping a benchmark limit.
// A mismatched task is rejected before its VM boots.
func (m *Manager) validateStorage(ctx context.Context, id string, mb int) error {
	if mb == 0 {
		return nil
	}
	mounts, err := m.client.SnapshotService(m.options.Snapshotter).Mounts(ctx, id)
	if err != nil {
		return err
	}
	if len(mounts) != 1 {
		return errors.New("vHive requires a single devmapper block device")
	}
	f, err := os.Open(mounts[0].Source)
	if err != nil {
		return fmt.Errorf("inspect vHive snapshot capacity: %w", err)
	}
	defer f.Close()
	var size uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&size))); errno != 0 {
		return fmt.Errorf("read vHive block device size: %w", errno)
	}
	if size != uint64(mb)*1024*1024 {
		return fmt.Errorf("vHive snapshot capacity is %d bytes; benchmark requires %d MiB; configure worker devmapper base_image_size", size, mb)
	}
	return nil
}

func (m *Manager) Validate(ctx context.Context, id string, r deployment.Request, secrets [][]byte) error {
	if err := m.validateRequest(r); err != nil {
		return err
	}
	if id != r.Name {
		return errors.New("vHive runtime identity differs")
	}
	ctx = m.ctx(ctx)
	c, err := m.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	if info.ID != id || info.Runtime.Name != runtimeName || info.SnapshotKey != id || info.Snapshotter != m.options.Snapshotter {
		return errors.New("vHive runtime or snapshot identity differs")
	}
	named, err := imageReference(r.Image)
	if err != nil || info.Image != named.String() {
		return errors.New("vHive image identity differs")
	}
	if err = m.validateStorage(ctx, id, r.StorageMB); err != nil {
		return err
	}
	for k, v := range r.Labels {
		if info.Labels[k] != v {
			return fmt.Errorf("vHive ownership label %s differs", k)
		}
	}
	var saved deployment.Request
	if err = json.Unmarshal([]byte(info.Labels[configLabel]), &saved); err != nil || !reflect.DeepEqual(saved, r) {
		return errors.New("vHive allocation request differs")
	}
	spec, err := c.Spec(ctx)
	if err != nil {
		return err
	}
	if spec.Annotations[vmAnnotation] != id || spec.Process == nil || !reflect.DeepEqual(spec.Process.Args, append(append([]string{}, r.Entrypoint...), r.Args...)) {
		return errors.New("vHive guest binding or command differs")
	}
	if r.Workdir != "" && spec.Process.Cwd != r.Workdir || spec.Process.NoNewPrivileges != r.NoNewPrivileges {
		return errors.New("vHive workdir or privilege policy differs")
	}
	if spec.Root == nil || spec.Root.Path != "rootfs" || spec.Linux == nil || len(spec.Linux.Devices) != 0 {
		return errors.New("vHive guest root or device policy differs")
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Path != "" {
			return errors.New("vHive guest cannot join an external namespace")
		}
	}
	for _, mount := range spec.Mounts {
		switch mount.Type {
		case "proc", "tmpfs", "devpts", "mqueue", "sysfs", "cgroup", "cgroup2":
		case "bind":
			if !((mount.Source == "/etc/hosts" || mount.Source == "/etc/resolv.conf") && mount.Source == mount.Destination) {
				return errors.New("vHive guest has an unexpected bind mount")
			}
		default:
			return errors.New("vHive guest has an unexpected filesystem mount")
		}
	}
	for _, entry := range r.Env {
		key, value, _ := strings.Cut(entry, "=")
		actual := ""
		found := false
		for _, e := range spec.Process.Env {
			k, v, _ := strings.Cut(e, "=")
			if k == key {
				actual = v
				found = true
			}
		}
		if !found || actual != value {
			return errors.New("vHive guest environment differs")
		}
	}
	metadata, err := json.Marshal([]any{info, spec})
	if err != nil {
		return err
	}
	for _, secret := range secrets {
		if len(secret) != 0 && bytes.Contains(metadata, secret) {
			return errors.New("secret found in vHive metadata")
		}
	}
	control, err := m.vmControl()
	if err != nil {
		return err
	}
	vm, err := control.GetVMInfo(ctx, &proto.GetVMInfoRequest{VMID: id})
	if err != nil {
		return err
	}
	if vm.VMID != id {
		return errors.New("vHive VM identity differs")
	}
	if !r.AllowNetwork {
		// A protobuf repeated field cannot distinguish nil from empty; the
		// worker must have no default NICs. Check inside the guest before access.
		result, err := m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `for p in /sys/class/net/*; do [ "${p##*/}" = lo ] || exit 1; done`}, Timeout: 10 * time.Second})
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return errors.New("network-disabled vHive guest has a network interface; remove worker default_network_interfaces")
		}
	}
	return nil
}

func (m *Manager) Start(ctx context.Context, id string) error {
	running, err := m.Running(ctx, id)
	if err != nil {
		return err
	}
	if !running {
		return errors.New("vHive idle guest task is not running")
	}
	return nil
}

func (m *Manager) Running(ctx context.Context, id string) (bool, error) {
	c, err := m.client.LoadContainer(m.ctx(ctx), id)
	if err != nil {
		return false, err
	}
	task, err := c.Task(m.ctx(ctx), nil)
	if err != nil {
		return false, err
	}
	status, err := task.Status(m.ctx(ctx))
	return status.Status == containerd.Running, err
}

// Stop confirms VM absence before deleting the persistent cleanup identity.
// Failed removals leave metadata intact so a later cleanup can retry safely.
func (m *Manager) Stop(ctx context.Context, id string) error {
	ctx = m.ctx(ctx)
	c, err := m.client.LoadContainer(ctx, id)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	// Metadata is allocated before the VM and removed only after confirmed stop.
	if errdefs.IsNotFound(err) {
		return nil
	}
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	if info.ID != id || info.Runtime.Name != runtimeName || info.Snapshotter != m.options.Snapshotter || info.Labels["aries.managed"] != "true" || info.Labels["aries.kind"] != "task-container" {
		return errors.New("refusing to remove unowned vHive runtime")
	}
	state := info.Labels[stateLabel]
	// Stop and delete the guest task before shutting down its agent. Otherwise
	// native outstanding Wait RPCs keep the agent's graceful shutdown open.
	if state != "stopped" {
		if task, taskErr := c.Task(ctx, nil); taskErr == nil {
			if _, err = task.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
				return err
			}
		} else if !errdefs.IsNotFound(taskErr) {
			return taskErr
		}
	}
	if state != "reserved" && state != "stopped" {
		control, err := m.vmControl()
		if err != nil {
			return err
		}
		if _, err = control.StopVM(ctx, &proto.StopVMRequest{VMID: id, TimeoutSeconds: 10}); err != nil {
			return fmt.Errorf("stop vHive VM (cleanup remains unconfirmed): %w", err)
		}
		if _, err = c.SetLabels(ctx, map[string]string{stateLabel: "stopped"}); err != nil {
			return err
		}
	}
	// In the pinned vHive fork StopVM waits for the VM's shim process to exit.
	// A successful response is the native confirmation; GetVMInfo after removal
	// returns an untyped socket-dial error rather than NotFound.
	// The snapshot name is reserved even if its metadata Update response was
	// lost. Remove it explicitly rather than relying on info.SnapshotKey.
	snapshotter := m.client.SnapshotService(m.options.Snapshotter)
	if err = snapshotter.Remove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if _, err = snapshotter.Stat(ctx, id); !errdefs.IsNotFound(err) {
		if err == nil {
			return errors.New("vHive snapshot remains after removal")
		}
		return err
	}
	if err = c.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if _, err = m.client.LoadContainer(ctx, id); !errdefs.IsNotFound(err) {
		if err == nil {
			return errors.New("vHive container metadata remains after removal")
		}
		return err
	}
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var vmErr error
	if m.vm != nil {
		vmErr = m.vm.Close()
	}
	return errors.Join(vmErr, m.client.Close())
}

// The sandbox's idle task has no log stream; tool output is recorded by exec.
func (m *Manager) Logs(ctx context.Context, id string, _ int) ([]byte, error) {
	_, err := m.client.LoadContainer(m.ctx(ctx), id)
	if errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("vHive logs: %w", runner.ErrNotFound)
	}
	return nil, err
}
func (m *Manager) LogsStream(ctx context.Context, id string, _, _ io.Writer) error {
	_, err := m.Logs(ctx, id, 0)
	return err
}
func (m *Manager) Address(context.Context, string, int) (string, error) {
	return "", errors.New("vHive sandboxes do not publish control services")
}

func (m *Manager) BridgeBackend() string { return "vhive" }
func (m *Manager) ValidateBridgeTarget(ctx context.Context, target core.BridgeTarget) error {
	if target.Backend != "vhive" || target.RuntimeID != target.SandboxID {
		return errors.New("vHive borrowed target identity differs")
	}
	ctx = m.ctx(ctx)
	c, err := m.client.LoadContainer(ctx, target.RuntimeID)
	if err != nil {
		return err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	if info.ID != target.RuntimeID || info.Runtime.Name != runtimeName {
		return errors.New("vHive borrowed runtime differs")
	}
	for k, v := range map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": target.RunID, "aries.task": target.TaskID} {
		if v == "" || info.Labels[k] != v {
			return errors.New("vHive borrowed sandbox ownership differs")
		}
	}
	spec, err := c.Spec(ctx)
	if err != nil {
		return err
	}
	if spec.Annotations[vmAnnotation] != target.RuntimeID {
		return errors.New("vHive borrowed VM binding differs")
	}
	running, err := m.Running(ctx, target.RuntimeID)
	if err != nil {
		return err
	}
	if !running {
		return errors.New("vHive borrowed sandbox is not running")
	}
	return nil
}
