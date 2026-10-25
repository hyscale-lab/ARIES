package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// fakeKubectl answers each invocation from respond and records what was run.
type fakeKubectl struct {
	mu      sync.Mutex
	calls   [][]string
	inputs  [][]byte
	respond func(ctx context.Context, args []string, stdin []byte, stdout, stderr io.Writer) error
}

func (f *fakeKubectl) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	var input []byte
	if stdin != nil {
		input, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	f.inputs = append(f.inputs, input)
	f.mu.Unlock()
	if f.respond == nil {
		return nil
	}
	return f.respond(ctx, args, input, stdout, stderr)
}

func (f *fakeKubectl) verbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var verbs []string
	for _, call := range f.calls {
		verb := call[0]
		if verb == "get" || verb == "delete" {
			verb += " " + call[1]
		}
		verbs = append(verbs, verb)
	}
	return verbs
}

type exitError int

func (e exitError) Error() string { return "exit status" }
func (e exitError) ExitCode() int { return int(e) }

func newTestManager(t *testing.T, options Options) (*Manager, *fakeKubectl) {
	t.Helper()
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeKubectl{}
	manager.kubectl = fake
	manager.inCluster = true
	return manager, fake
}

func testRequest() deployment.Request {
	cpu, memory := 1.5, 512
	return deployment.Request{
		Name: "aries-sandbox-abc123", Image: "example.test/task:1", Workdir: "/app",
		Env: []string{"A=b=c"}, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"},
		Labels:    map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run-1", "aries.task": "task-1"},
		Placement: core.RuntimePlacement{AttachmentID: "aries"}, CPU: &cpu, MemoryMB: &memory, NoNewPrivileges: true, ServicePort: 8443,
	}
}

func decodeManifest(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestPodManifestGatesTheRequestedCommand(t *testing.T) {
	manager, _ := newTestManager(t, Options{NodeSelector: map[string]string{"loader-nodetype": "worker"}, ImagePullSecret: "registry"})
	raw, err := manager.podManifest(testRequest())
	if err != nil {
		t.Fatal(err)
	}
	manifest := decodeManifest(t, raw)
	metadata := manifest["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	if labels[managedByLabel] != "aries" || labels[componentLabel] != "sandbox" || labels[runtimeLabel] != "aries-sandbox-abc123" {
		t.Fatalf("labels = %v", labels)
	}
	// Run and task IDs may exceed the label-value limit, so they are annotations.
	if annotations := metadata["annotations"].(map[string]any); annotations["aries.run"] != "run-1" || annotations["aries.task"] != "task-1" {
		t.Fatalf("annotations = %v", annotations)
	}
	spec := manifest["spec"].(map[string]any)
	if spec["restartPolicy"] != "Never" || spec["automountServiceAccountToken"] != false || spec["enableServiceLinks"] != false {
		t.Fatalf("spec = %v", spec)
	}
	if selector := spec["nodeSelector"].(map[string]any); len(selector) != 1 || selector["loader-nodetype"] != "worker" || spec["tolerations"] != nil {
		t.Fatalf("node placement = %v", spec)
	}
	if _, present := spec["initContainers"]; present {
		t.Fatal("init container added without staging directories")
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	command := container["command"].([]any)
	want := []any{"/bin/sh", "-c", gateShell, "aries-gate", "/bin/sleep", "infinity"}
	if !slices.Equal(command, want) || container["args"] != nil {
		t.Fatalf("command = %v", command)
	}
	if env := container["env"].([]any)[0].(map[string]any); env["name"] != "A" || env["value"] != "b=c" {
		t.Fatalf("env = %v", env)
	}
	limits := container["resources"].(map[string]any)["limits"].(map[string]any)
	if limits["cpu"] != "1.5" || limits["memory"] != "512Mi" {
		t.Fatalf("limits = %v", limits)
	}
	if security := container["securityContext"].(map[string]any); security["allowPrivilegeEscalation"] != false {
		t.Fatalf("security = %v", security)
	}
}

func TestPodManifestStagingAndBridgeIdentity(t *testing.T) {
	manager, _ := newTestManager(t, Options{ServiceAccount: "aries-bridge", StagingDirs: []string{"/run/aries", "/opt/aries"}, StagingOwner: "1000:1000", Images: map[string]string{"example.test/task": "registry.test/task"}})
	raw, err := manager.podManifest(testRequest())
	if err != nil {
		t.Fatal(err)
	}
	spec := decodeManifest(t, raw)["spec"].(map[string]any)
	if spec["serviceAccountName"] != "aries-bridge" || spec["automountServiceAccountToken"] != true {
		t.Fatalf("service account = %v", spec)
	}
	init := spec["initContainers"].([]any)[0].(map[string]any)
	command := init["command"].([]any)
	if !slices.Equal(command[3:], []any{"aries-staging", "1000:1000", "/run/aries", "/opt/aries"}) || init["securityContext"].(map[string]any)["runAsUser"] != float64(0) {
		t.Fatalf("init container = %v", init)
	}
	// A repository mapping keeps the requested tag.
	if init["image"] != "registry.test/task:1" || spec["containers"].([]any)[0].(map[string]any)["image"] != "registry.test/task:1" {
		t.Fatalf("image = %v", init["image"])
	}
	if len(spec["volumes"].([]any)) != 3 {
		t.Fatalf("volumes = %v", spec["volumes"])
	}
}

func TestValidateRequestRejectsWhatTheProviderCannotHonor(t *testing.T) {
	manager, _ := newTestManager(t, Options{})
	for name, mutate := range map[string]func(*deployment.Request){
		"name":      func(r *deployment.Request) { r.Name = "Aries_Task" },
		"placement": func(r *deployment.Request) { r.Placement.AttachmentID = "other" },
		"host mount": func(r *deployment.Request) {
			r.Mounts = []deployment.Mount{{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}}
		},
		"alias":        func(r *deployment.Request) { r.NetworkAliases = []string{"search"} },
		"no command":   func(r *deployment.Request) { r.Entrypoint, r.Args = nil, nil },
		"env":          func(r *deployment.Request) { r.Env = []string{"NOVALUE"} },
		"service port": func(r *deployment.Request) { r.ServicePort = 70000 },
	} {
		request := testRequest()
		mutate(&request)
		if err := manager.validateRequest(request); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if id, err := manager.Create(context.Background(), request); err == nil || id != "" {
			t.Errorf("%s: Create = %q, %v", name, id, err)
		}
	}
}

func TestCreateWaitsForTheGateAndRecoversLostResponses(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil || id != "aries-sandbox-abc123" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if verbs := fake.verbs(); !slices.Equal(verbs, []string{"create", "wait"}) {
		t.Fatalf("calls = %v", verbs)
	}
	if !bytes.Contains(fake.inputs[0], []byte(`"kind":"Pod"`)) {
		t.Fatalf("create input = %s", fake.inputs[0])
	}

	existing := `{"metadata":{"name":"aries-sandbox-abc123","namespace":"aries","annotations":{"aries.managed":"true","aries.kind":"task-container","aries.component":"sandbox","aries.run":"run-1","aries.task":"task-1"}}}`
	for name, test := range map[string]struct {
		pod        string
		wantID     string
		unresolved bool
	}{
		"absent":  {pod: "", wantID: ""},
		"owned":   {pod: existing, wantID: "aries-sandbox-abc123"},
		"foreign": {pod: strings.Replace(existing, "run-1", "run-2", 1), wantID: "", unresolved: true},
	} {
		fake.respond = func(_ context.Context, args []string, _ []byte, stdout, _ io.Writer) error {
			if args[0] == "create" {
				return errors.New("connection reset")
			}
			_, _ = io.WriteString(stdout, test.pod)
			return nil
		}
		id, err := manager.Create(context.Background(), testRequest())
		if err == nil || id != test.wantID || errors.Is(err, deployment.ErrAllocationUnconfirmed) != test.unresolved {
			t.Errorf("%s: Create = %q, %v", name, id, err)
		}
	}
}

func podJSON(t *testing.T, manager *Manager, request deployment.Request, edit func(map[string]any)) string {
	t.Helper()
	raw, err := manager.podManifest(request)
	if err != nil {
		t.Fatal(err)
	}
	manifest := decodeManifest(t, raw)
	manifest["status"] = map[string]any{"phase": "Running", "podIP": "10.0.0.7"}
	if edit != nil {
		edit(manifest)
	}
	out, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestValidateChecksIdentityIsolationAndSecrets(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	request := testRequest()
	serve := func(pod string) {
		fake.respond = func(_ context.Context, _ []string, _ []byte, stdout, _ io.Writer) error {
			_, _ = io.WriteString(stdout, pod)
			return nil
		}
	}
	serve(podJSON(t, manager, request, nil))
	if err := manager.Validate(context.Background(), request.Name, request, [][]byte{[]byte("sk-secret")}); err != nil {
		t.Fatal(err)
	}
	container := func(m map[string]any) map[string]any {
		return m["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	}
	for name, edit := range map[string]func(map[string]any){
		"secret in env": func(m map[string]any) {
			container(m)["env"] = []any{map[string]string{"name": "KEY", "value": "sk-secret"}}
		},
		"command": func(m map[string]any) { container(m)["command"] = []any{"/bin/sleep", "infinity"} },
		"image":   func(m map[string]any) { container(m)["image"] = "example.test/other:1" },
		"owner": func(m map[string]any) {
			m["metadata"].(map[string]any)["annotations"].(map[string]any)["aries.run"] = "run-2"
		},
		"privilege": func(m map[string]any) {
			container(m)["securityContext"] = map[string]any{"allowPrivilegeEscalation": true}
		},
		"host network": func(m map[string]any) { m["spec"].(map[string]any)["hostNetwork"] = true },
		"host path": func(m map[string]any) {
			m["spec"].(map[string]any)["volumes"] = []any{map[string]any{"name": "h", "hostPath": map[string]any{"path": "/"}}}
		},
		"token": func(m map[string]any) { m["spec"].(map[string]any)["automountServiceAccountToken"] = true },
	} {
		serve(podJSON(t, manager, request, edit))
		if err := manager.Validate(context.Background(), request.Name, request, [][]byte{[]byte("sk-secret")}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStartRunningAddressAndStop(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	request := testRequest()
	present := true
	fake.respond = func(_ context.Context, args []string, _ []byte, stdout, _ io.Writer) error {
		if args[0] == "delete" {
			present = false
		}
		if args[0] == "get" && present {
			_, _ = io.WriteString(stdout, podJSON(t, manager, request, nil))
		}
		return nil
	}
	if err := manager.Start(context.Background(), request.Name); err != nil {
		t.Fatal(err)
	}
	if start := fake.calls[0]; !slices.Equal(start[len(start)-3:], []string{"/bin/sh", "-c", ": > " + gateMarker}) {
		t.Fatalf("start = %v", start)
	}
	if running, err := manager.Running(context.Background(), request.Name); err != nil || !running {
		t.Fatalf("Running = %v, %v", running, err)
	}
	for _, resolve := range []func(context.Context, string, int) (string, error){manager.Address, manager.TaskAddress} {
		if address, err := resolve(context.Background(), request.Name, 8443); err != nil || address != "10.0.0.7:8443" {
			t.Fatalf("address = %q, %v", address, err)
		}
	}
	if err := manager.Stop(context.Background(), request.Name); err != nil {
		t.Fatal(err)
	}
	if running, err := manager.Running(context.Background(), request.Name); err != nil || running {
		t.Fatalf("Running after stop = %v, %v", running, err)
	}
	// Stop must not report success while the pod is still visible.
	present = true
	fake.respond = func(_ context.Context, args []string, _ []byte, stdout, _ io.Writer) error {
		if args[0] == "get" {
			_, _ = io.WriteString(stdout, podJSON(t, manager, request, nil))
		}
		return nil
	}
	if err := manager.Stop(context.Background(), request.Name); err == nil {
		t.Fatal("Stop confirmed absence of a pod that remains")
	}
}

func TestAddressForwardsOutsideTheCluster(t *testing.T) {
	manager, _ := newTestManager(t, Options{})
	manager.inCluster = false
	started := 0
	manager.forward = func(context.Context, string, int) (*portForward, error) {
		started++
		done := make(chan struct{})
		close(done)
		return &portForward{address: "127.0.0.1:40000", cancel: func() {}, done: done}, nil
	}
	for range 2 {
		if address, err := manager.Address(context.Background(), "aries-bridge-1", 8443); err != nil || address != "127.0.0.1:40000" {
			t.Fatalf("Address = %q, %v", address, err)
		}
	}
	if started != 1 {
		t.Fatalf("port-forwards started = %d, want one reused", started)
	}
	if err := manager.Close(); err != nil || len(manager.forwards) != 0 {
		t.Fatalf("Close = %v, forwards = %d", err, len(manager.forwards))
	}
}

func TestExecStreamPreservesArgvAndKubectlResult(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	fake.respond = func(_ context.Context, _ []string, stdin []byte, stdout, stderr io.Writer) error {
		_, _ = stdout.Write(append([]byte("out:"), stdin...))
		_, _ = io.WriteString(stderr, "warning\ncommand terminated with exit code 3\n")
		return exitError(3)
	}
	command := core.Command{Path: "printf", Args: []string{"a b", "", "$HOME; rm"}, Stdin: []byte("in")}
	result, err := manager.Exec(context.Background(), "aries-sandbox-abc123", command)
	if err != nil || result.ExitCode != 3 || result.Stdout != "out:in" || result.Stderr != "warning\ncommand terminated with exit code 3\n" {
		t.Fatalf("Exec = %+v, %v", result, err)
	}
	want := []string{"exec", "-n", "aries", "aries-sandbox-abc123", "-c", containerName, "-i", "--", "printf", "a b", "", "$HOME; rm"}
	if !slices.Equal(fake.calls[0], want) {
		t.Fatalf("kubectl args = %q, want %q", fake.calls[0], want)
	}

	failure := errors.New("unable to start kubectl")
	fake.respond = func(_ context.Context, _ []string, _ []byte, _, _ io.Writer) error {
		return failure
	}
	if result, err := manager.Exec(context.Background(), "aries-sandbox-abc123", core.Command{Path: "true"}); !errors.Is(err, failure) || result.ExitCode != -1 {
		t.Fatalf("transport failure = %+v, %v", result, err)
	}
}

func TestExecWorkdirAndEnvironmentPreserveArguments(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	// Run the remote argv locally to verify shell quoting and I/O behavior.
	fake.respond = func(ctx context.Context, args []string, stdin []byte, stdout, stderr io.Writer) error {
		remote := args[slices.Index(args, "--")+1:]
		command := exec.CommandContext(ctx, remote[0], remote[1:]...)
		command.Stdin = bytes.NewReader(stdin)
		command.Stdout, command.Stderr = stdout, stderr
		return command.Run()
	}
	dir := filepath.Join(t.TempDir(), "work dir; $HOME")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	command := core.Command{
		Path: "/bin/sh", Args: []string{"-c", `printf '%s\n' "$PWD" "$CHECK" "$@"; cat; printf 'warning\n' >&2; exit 3`, "probe", "a b", "", "$HOME; rm"},
		Dir: dir, Env: map[string]string{"CHECK": "value; $HOME"}, Stdin: []byte("input\n"),
	}
	result, err := manager.Exec(context.Background(), "aries-sandbox-abc123", command)
	want := dir + "\nvalue; $HOME\na b\n\n$HOME; rm\ninput\n"
	if err != nil || result.ExitCode != 3 || result.Stdout != want || result.Stderr != "warning\n" {
		t.Fatalf("Exec = %+v, %v; want stdout %q", result, err, want)
	}
}

func TestExecUserUsesSetpriv(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	if _, err := manager.Exec(context.Background(), "aries-sandbox-abc123", core.Command{Path: "id", User: "65532:65532", Env: map[string]string{"HOME": "/home/agent"}}); err != nil {
		t.Fatal(err)
	}
	remote := fake.calls[0][slices.Index(fake.calls[0], "--")+1:]
	want := []string{"env", "HOME=/", "setpriv", "--reuid=65532", "--regid=65532", "--clear-groups", "--", "env", "HOME=/home/agent", "id"}
	if !slices.Equal(remote, want) {
		t.Fatalf("argv = %q, want %q", remote, want)
	}
	if _, err := manager.Exec(context.Background(), "aries-sandbox-abc123", core.Command{Path: "true", User: "0:0"}); err != nil {
		t.Fatal(err)
	}
	remote = fake.calls[1][slices.Index(fake.calls[1], "--")+1:]
	if !slices.Equal(remote, []string{"true"}) {
		t.Fatalf("root argv = %q", remote)
	}
	if _, err := manager.Exec(context.Background(), "aries-sandbox-abc123", core.Command{Path: "true", User: "agent"}); err == nil {
		t.Fatal("named exec user accepted")
	}
}

func TestExecStreamCancelsOnlyTheClientOnTimeout(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	fake.respond = func(ctx context.Context, _ []string, _ []byte, _, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	result, err := manager.Exec(context.Background(), "aries-sandbox-abc123", core.Command{Path: "sleep", Args: []string{"60"}, Timeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != -1 {
		t.Fatalf("Exec = %+v, %v", result, err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("timeout made additional kubectl calls: %v", fake.calls)
	}
}

func TestExecStreamBoundsOutput(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			manager, fake := newTestManager(t, Options{})
			fake.respond = func(ctx context.Context, _ []string, _ []byte, stdout, stderr io.Writer) error {
				output := stdout
				if stream == "stderr" {
					output = stderr
				}
				if _, err := output.Write(bytes.Repeat([]byte("x"), 32)); err == nil {
					return errors.New("limit not enforced")
				}
				<-ctx.Done()
				return ctx.Err()
			}
			_, err := manager.Exec(context.Background(), "aries-sandbox-abc123", core.Command{Path: "yes", OutputLimitBytes: 16})
			if err == nil || !strings.Contains(err.Error(), "output exceeds 16 bytes") {
				t.Fatalf("Exec = %v", err)
			}
			if len(fake.calls) != 1 {
				t.Fatalf("output limit made additional kubectl calls: %v", fake.calls)
			}
		})
	}
}

func TestArchiveTransfer(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	if err := manager.UploadArchive(context.Background(), "aries-sandbox-abc123", "/app", strings.NewReader("tar-bytes")); err != nil {
		t.Fatal(err)
	}
	upload := fake.calls[0]
	if !slices.Equal(upload[len(upload)-6:], []string{"tar", "-x", "-f", "-", "-C", "/app"}) || string(fake.inputs[0]) != "tar-bytes" {
		t.Fatalf("upload = %v", upload)
	}
	fake.respond = func(_ context.Context, args []string, _ []byte, stdout, stderr io.Writer) error {
		if slices.Contains(args, "stat") {
			_, _ = io.WriteString(stdout, "9 81a4\n")
			return nil
		}
		_, _ = io.WriteString(stdout, "tar-bytes")
		return nil
	}
	stream, info, err := manager.DownloadArchive(context.Background(), "aries-sandbox-abc123", "/app/out.txt")
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(stream)
	if readErr != nil || string(content) != "tar-bytes" || info.Size != 9 || !info.Mode.IsRegular() || info.Mode.Perm() != 0o644 || stream.Close() != nil {
		t.Fatalf("download = %q %+v %v", content, info, readErr)
	}
	archive := fake.calls[len(fake.calls)-1]
	if !slices.Equal(archive[len(archive)-6:], []string{"tar", "-c", "-f", "-", "-C", "/app", "out.txt"}[1:]) {
		t.Fatalf("archive = %v", archive)
	}
	fake.respond = func(_ context.Context, args []string, _ []byte, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "stat: cannot statx '/app/missing': No such file or directory\n")
		return exitError(1)
	}
	if _, _, err := manager.DownloadArchive(context.Background(), "aries-sandbox-abc123", "/app/missing"); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("missing source = %v", err)
	}
}

func TestParseStatAndMemoryQuantity(t *testing.T) {
	if info, err := parseStat("4096 41ed"); err != nil || !info.Mode.IsDir() || info.Mode.Perm() != 0o755 {
		t.Fatalf("directory = %+v, %v", info, err)
	}
	if _, err := parseStat("garbage"); err == nil {
		t.Fatal("malformed stat accepted")
	}
	for value, want := range map[string]uint64{"512Mi": 512 << 20, "2Gi": 2 << 30, "1000": 1000, "5M": 5_000_000, "": 0, "lots": 0} {
		if got := parseMemoryQuantity(value); got != want {
			t.Errorf("parseMemoryQuantity(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	for name, options := range map[string]Options{
		"namespace":     {Namespace: "Not_A_Label"},
		"selector key":  {NodeSelector: map[string]string{"bad key": "worker"}},
		"selector val":  {NodeSelector: map[string]string{"loader-nodetype": "-bad"}},
		"staging path":  {StagingDirs: []string{"relative"}, StagingOwner: "1000:1000"},
		"staging owner": {StagingDirs: []string{"/run/aries"}, StagingOwner: "node"},
	} {
		if _, err := New(options); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDescribeWaitingNamesTheImageAPullFailedFor(t *testing.T) {
	var pod podObject
	if err := json.Unmarshal([]byte(`{"status":{"phase":"Pending","containerStatuses":[
		{"image":"ghcr.io/hyscale-lab/aries-bridge:v1-0123456789ab","state":{"waiting":{"reason":"ErrImagePull"}}}]}}`), &pod); err != nil {
		t.Fatal(err)
	}
	if got := describeWaiting(pod); got != " (ErrImagePull for ghcr.io/hyscale-lab/aries-bridge:v1-0123456789ab)" {
		t.Fatalf("pull failure = %q", got)
	}
	pod.Status.ContainerStatuses[0].State.Waiting.Reason = "ContainerCreating"
	if got := describeWaiting(pod); got != " (ContainerCreating)" {
		t.Fatalf("other reason = %q", got)
	}
	pod.Status.ContainerStatuses = nil
	if got := describeWaiting(pod); got != " (phase Pending)" {
		t.Fatalf("no reason = %q", got)
	}
}
