package docker

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

func deploymentRequest() deployment.Request {
	return deployment.Request{Name: "aries-test-attempt", Image: "example/agent:v1", Args: []string{"/launcher", "one two", "$(literal)"}, Env: []string{"CONFIG=/private/config"}, Network: "task-network", Labels: map[string]string{"aries.managed": "true", "aries.kind": "test-harness", "aries.component": "harness", "aries.run": "run", "aries.task": "task", "aries.attempt": "attempt"}, ServicePort: 18789}
}

func TestCreatePreservesArgumentsResourcesAndPrivateService(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	request := deploymentRequest()
	cpu, memory := 2.5, 1536
	request.CPU, request.MemoryMB = &cpu, &memory
	id, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if id != "openclaw-id" {
		t.Fatalf("identity=%q", id)
	}
	if !slices.Equal(fake.created.Config.Cmd, request.Args) {
		t.Fatalf("argv=%q", fake.created.Config.Cmd)
	}
	limits := fake.created.HostConfig.Resources
	if limits.NanoCPUs != 2500000000 || limits.Memory != 1536<<20 {
		t.Fatalf("limits=%+v", limits)
	}
	port := network.MustParsePort("18789/tcp")
	bindings := fake.created.HostConfig.PortBindings[port]
	if len(bindings) != 1 || bindings[0].HostIP.String() != "127.0.0.1" || bindings[0].HostPort != "" {
		t.Fatalf("bindings=%+v", bindings)
	}
	if err := manager.UploadArchive(context.Background(), id, "/", strings.NewReader("private archive")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fake.archive, []byte("private archive")) {
		t.Fatal("archive changed")
	}
	if err := manager.Validate(context.Background(), id, request, [][]byte{[]byte("model-secret")}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRejectsInvalidResourcesBeforeDocker(t *testing.T) {
	for _, value := range []float64{0, -1, math.NaN(), math.Inf(1), math.Exp2(63) / 1e9} {
		fake := newFakeDocker()
		manager := &Manager{client: fake}
		request := deploymentRequest()
		request.CPU = &value
		if _, err := manager.Create(context.Background(), request); err == nil {
			t.Fatalf("CPU %v accepted", value)
		}
		if fake.createCalls != 0 {
			t.Fatal("Docker create called")
		}
	}
	for _, value := range []int{0, -1, int(math.MaxInt64>>20) + 1} {
		fake := newFakeDocker()
		manager := &Manager{client: fake}
		request := deploymentRequest()
		request.MemoryMB = &value
		if _, err := manager.Create(context.Background(), request); err == nil {
			t.Fatalf("memory %v accepted", value)
		}
		if fake.createCalls != 0 {
			t.Fatal("Docker create called")
		}
	}
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	if _, err := manager.Create(context.Background(), deploymentRequest()); err != nil {
		t.Fatal(err)
	}
	if got := fake.created.HostConfig.Resources; got.NanoCPUs != 0 || got.Memory != 0 {
		t.Fatalf("omitted resources=%+v", got)
	}
}

func TestValidateRejectsChangedOwnershipIsolationAndSecretMetadata(t *testing.T) {
	for name, change := range map[string]func(*container.InspectResponse){
		"identity":            func(c *container.InspectResponse) { c.ID = "other" },
		"missing config":      func(c *container.InspectResponse) { c.Config = nil },
		"missing host config": func(c *container.InspectResponse) { c.HostConfig = nil },
		"image":               func(c *container.InspectResponse) { c.Config.Image = "other:v1" },
		"argv":                func(c *container.InspectResponse) { c.Config.Cmd = []string{"other"} },
		"label":               func(c *container.InspectResponse) { c.Config.Labels["aries.attempt"] = "other" },
		"network":             func(c *container.InspectResponse) { c.HostConfig.NetworkMode = "host" },
		"env secret":          func(c *container.InspectResponse) { c.Config.Env = append(c.Config.Env, "TOKEN=model-secret") },
		"label secret":        func(c *container.InspectResponse) { c.Config.Labels["unexpected"] = "model-secret" },
		"entrypoint secret":   func(c *container.InspectResponse) { c.Config.Entrypoint = []string{"model-secret"} },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := &Manager{client: fake}
			request := deploymentRequest()
			id, err := manager.Create(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			// The fake stores SDK options; detach its label map before tampering.
			labels := map[string]string{}
			for k, v := range fake.container.Config.Labels {
				labels[k] = v
			}
			fake.container.Config.Labels = labels
			change(&fake.container)
			if err := manager.Validate(context.Background(), id, request, [][]byte{[]byte("model-secret")}); err == nil {
				t.Fatal("tampering accepted")
			}
		})
	}
}

func TestValidateAllowsOnlyDeclaredImageVolumes(t *testing.T) {
	for _, test := range []struct {
		name   string
		mounts []container.MountPoint
		binds  []string
		allow  bool
	}{
		{name: "no mounts", allow: true},
		{name: "declared volume", mounts: []container.MountPoint{{Type: "volume", Name: "anon", Destination: "/opt/data"}}, allow: true},
		{name: "bind", mounts: []container.MountPoint{{Type: "bind", Source: "/etc", Destination: "/etc"}}},
		{name: "unnamed volume", mounts: []container.MountPoint{{Type: "volume", Destination: "/opt/data"}}},
		{name: "other volume", mounts: []container.MountPoint{{Type: "volume", Name: "anon", Destination: "/workspace"}}},
		{name: "bind request", binds: []string{"/etc:/etc"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := &Manager{client: fake}
			request := deploymentRequest()
			request.ImageVolumes = []string{"/opt/data"}
			id, err := manager.Create(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			fake.container.Mounts = test.mounts
			fake.container.HostConfig.Binds = test.binds
			err = manager.Validate(context.Background(), id, request, nil)
			if (err == nil) != test.allow {
				t.Fatalf("Validate=%v", err)
			}
		})
	}
}

func TestAddressRequiresOneLoopbackBinding(t *testing.T) {
	for name, bindings := range map[string][]network.PortBinding{
		"missing": nil, "multiple": {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "80"}, {HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "81"}},
		"public": {{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "80"}}, "zero": {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "0"}}, "overflow": {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "65536"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := &Manager{client: fake}
			id, err := manager.Create(context.Background(), deploymentRequest())
			if err != nil {
				t.Fatal(err)
			}
			fake.container.NetworkSettings.Ports[network.MustParsePort("18789/tcp")] = bindings
			if _, err := manager.Address(context.Background(), id, 18789); err == nil {
				t.Fatal("invalid binding accepted")
			}
		})
	}
}

func TestStopRequiresPositiveAbsenceAndCanRetry(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	id, err := manager.Create(context.Background(), deploymentRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	fake.inspectErr = errors.New("daemon unavailable")
	fake.removeErr = errors.New("remove unavailable")
	if err := manager.Stop(context.Background(), id); err == nil {
		t.Fatal("unconfirmed absence accepted")
	}
	fake.inspectErr = nil
	fake.removeErr = nil
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !fake.removed {
		t.Fatal("runtime remains")
	}
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestStopRecoversOnlyAfterConfirmedRemoval(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	id, err := manager.Create(context.Background(), deploymentRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	fake.stopErr = errors.New("stop failed")
	fake.killErr = errors.New("kill failed")
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if fake.stopCalls != 1 || fake.killCalls != 1 || fake.removeCalls != 1 || !fake.removed {
		t.Fatal("cleanup skipped")
	}
}

func TestDownloadTranslatesOnlyTypedAbsence(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	fake.container.ID = "id"
	fake.copyFromErr = errors.Join(errors.New("absent"), errdefs.ErrNotFound)
	if _, _, err := manager.DownloadArchive(context.Background(), "id", "/missing"); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("absence=%v", err)
	}
	fake.copyFromErr = errors.New("daemon not found")
	if _, _, err := manager.DownloadArchive(context.Background(), "id", "/missing"); errors.Is(err, runner.ErrNotFound) || err == nil {
		t.Fatalf("daemon error=%v", err)
	}
}

func TestExecPreservesCommand(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	id, err := manager.Create(context.Background(), deploymentRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	command := core.Command{Path: "/launcher", Args: []string{"two words", "$(literal)"}, Dir: "/workspace", User: "123:456", Env: map[string]string{"SECOND": "two words", "FIRST": "$(literal)"}}
	result, err := manager.Exec(context.Background(), id, command)
	if err != nil {
		t.Fatal(err)
	}
	created := fake.execs["exec-1"]
	if !slices.Equal(created.Cmd[6:], append([]string{command.Path}, command.Args...)) || created.WorkingDir != command.Dir || created.User != command.User || !slices.Equal(created.Env, []string{"FIRST=$(literal)", "SECOND=two words"}) {
		t.Fatalf("exec request=%+v", created)
	}
	if result.Duration <= 0 || result.ExitCode != 0 || !strings.Contains(result.Stderr, "agent diagnostic") {
		t.Fatalf("result=%+v", result)
	}

}

func TestExecEnforcesOutputLimitIncludingBufferedStderr(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	id, err := manager.Create(context.Background(), deploymentRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Exercise a short stderr record retained alongside the final exit trailer.
	// Its bytes reach the bounded buffer only when the trailer is removed.
	fake.stderrOnly = true
	if _, err := manager.Exec(context.Background(), id, core.Command{Path: "/launcher", OutputLimitBytes: 4}); err == nil {
		t.Fatal("stderr overflow was accepted")
	}
}

func TestExecEnforcesCommandDeadline(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	id, err := manager.Create(context.Background(), deploymentRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	fake.execDelay = 100 * time.Millisecond
	if _, err := manager.Exec(context.Background(), id, core.Command{Path: "true", Timeout: time.Millisecond}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline=%v", err)
	}
}

func TestLogsEnforceBound(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	if _, err := manager.Logs(context.Background(), "id", 1); err == nil {
		t.Fatal("unbounded log accepted")
	}
	if _, err := manager.Logs(context.Background(), "id", 0); err == nil {
		t.Fatal("nonpositive bound accepted")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	fake := newFakeDocker()
	fake.closeErr = errors.New("close failed")
	manager := &Manager{client: fake}
	if err := manager.Close(); !errors.Is(err, fake.closeErr) {
		t.Fatal(err)
	}
	if err := manager.Close(); !errors.Is(err, fake.closeErr) {
		t.Fatal(err)
	}
	if fake.closeCalls != 1 {
		t.Fatalf("close calls=%d", fake.closeCalls)
	}
}

func TestSandboxConfigurationUsesSharedDeployment(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	request := deploymentRequest()
	request.Workdir = "/workspace"
	request.Init = true
	request.NoNewPrivileges = true
	request.StorageMB = 128
	request.GPUs = 2
	request.NetworkAliases = []string{"task-sandbox"}
	id, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Validate(context.Background(), id, request, nil); err != nil {
		t.Fatal(err)
	}
	host := fake.created.HostConfig
	if host.Init == nil || !*host.Init || !noNewPrivilegesEnabled(host.SecurityOpt) || host.StorageOpt["size"] != "128m" || len(host.DeviceRequests) != 1 || host.DeviceRequests[0].Count != 2 {
		t.Fatalf("host configuration=%+v", host)
	}
	if !slices.Equal(fake.created.NetworkingConfig.EndpointsConfig[request.Network].Aliases, request.NetworkAliases) {
		t.Fatal("network aliases missing")
	}
	host.SecurityOpt = nil
	if err := manager.Validate(context.Background(), id, request, nil); err == nil {
		t.Fatal("lost security option accepted")
	}
}

func TestDownloadContainerLossDoesNotTranslateToFileAbsence(t *testing.T) {
	fake := newFakeDocker()
	fake.copyFromErr = errdefs.ErrNotFound
	manager := &Manager{client: fake}
	if _, _, err := manager.DownloadArchive(context.Background(), "missing-runtime", "/missing"); err == nil || errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("download error=%v", err)
	}
}

func TestLogsTranslateOnlyTypedContainerAbsence(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	fake.containerLogsErr = errdefs.ErrNotFound
	if err := manager.LogsStream(context.Background(), "missing", nil, nil); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("missing logs error=%v", err)
	}
	if _, err := manager.Logs(context.Background(), "missing", 128); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("bounded logs suppressed absence: %v", err)
	}
	failure := errors.New("daemon not found")
	fake.containerLogsErr = failure
	if err := manager.LogsStream(context.Background(), "missing", nil, nil); !errors.Is(err, failure) || errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("ordinary logs error=%v", err)
	}
}
