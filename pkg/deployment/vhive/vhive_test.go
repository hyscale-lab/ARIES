package vhive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/errdefs"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/emptypb"
)

type fakeClient struct {
	containerClient
	container     *fakeContainer
	loadErr       error
	snapshots     *fakeSnapshots
	leaseReleased bool
}

type fakeImage struct{ containerd.Image }

func (fakeImage) Name() string                                                  { return "docker.io/library/example:test" }
func (fakeImage) Unpack(context.Context, string, ...containerd.UnpackOpt) error { return nil }
func (c *fakeClient) GetImage(context.Context, string) (containerd.Image, error) {
	return fakeImage{}, nil
}
func (c *fakeClient) WithLease(ctx context.Context, _ ...leases.Opt) (context.Context, func(context.Context) error, error) {
	return ctx, func(context.Context) error { c.leaseReleased = true; return nil }, nil
}
func (c *fakeClient) NewContainer(ctx context.Context, id string, opts ...containerd.NewContainerOpts) (containerd.Container, error) {
	info := containers.Container{ID: id}
	for _, opt := range opts {
		if err := opt(ctx, nil, &info); err != nil {
			return nil, err
		}
	}
	c.container.info = info
	return c.container, nil
}

type fakeSnapshots struct {
	snapshots.Snapshotter
	removed   bool
	removeErr error
}

func (s *fakeSnapshots) Remove(context.Context, string) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	s.removed = true
	return nil
}
func (s *fakeSnapshots) Stat(context.Context, string) (snapshots.Info, error) {
	if s.removed {
		return snapshots.Info{}, errdefs.ErrNotFound
	}
	return snapshots.Info{Name: "sandbox"}, nil
}
func (c *fakeClient) SnapshotService(string) snapshots.Snapshotter {
	if c.snapshots == nil {
		c.snapshots = &fakeSnapshots{}
	}
	return c.snapshots
}

func (c *fakeClient) LoadContainer(context.Context, string) (containerd.Container, error) {
	if c.loadErr != nil {
		return nil, c.loadErr
	}
	if c.container == nil || c.container.removed {
		return nil, errdefs.ErrNotFound
	}
	return c.container, nil
}

type fakeContainer struct {
	containerd.Container
	info      containers.Container
	spec      *specs.Spec
	task      containerd.Task
	removed   bool
	deleteErr error
	updateErr error
}

func (c *fakeContainer) Update(context.Context, ...containerd.UpdateContainerOpts) error {
	return c.updateErr
}

func (c *fakeContainer) Info(context.Context, ...containerd.InfoOpts) (containers.Container, error) {
	return c.info, nil
}
func (c *fakeContainer) Spec(context.Context) (*specs.Spec, error) { return c.spec, nil }
func (c *fakeContainer) SetLabels(_ context.Context, labels map[string]string) (map[string]string, error) {
	for k, v := range labels {
		c.info.Labels[k] = v
	}
	return c.info.Labels, nil
}
func (c *fakeContainer) Delete(context.Context, ...containerd.DeleteOpts) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}
	c.removed = true
	return nil
}
func (c *fakeContainer) Task(context.Context, cio.Attach) (containerd.Task, error) {
	if c.task == nil {
		return nil, errdefs.ErrNotFound
	}
	return c.task, nil
}

type fakeVM struct {
	vmClient
	stopErr error
	stops   int
	wrongID bool
}

func (v *fakeVM) GetVMInfo(_ context.Context, r *proto.GetVMInfoRequest) (*proto.GetVMInfoResponse, error) {
	id := r.VMID
	if v.wrongID {
		id = "other"
	}
	return &proto.GetVMInfoResponse{VMID: id}, nil
}

func (v *fakeVM) StopVM(context.Context, *proto.StopVMRequest) (*emptypb.Empty, error) {
	v.stops++
	return &emptypb.Empty{}, v.stopErr
}

func ownedContainer() *fakeContainer {
	return &fakeContainer{info: containers.Container{ID: "sandbox", Snapshotter: "devmapper", Runtime: containers.RuntimeInfo{Name: runtimeName}, Labels: map[string]string{
		"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run", "aries.task": "task", stateLabel: "starting",
	}}}
}

func TestStopRetainsOwnershipAndRetriesOnlyUnfinishedRemoval(t *testing.T) {
	c := ownedContainer()
	vm := &fakeVM{stopErr: errors.New("native stop failed")}
	m := &Manager{options: Options{Namespace: "aries", Snapshotter: "devmapper"}, client: &fakeClient{container: c}, vm: vm}
	if err := m.Stop(t.Context(), "sandbox"); err == nil || c.removed {
		t.Fatalf("failed VM stop lost ownership: %v", err)
	}
	vm.stopErr = nil
	c.deleteErr = errors.New("snapshot removal failed")
	if err := m.Stop(t.Context(), "sandbox"); err == nil || c.removed || c.info.Labels[stateLabel] != "stopped" {
		t.Fatalf("failed snapshot cleanup: %v", err)
	}
	c.deleteErr = nil
	if err := m.Stop(t.Context(), "sandbox"); err != nil {
		t.Fatal(err)
	}
	if vm.stops != 2 || !c.removed {
		t.Fatalf("stops=%d removed=%v", vm.stops, c.removed)
	}
	if err := m.Stop(t.Context(), "sandbox"); err != nil || vm.stops != 2 {
		t.Fatalf("repeated removal: %v", err)
	}
}

func TestReservedAllocationDoesNotRequireAStartedVM(t *testing.T) {
	c := ownedContainer()
	c.info.Labels[stateLabel] = "reserved"
	m := &Manager{options: Options{Namespace: "aries", Snapshotter: "devmapper"}, client: &fakeClient{container: c}}
	if err := m.Stop(t.Context(), "sandbox"); err != nil || !c.removed {
		t.Fatalf("reserved allocation cleanup: %v", err)
	}
}

func TestStopRejectsForeignRuntime(t *testing.T) {
	c := ownedContainer()
	c.info.Runtime.Name = "io.containerd.runc.v2"
	vm := &fakeVM{}
	m := &Manager{options: Options{Namespace: "aries", Snapshotter: "devmapper"}, client: &fakeClient{container: c}, vm: vm}
	if err := m.Stop(t.Context(), "sandbox"); err == nil || c.removed || vm.stops != 0 {
		t.Fatalf("removed foreign runtime: %v", err)
	}
}

func TestCreateRecoveryDistinguishesAbsenceOwnershipAndUncertainty(t *testing.T) {
	r := deployment.Request{Name: "sandbox", Labels: map[string]string{"aries.run": "run"}}
	cause := errors.New("lost create response")
	for _, tc := range []struct {
		name      string
		client    *fakeClient
		id        string
		uncertain bool
	}{
		{"absent", &fakeClient{}, "", false},
		{"owned", &fakeClient{container: ownedContainer()}, "sandbox", false},
		{"inspection failed", &fakeClient{loadErr: errors.New("offline")}, "", true},
		{"foreign", &fakeClient{container: &fakeContainer{info: containers.Container{Labels: map[string]string{"aries.run": "other"}}}}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{client: tc.client}
			id, err := m.recoverCreate(t.Context(), r, cause)
			if id != tc.id || !errors.Is(err, cause) || errors.Is(err, deployment.ErrAllocationUnconfirmed) != tc.uncertain {
				t.Fatalf("id=%q error=%v", id, err)
			}
		})
	}
}

func TestFailedSnapshotSetupRetainsAnOwnedCleanupIdentity(t *testing.T) {
	c := &fakeContainer{updateErr: errors.New("lost snapshot setup response")}
	client := &fakeClient{container: c, snapshots: &fakeSnapshots{}}
	m := &Manager{options: Options{Namespace: "aries", Snapshotter: "devmapper"}, client: client}
	r := deployment.Request{Name: "sandbox", Image: "example:test", Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: ownedContainer().info.Labels}
	id, err := m.Create(t.Context(), r)
	if id != "sandbox" || !errors.Is(err, c.updateErr) || !client.leaseReleased || c.info.SnapshotKey != "" {
		t.Fatalf("id=%q err=%v info=%#v", id, err, c.info)
	}
	if err = m.Stop(t.Context(), id); err != nil || !client.snapshots.removed || !c.removed {
		t.Fatalf("partial snapshot cleanup: %v", err)
	}
}

func TestNativeSpecHonorsExplicitPrivilegePolicyAndCPUQuota(t *testing.T) {
	for _, noNewPrivileges := range []bool{false, true} {
		cpu := 1.5
		r := deployment.Request{Name: "sandbox", Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Workdir: "/task", NoNewPrivileges: noNewPrivileges, CPU: &cpu}
		spec, err := oci.GenerateSpec(namespaces.WithNamespace(t.Context(), "aries"), nil, &containers.Container{ID: r.Name}, sandboxSpecOptions(r)...)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Process.NoNewPrivileges != noNewPrivileges || spec.Process.Cwd != "/task" || spec.Annotations[vmAnnotation] != "sandbox" || !reflect.DeepEqual(spec.Process.Args, []string{"/bin/sleep", "infinity"}) {
			t.Fatalf("native spec=%#v", spec.Process)
		}
		if *spec.Linux.Resources.CPU.Period != 100000 || *spec.Linux.Resources.CPU.Quota != 150000 {
			t.Fatalf("CPU quota=%#v", spec.Linux.Resources.CPU)
		}
	}
}

func TestExecProcessPreservesShellAndExplicitOverrides(t *testing.T) {
	body := `printf '%s\n' "$HOME"; echo '$(do not expand)'`
	base := specs.Process{Cwd: "/image-work", Env: []string{"PATH=/bin", "HOME=/root", "KEEP=yes"}, User: specs.User{UID: 0, AdditionalGids: []uint32{10}}, Capabilities: &specs.LinuxCapabilities{Effective: []string{"CAP_DAC_OVERRIDE"}}}
	cmd := core.Command{Path: "/bin/sh", Args: []string{"-c", body}, Dir: "/task", User: "65532:65532", Env: map[string]string{"HOME": "/private", "X": "two words"}}
	p, err := execProcess(base, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Args, append([]string{cmd.Path}, cmd.Args...)) || p.Cwd != "/task" || p.User.UID != 65532 || len(p.User.AdditionalGids) != 0 || p.Capabilities != nil {
		t.Fatalf("process changed semantics: %#v", p)
	}
	if !reflect.DeepEqual(p.Env, []string{"HOME=/private", "KEEP=yes", "PATH=/bin", "X=two words"}) {
		t.Fatalf("env=%v", p.Env)
	}
	if base.Env[1] != "HOME=/root" || base.User.UID != 0 {
		t.Fatal("mutated shared image defaults")
	}
}

func TestValidationChecksGuestBindingIsolationAndSecrets(t *testing.T) {
	fixture := func(t *testing.T) (*Manager, deployment.Request, *fakeContainer, *fakeVM) {
		t.Helper()
		labels := ownedContainer().info.Labels
		delete(labels, stateLabel)
		r := deployment.Request{Name: "sandbox", Image: "example:test", Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Workdir: "/task", AllowNetwork: true, Labels: labels}
		spec, err := oci.GenerateSpec(namespaces.WithNamespace(t.Context(), "aries"), nil, &containers.Container{ID: r.Name}, sandboxSpecOptions(r)...)
		if err != nil {
			t.Fatal(err)
		}
		c := ownedContainer()
		c.spec = spec
		c.info.SnapshotKey = "sandbox"
		c.info.Image = "docker.io/library/example:test"
		encoded, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		c.info.Labels[configLabel] = string(encoded)
		vm := &fakeVM{}
		return &Manager{options: Options{Namespace: "aries", Snapshotter: "devmapper", CNINetwork: "fcnet"}, client: &fakeClient{container: c}, vm: vm}, r, c, vm
	}
	m, r, _, _ := fixture(t)
	if err := m.Validate(t.Context(), "sandbox", r, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*fakeContainer, *fakeVM)
	}{
		{"owner", func(c *fakeContainer, _ *fakeVM) { c.info.Labels["aries.run"] = "foreign" }},
		{"image", func(c *fakeContainer, _ *fakeVM) { c.info.Image = "other:tag" }},
		{"VM binding", func(c *fakeContainer, _ *fakeVM) { c.spec.Annotations[vmAnnotation] = "other" }},
		{"VM response", func(_ *fakeContainer, v *fakeVM) { v.wrongID = true }},
		{"shell", func(c *fakeContainer, _ *fakeVM) { c.spec.Process.Args = []string{"/bin/sh", "-c", "changed"} }},
		{"external namespace", func(c *fakeContainer, _ *fakeVM) {
			c.spec.Linux.Namespaces = append(c.spec.Linux.Namespaces, specs.LinuxNamespace{Type: specs.PIDNamespace, Path: "/proc/1/ns/pid"})
		}},
		{"host bind", func(c *fakeContainer, _ *fakeVM) {
			c.spec.Mounts = append(c.spec.Mounts, specs.Mount{Type: "bind", Source: "/host", Destination: "/host"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r, c, v := fixture(t)
			tc.change(c, v)
			if err := m.Validate(t.Context(), "sandbox", r, nil); err == nil {
				t.Fatal("accepted changed guest isolation")
			}
		})
	}
	m, r, c, _ := fixture(t)
	c.spec.Process.Env = append(c.spec.Process.Env, "PRIVATE=test-secret-value")
	if err := m.Validate(t.Context(), "sandbox", r, [][]byte{[]byte("test-secret-value")}); err == nil || strings.Contains(err.Error(), "test-secret-value") {
		t.Fatalf("secret validation=%v", err)
	}
}

type fakeTask struct {
	containerd.Task
	proc    *fakeProcess
	process *specs.Process
}

func (t *fakeTask) Exec(_ context.Context, _ string, spec *specs.Process, _ cio.Creator) (containerd.Process, error) {
	t.process = spec
	return t.proc, nil
}

type fakeProcess struct {
	containerd.Process
	exit    chan containerd.ExitStatus
	started chan struct{}
	deleted bool
	pipes   *fakeIO
}

func (p *fakeProcess) Wait(context.Context) (<-chan containerd.ExitStatus, error) { return p.exit, nil }
func (p *fakeProcess) Start(context.Context) error                                { close(p.started); return nil }
func (p *fakeProcess) IO() cio.IO                                                 { return p.pipes }
func (p *fakeProcess) Delete(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	p.deleted = true
	return containerd.NewExitStatus(0, time.Now(), nil), nil
}

type fakeIO struct {
	canceled, closed bool
	waitForClose     chan struct{}
}

func (*fakeIO) Config() cio.Config { return cio.Config{} }
func (p *fakeIO) Wait() {
	if p.waitForClose != nil {
		<-p.waitForClose
	}
}
func (p *fakeIO) Cancel() { p.canceled = true }
func (p *fakeIO) Close() error {
	p.closed = true
	if p.waitForClose != nil {
		close(p.waitForClose)
	}
	return nil
}

func TestExecCancellationClosesIOAndLeavesGuestProcessAlive(t *testing.T) {
	p := &fakeProcess{exit: make(chan containerd.ExitStatus), started: make(chan struct{}), pipes: &fakeIO{}}
	task := &fakeTask{proc: p}
	c := ownedContainer()
	c.spec = &specs.Spec{Annotations: map[string]string{vmAnnotation: "sandbox"}, Process: &specs.Process{Cwd: "/"}}
	c.task = task
	m := &Manager{options: Options{Namespace: "aries"}, client: &fakeClient{container: c}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := m.ExecStream(ctx, "sandbox", core.Command{Path: "/bin/sh", Args: []string{"-c", "sleep 60"}}, nil, io.Discard, io.Discard)
		done <- err
	}()
	<-p.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if p.deleted || !p.pipes.closed || !p.pipes.canceled {
		t.Fatalf("deleted=%v IO=%#v", p.deleted, p.pipes)
	}
	// Kill is deliberately not implemented by the fake; calling it panics.
}

func TestNativeNonzeroExitIsAResult(t *testing.T) {
	p := &fakeProcess{exit: make(chan containerd.ExitStatus, 1), started: make(chan struct{}), pipes: &fakeIO{}}
	p.exit <- *containerd.NewExitStatus(17, time.Now(), nil)
	c := ownedContainer()
	c.spec = &specs.Spec{Annotations: map[string]string{vmAnnotation: "sandbox"}, Process: &specs.Process{Cwd: "/"}}
	c.task = &fakeTask{proc: p}
	m := &Manager{options: Options{Namespace: "aries"}, client: &fakeClient{container: c}}
	result, err := m.ExecStream(t.Context(), "sandbox", core.Command{Path: "/bin/false"}, nil, nil, nil)
	if err != nil || result.ExitCode != 17 || !p.deleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestExecDeadlineClosesOutputInheritedByBackgroundProcesses(t *testing.T) {
	p := &fakeProcess{exit: make(chan containerd.ExitStatus, 1), started: make(chan struct{}), pipes: &fakeIO{waitForClose: make(chan struct{})}}
	p.exit <- *containerd.NewExitStatus(0, time.Now(), nil)
	c := ownedContainer()
	c.spec = &specs.Spec{Annotations: map[string]string{vmAnnotation: "sandbox"}, Process: &specs.Process{Cwd: "/"}}
	c.task = &fakeTask{proc: p}
	m := &Manager{options: Options{Namespace: "aries"}, client: &fakeClient{container: c}}
	_, err := m.ExecStream(t.Context(), "sandbox", core.Command{Path: "/bin/sh", Timeout: 20 * time.Millisecond}, nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !p.pipes.closed || p.deleted {
		t.Fatalf("drain timeout=%v IO=%#v", err, p.pipes)
	}
}

func TestVMRequestAndUnsupportedSettings(t *testing.T) {
	m := &Manager{options: Options{VCPUs: 1, MemoryMB: 1024, CNINetwork: "fcnet"}}
	cpu, memory := 1.5, 4096
	r := deployment.Request{Name: "sandbox", Image: "image:tag", Entrypoint: []string{"/bin/sleep"}, Labels: ownedContainer().info.Labels, CPU: &cpu, MemoryMB: &memory}
	if err := m.validateRequest(r); err != nil {
		t.Fatal(err)
	}
	vm := m.vmRequest(r)
	if vm.MachineCfg.VcpuCount != 2 || vm.MachineCfg.MemSizeMib != 4096 || len(vm.NetworkInterfaces) != 0 || vm.ExitAfterAllTasksDeleted {
		t.Fatalf("VM config=%v", vm)
	}
	r.AllowNetwork = true
	if m.vmRequest(r).NetworkInterfaces[0].CNIConfig.NetworkName != "fcnet" {
		t.Fatal("missing CNI network")
	}
	m.options.CNINetwork = ""
	if err := m.validateRequest(r); err == nil {
		t.Fatal("allowed network without a CNI network")
	}
	r.AllowNetwork = false
	r.Mounts = []deployment.Mount{{Source: "/", Target: "/host"}}
	if err := m.validateRequest(r); err == nil {
		t.Fatal("accepted host mount")
	}
}

func TestOutputLimitCancelsTransport(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	var out strings.Builder
	w := &boundedWriter{out: &out, limit: 3, cancel: cancel}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("d")); err == nil || ctx.Err() == nil || out.String() != "abc" {
		t.Fatal("unbounded exec output")
	}
}
