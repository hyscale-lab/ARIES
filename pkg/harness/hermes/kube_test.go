package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const testKubeSecret = "sk-kube-test-secret-value"

// fakeKubectl scripts the kubectl calls the Kubernetes backend makes. It echoes
// the created pod back as the admitted one, optionally through admit, which is
// how a mutating admission webhook is simulated.
type fakeKubectl struct {
	mu        sync.Mutex
	calls     [][]string
	stdins    map[string][]byte
	created   []byte
	deleted   bool
	stuck     bool // pod survives delete
	admit     func(map[string]any)
	agentOut  string
	agentErr  string
	agentExit int
}

func newFakeKubectl() *fakeKubectl {
	return &fakeKubectl{stdins: map[string][]byte{}, agentOut: "done\n"}
}

func (fake *fakeKubectl) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, append([]string(nil), args...))
	switch args[0] {
	case "create":
		fake.created = bytes.Clone(stdin)
		return nil, nil, nil
	case "wait":
		return nil, nil, nil
	case "delete":
		fake.deleted = !fake.stuck
		return nil, nil, nil
	case "logs":
		return []byte("container started\n"), nil, nil
	case "get":
		switch {
		case slices.Contains(args, "json"):
			var pod map[string]any
			if err := json.Unmarshal(fake.created, &pod); err != nil {
				return nil, nil, err
			}
			if fake.admit != nil {
				fake.admit(pod)
			}
			out, err := json.Marshal(pod)
			return out, nil, err
		case slices.Contains(args, "name"):
			if fake.deleted {
				return nil, nil, nil
			}
			return []byte("pod/aries-hermes-attempt\n"), nil, nil
		default:
			return []byte("Running"), nil, nil
		}
	case "exec":
		if slices.Contains(args, "tar") {
			fake.stdins["tar"] = bytes.Clone(stdin)
			return nil, nil, nil
		}
		return fake.execResponse(args)
	}
	return nil, nil, fmt.Errorf("unexpected kubectl call %q", args)
}

// execResponse answers a wrapped exec the way the real shell would: stdout,
// then stderr followed by the trailer, then kubectl's own suffix on failure.
func (fake *fakeKubectl) execResponse(args []string) ([]byte, []byte, error) {
	marker := slices.Index(args, "aries-hermes-exec")
	if marker < 0 || len(args) < marker+4 {
		return nil, nil, fmt.Errorf("exec was not wrapped: %q", args)
	}
	token, command := args[marker+2], args[marker+3:]
	trailer := func(stderr string, status int) []byte {
		return []byte(fmt.Sprintf("%s\x1eARIES_HERMES_EXIT_%s=%d\x1f", stderr, token, status))
	}
	switch command[0] {
	case "/bin/sh": // readiness probe
		return nil, trailer("", 0), nil
	case "hermes": // sessions export
		return []byte(`{"id":"s1","messages":[]}` + "\n"), trailer("", 0), nil
	case agentWrapperPath:
		stderr := trailer(fake.agentErr, fake.agentExit)
		if fake.agentExit != 0 {
			stderr = append(stderr, fmt.Sprintf("command terminated with exit code %d\n", fake.agentExit)...)
			return []byte(fake.agentOut), stderr, fmt.Errorf("exit status %d", fake.agentExit)
		}
		return []byte(fake.agentOut), stderr, nil
	}
	return nil, nil, fmt.Errorf("unexpected exec %q", command)
}

func (fake *fakeKubectl) allArgs() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var all []string
	for _, call := range fake.calls {
		all = append(all, strings.Join(call, " "))
	}
	return strings.Join(all, "\n")
}

func newTestKubeManager(t *testing.T, fake *fakeKubectl, nodeRole string) *KubeManager {
	t.Helper()
	manager, err := NewKube(KubeOptions{
		Image: testHermesImage, OutputDir: t.TempDir(), NodeRole: nodeRole,
		StartTimeout: 2 * time.Second, AgentTimeout: 2 * time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return []byte(testKubeSecret), true },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.kubectl = fake
	manager.newID = func() (string, error) { return "attempt", nil }
	return manager
}

func kubeRequest(t *testing.T) core.HarnessRequest {
	t.Helper()
	request := testRequest(t)
	// On Kubernetes the endpoint is the bridge's advertised pod IP, and the
	// sandbox reports namespace/pod as its network name.
	request.Endpoint.Address = "10.244.1.7:39425"
	request.Endpoint.Network = "aries/aries-sandbox-x"
	cpu, memory := 1.5, 2048
	request.CPU, request.MemoryMB = &cpu, &memory
	return request
}

// The whole lifecycle, and the artifact layout it leaves: identical file names
// to the Docker backend, so results compare across backends.
func TestKubeLifecycleStagesRunsAndCollectsTheDockerArtifactLayout(t *testing.T) {
	fake := newFakeKubectl()
	manager := newTestKubeManager(t, fake, "harness")
	ctx := context.Background()

	if err := manager.Start(ctx, kubeRequest(t)); err != nil {
		t.Fatal(err)
	}
	entries := archiveEntries(t, fake.stdins["tar"])
	for _, path := range []string{configContainerPath, modelKeyPath, identityContainerFS, agentWrapperPath} {
		header, ok := entries[strings.TrimPrefix(path, "/")]
		if !ok {
			t.Fatalf("staged archive lacks %s", path)
		}
		if header.Uid != runtimeUID || header.Gid != runtimeGID {
			t.Errorf("%s owned by %d:%d, want the hermes user", path, header.Uid, header.Gid)
		}
	}

	result, err := manager.Run(ctx, "fix the git repository")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != core.StatusSucceeded || result.FinalResponse != "done" {
		t.Fatalf("result = %+v", result)
	}
	artifactDir := filepath.Join(manager.outputDir, "fix-git", "harness")
	for _, name := range []string{
		"config.yaml", "session-outcome.json", "hermes_stdout.log", "hermes_stderr.log",
		"container.log", "telemetry/sessions.jsonl", "telemetry.index.json",
	} {
		if _, err := os.Stat(filepath.Join(artifactDir, name)); err != nil {
			t.Errorf("artifact %s: %v", name, err)
		}
	}

	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if manager.active != nil {
		t.Error("session still active after a confirmed teardown")
	}

	// The key is staged into the running pod and never enters a kubectl argv,
	// the pod manifest, or any retained artifact.
	if strings.Contains(fake.allArgs(), testKubeSecret) {
		t.Error("the API key appeared in a kubectl argument")
	}
	if bytes.Contains(fake.created, []byte(testKubeSecret)) {
		t.Error("the API key appeared in the pod manifest")
	}
	_ = filepath.WalkDir(artifactDir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if content, _ := os.ReadFile(path); bytes.Contains(content, []byte(testKubeSecret)) {
				t.Errorf("the API key appeared in artifact %s", path)
			}
		}
		return nil
	})
}

// A task the agent fails is a normal outcome, not a broken exec. kubectl
// reports it as an error and appends its own message after the trailer; the
// harness must still recover the exit status and write every artifact.
func TestKubeNonZeroAgentExitIsAnOutcomeNotATransportFailure(t *testing.T) {
	fake := newFakeKubectl()
	fake.agentOut, fake.agentErr, fake.agentExit = "partial\n", "tool failed\n", 3
	manager := newTestKubeManager(t, fake, "")
	ctx := context.Background()
	if err := manager.Start(ctx, kubeRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(ctx)

	result, err := manager.Run(ctx, "fix the git repository")
	if err == nil || !strings.Contains(err.Error(), "exited with status 3") {
		t.Fatalf("error = %v, want the agent's own exit status", err)
	}
	if result.Status != core.StatusFailed {
		t.Errorf("status = %s", result.Status)
	}
	artifactDir := filepath.Join(manager.outputDir, "fix-git", "harness")
	var outcome runOutcome
	raw, readErr := os.ReadFile(filepath.Join(artifactDir, "session-outcome.json"))
	if readErr != nil || json.Unmarshal(raw, &outcome) != nil {
		t.Fatalf("session outcome: %v", readErr)
	}
	if outcome.ExitCode != 3 || outcome.EndReason != "nonzero_exit" {
		t.Errorf("outcome = %+v, want exit 3 / nonzero_exit", outcome)
	}
	stderr, _ := os.ReadFile(filepath.Join(artifactDir, "hermes_stderr.log"))
	if string(stderr) != "tool failed\n" {
		t.Errorf("stderr = %q; the trailer and kubectl's suffix must both be stripped", stderr)
	}
}

func TestParseKubeExec(t *testing.T) {
	ctx := context.Background()
	const token = "tok"
	trailer := func(status int) string { return fmt.Sprintf("\x1eARIES_HERMES_EXIT_%s=%d\x1f", token, status) }
	cases := []struct {
		name     string
		stderr   string
		runErr   error
		wantCode int
		wantErr  bool
	}{
		{"clean exit", "diag\n" + trailer(0), nil, 0, false},
		{"non-zero with kubectl suffix", "diag\n" + trailer(2) + "command terminated with exit code 2\n", errors.New("exit 2"), 2, false},
		// Without a trailer the command never reported back.
		{"no trailer", "error: unable to upgrade connection\n", errors.New("exit 1"), -1, true},
		// Anything other than kubectl's own suffix after the trailer is
		// ambiguous, so it is refused rather than guessed at.
		{"data after trailer", trailer(0) + "unexpected\n", nil, -1, true},
		{"forged token", "\x1eARIES_HERMES_EXIT_other=0\x1f", nil, -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseKubeExec(ctx, token, nil, []byte(tc.stderr), tc.runErr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if result.exitCode != tc.wantCode {
				t.Errorf("exit code = %d, want %d", result.exitCode, tc.wantCode)
			}
		})
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := parseKubeExec(canceled, token, nil, []byte(trailer(0)), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled exec must report cancellation, got %v", err)
	}
}

// What the API server admitted is checked before any secret is staged, since a
// mutating webhook can change the pod after ARIES creates it.
func TestKubeRefusesAPodChangedByAdmission(t *testing.T) {
	cases := map[string]func(map[string]any){
		"hostPath volume": func(pod map[string]any) {
			pod["spec"].(map[string]any)["volumes"] = []any{map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}}}
		},
		"injected sidecar": func(pod map[string]any) {
			spec := pod["spec"].(map[string]any)
			spec["containers"] = append(spec["containers"].([]any), map[string]any{"name": "proxy", "image": "envoy:1"})
		},
		"host network": func(pod map[string]any) { pod["spec"].(map[string]any)["hostNetwork"] = true },
		"token mounted": func(pod map[string]any) {
			pod["spec"].(map[string]any)["automountServiceAccountToken"] = true
		},
		"swapped image": func(pod map[string]any) {
			container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			container["image"] = "docker.io/attacker/hermes:v1"
		},
		"secret-sourced env": func(pod map[string]any) {
			container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			container["env"] = append(container["env"].([]any), map[string]any{
				"name": "LEAK", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "aries-model", "key": "k"}},
			})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeKubectl()
			fake.admit = mutate
			manager := newTestKubeManager(t, fake, "")
			if err := manager.Start(context.Background(), kubeRequest(t)); err == nil {
				t.Fatal("started despite an altered pod")
			}
			if _, staged := fake.stdins["tar"]; staged {
				t.Error("the runtime, including the API key, was staged into an unverified pod")
			}
			if !fake.deleted {
				t.Error("the rejected pod was not deleted")
			}
		})
	}
}

func TestKubePodManifest(t *testing.T) {
	active := &kubeSession{session: &session{runID: "run-1", taskID: "fix-git", attemptID: "attempt"}, podName: "aries-hermes-attempt"}
	env, err := containerEnvironment(kubeRequest(t).Endpoint, workspaceRoot, defaultTerminalTimeout)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hermesPodManifest(active, "aries", testHermesImage, "harness", env, map[string]string{"cpu": "1500m", "memory": "2147483648"})
	if err != nil {
		t.Fatal(err)
	}
	var pod struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			AutomountServiceAccountToken bool              `json:"automountServiceAccountToken"`
			EnableServiceLinks           bool              `json:"enableServiceLinks"`
			RestartPolicy                string            `json:"restartPolicy"`
			NodeSelector                 map[string]string `json:"nodeSelector"`
			Tolerations                  []map[string]string
			Containers                   []struct {
				Command   []string            `json:"command"`
				Env       []map[string]string `json:"env"`
				Resources struct {
					Limits map[string]string `json:"limits"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		t.Fatal(err)
	}
	// managed-by is what pod telemetry selects on.
	if pod.Metadata.Labels["app.kubernetes.io/managed-by"] != "aries" || pod.Metadata.Labels["app.kubernetes.io/component"] != "harness" {
		t.Errorf("labels = %v", pod.Metadata.Labels)
	}
	if pod.Metadata.Annotations["aries.dev/task-id"] != "fix-git" {
		t.Errorf("annotations = %v", pod.Metadata.Annotations)
	}
	if pod.Spec.AutomountServiceAccountToken || pod.Spec.EnableServiceLinks || pod.Spec.RestartPolicy != "Never" {
		t.Errorf("spec = %+v", pod.Spec)
	}
	if pod.Spec.NodeSelector[nodeRoleLabel] != "harness" || len(pod.Spec.Tolerations) != 1 {
		t.Errorf("placement: selector=%v tolerations=%v", pod.Spec.NodeSelector, pod.Spec.Tolerations)
	}
	container := pod.Spec.Containers[0]
	if !slices.Equal(container.Command, kubeIdleCommand) {
		t.Errorf("command = %q", container.Command)
	}
	if container.Resources.Limits["cpu"] != "1500m" || container.Resources.Limits["memory"] != "2147483648" {
		t.Errorf("limits = %v", container.Resources.Limits)
	}
	values := map[string]string{}
	for _, entry := range container.Env {
		values[entry["name"]] = entry["value"]
	}
	if values["TERMINAL_SSH_HOST"] != "10.244.1.7" || values["TERMINAL_SSH_PORT"] != "39425" || values["TERMINAL_ENV"] != "ssh" {
		t.Errorf("terminal environment = %v", values)
	}
}

func TestKubeResourceLimits(t *testing.T) {
	cpu, tiny, memory := 1.5, 0.0001, 512
	limits, err := kubeResourceLimits(core.HarnessRequest{CPU: &cpu, MemoryMB: &memory})
	if err != nil {
		t.Fatal(err)
	}
	if limits["cpu"] != "1500m" || limits["memory"] != "536870912" {
		t.Errorf("limits = %v", limits)
	}
	// Below one millicore still has to be a valid, non-zero quantity.
	limits, err = kubeResourceLimits(core.HarnessRequest{CPU: &tiny})
	if err != nil || limits["cpu"] != "1m" {
		t.Errorf("tiny CPU limits = %v, err %v", limits, err)
	}
	if limits, _ := kubeResourceLimits(core.HarnessRequest{}); len(limits) != 0 {
		t.Errorf("no request should mean no limits, got %v", limits)
	}
	negative := -1.0
	if _, err := kubeResourceLimits(core.HarnessRequest{CPU: &negative}); err == nil {
		t.Error("negative CPU accepted")
	}
}

// A pod that survives deletion must not be forgotten: the session stays active
// so Stop can be retried.
func TestKubeStopKeepsTheSessionWhenThePodSurvives(t *testing.T) {
	fake := newFakeKubectl()
	manager := newTestKubeManager(t, fake, "")
	ctx := context.Background()
	if err := manager.Start(ctx, kubeRequest(t)); err != nil {
		t.Fatal(err)
	}
	fake.stuck = true
	if err := manager.Stop(ctx); err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("error = %v", err)
	}
	if manager.active == nil {
		t.Fatal("session dropped while its pod still exists")
	}
	fake.stuck = false
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestNewKubeValidatesNamesUpFront(t *testing.T) {
	base := KubeOptions{Image: testHermesImage, OutputDir: t.TempDir()}
	for name, mutate := range map[string]func(*KubeOptions){
		"floating image":   func(o *KubeOptions) { o.Image = "docker.io/nousresearch/hermes-agent:latest" },
		"namespace case":   func(o *KubeOptions) { o.Namespace = "Aries" },
		"role injection":   func(o *KubeOptions) { o.NodeRole = "harness,evil" },
		"empty output dir": func(o *KubeOptions) { o.OutputDir = " " },
	} {
		options := base
		mutate(&options)
		if _, err := NewKube(options); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
