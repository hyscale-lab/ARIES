//go:build integration

package bridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	bridgewiring "github.com/hyscale-lab/aries/internal/app/wiring/bridge"
	deploymentwiring "github.com/hyscale-lab/aries/internal/app/wiring/deployment"
	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	docker "github.com/hyscale-lab/aries/pkg/deployment/docker"
	tasksandbox "github.com/hyscale-lab/aries/pkg/sandbox"
	"github.com/moby/moby/client"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const managedFixtureImage = "docker.io/library/debian:12-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241"

func managedRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			t.Fatal("repository root missing")
		}
		dir = next
	}
}
func managedBinary(t *testing.T, name, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		path = filepath.Join(managedRoot(t), "bin", name)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("build %s before integration: %v", name, err)
	}
	return path
}
func managedQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }

// This matrix exercises borrowed Docker execution from actual independent
// container runtimes. Concurrent occurrences deliberately share task IDs.
func TestManagedBridgeRuntimeMatrix(t *testing.T) {
	helper := managedBinary(t, "aries-ssh-client", "ARIES_SSH_CLIENT")
	versions, err := config.LoadVersions(filepath.Join(managedRoot(t), "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = docker.PullImages(ctx, "", []string{managedFixtureImage}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	identities := map[string]bool{}
	t.Run("docker", func(t *testing.T) {
		for _, protocol := range []string{"hermes-ssh", "openclaw-ssh"} {
			t.Run(protocol, func(t *testing.T) {
				for occurrence := range 2 {
					t.Run(fmt.Sprint(occurrence), func(t *testing.T) {
						t.Parallel()
						runManagedOccurrence(t, protocol, helper, versions.Bridge.Image, func(identity string) {
							mu.Lock()
							defer mu.Unlock()
							if identities[identity] {
								t.Errorf("runtime reused for repeated task occurrence: %s", identity)
							}
							identities[identity] = true
						})
					})
				}
			})
		}
	})
}
func TestManagedContainerCrashReportsMissingEvidence(t *testing.T) {
	helper := managedBinary(t, "aries-ssh-client", "ARIES_SSH_CLIENT")
	versions, err := config.LoadVersions(filepath.Join(managedRoot(t), "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	runManagedOccurrence(t, "openclaw-ssh", helper, versions.Bridge.Image, func(string) {}, true)
}

func runManagedOccurrence(t *testing.T, protocol, helper, image string, remember func(string), crash ...bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	sandboxRuntime, err := docker.New(docker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := tasksandbox.New(tasksandbox.Options{Deployment: sandboxRuntime, NewEnvironment: sandboxRuntime.NewTaskEnvironment, OutputDir: root})
	if err != nil {
		t.Fatal(err)
	}
	live, err := sandboxes.Start(ctx, core.SandboxRequest{RunID: "managed-integration", TaskID: "repeated-task", Environment: core.Environment{Image: managedFixtureImage, Workdir: "/work", MemoryMB: 64}})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*tasksandbox.Sandbox)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := sandboxes.Stop(cleanup, live); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
		if err := sandboxes.Close(); err != nil {
			t.Errorf("sandbox transport: %v", err)
		}
	})
	// Benchmark services may already be running before the bridge is admitted.
	benchmarkProcess, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "setsid sleep 120 </dev/null >/dev/null 2>&1 & echo $! > /work/benchmark.pid"}})
	if err != nil || benchmarkProcess.ExitCode != 0 {
		t.Fatalf("start benchmark process: %+v %v", benchmarkProcess, err)
	}
	runtime, err := docker.New(docker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := bridgewiring.SSHClientConfig(protocol, helper)
	if err != nil {
		t.Fatal(err)
	}
	launch := bridgewiring.Launch(image)
	launch.RuntimeBackend, launch.ResourceMetrics = "docker", "docker-stats"
	launch.Config.Backend = "docker"
	launch.Config.BackendEndpoint, launch.Request.Mounts = deploymentwiring.DockerExecutionAccess("")
	bridge, err := managed.New(managed.Options{Runtime: runtime, Launch: launch, OutputDir: root, Client: clientConfig, BridgeType: protocol, RetainRawLog: true})
	if err != nil {
		t.Fatal(err)
	}
	expectEvidenceFailure := false
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := bridge.Stop(cleanup); err != nil && !expectEvidenceFailure {
			t.Errorf("bridge cleanup: %v", err)
		}
		if err := runtime.Close(); err != nil {
			t.Errorf("bridge transport: %v", err)
		}
	})
	endpoint, err := bridge.Start(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if protocol == "openclaw-ssh" {
		if info, err := os.Lstat(endpoint.ClientSourceFile); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0555 || endpoint.ClientSourceFile == helper {
			t.Fatal("OpenClaw helper did not satisfy the task-local harness staging contract")
		}
	}
	metadata, err := os.ReadFile(filepath.Join(root, "repeated-task", "bridge", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var owner struct{ RuntimeID, InstanceID, AssignmentID string }
	if err = json.Unmarshal(metadata, &owner); err != nil || owner.RuntimeID == "" {
		t.Fatalf("runtime ownership record: %s %v", metadata, err)
	}
	remember(owner.RuntimeID)
	key, err := os.ReadFile(endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	// Hermes's documented first-use host-key behavior is intentionally preserved.
	hostKey := ssh.InsecureIgnoreHostKey()
	if protocol == "openclaw-ssh" {
		hostKey, err = knownhosts.New(endpoint.KnownHostsSourceFile)
		if err != nil {
			t.Fatal(err)
		}
	}
	connection, err := ssh.Dial("tcp", endpoint.Address, &ssh.ClientConfig{User: endpoint.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: hostKey, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	session, err := connection.NewSession()
	if err != nil {
		connection.Close()
		t.Fatal(err)
	}
	payload := "managed-" + owner.AssignmentID
	// A completed tool call can intentionally leave a background service running.
	script := "setsid sleep 120 </dev/null >/dev/null 2>&1 & echo $! > /work/tool.pid; cat > /work/managed-state; cat /work/managed-state; printf managed-stderr >&2; exit 7"
	wire := managedQuote("/bin/sh") + " " + managedQuote("-c") + " " + managedQuote(script)
	if protocol == "hermes-ssh" {
		wire = "bash -l -c " + managedQuote(script)
	}
	var stdout, stderr bytes.Buffer
	session.Stdin = strings.NewReader(payload)
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Run(wire)
	session.Close()
	defer connection.Close()
	var status *ssh.ExitError
	if !errors.As(err, &status) || status.ExitStatus() != 7 || stdout.String() != payload || stderr.String() != "managed-stderr" {
		t.Fatalf("native exec err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	binarySession, err := connection.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	binaryPayload := []byte{0, 1, 255, 'b', 'i', 'n'}
	binaryScript := "cat > /work/managed-binary; cat /work/managed-binary"
	binaryWire := managedQuote("/bin/sh") + " " + managedQuote("-c") + " " + managedQuote(binaryScript)
	if protocol == "hermes-ssh" {
		binaryWire = "bash -l -c " + managedQuote(binaryScript)
	}
	var binaryOutput bytes.Buffer
	binarySession.Stdin = bytes.NewReader(binaryPayload)
	binarySession.Stdout = &binaryOutput
	if err = binarySession.Run(binaryWire); err != nil || !bytes.Equal(binaryOutput.Bytes(), binaryPayload) {
		t.Fatalf("binary stream differs: %x %v", binaryOutput.Bytes(), err)
	}
	binarySession.Close()
	connection.Close()
	assertSandboxProcessesAlive := func() {
		t.Helper()
		result, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", `for source in /work/benchmark.pid /work/tool.pid; do pid=$(cat "$source") || exit; kill -0 "$pid" || exit; test -e "/proc/$pid/exe" || exit 1; done`}})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("bridge cleanup changed live sandbox processes: %+v %v", result, err)
		}
	}
	assertSandboxProcessesAlive()
	if len(crash) != 0 && crash[0] {
		expectEvidenceFailure = true
		if err = runtime.Stop(ctx, owner.RuntimeID); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err = bridge.Stop(ctx); err == nil || !strings.Contains(err.Error(), "bridge exited without finalized evidence") {
				t.Fatalf("child crash did not report missing finalized evidence: %v", err)
			}
		}
		if _, err = os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("crashed container client key remains")
		}
		assertSandboxProcessesAlive()
		return
	}
	if err = bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	assertSandboxProcessesAlive()
	result, err := sandbox.Exec(ctx, core.Command{Path: "/bin/cat", Args: []string{"/work/managed-state"}})
	if err != nil || result.ExitCode != 0 || result.Stdout != payload {
		t.Fatalf("independent evaluation of same live sandbox: %+v %v", result, err)
	}
	binaryResult, binaryErr := sandbox.Exec(ctx, core.Command{Path: "/usr/bin/base64", Args: []string{"/work/managed-binary"}})
	if binaryErr != nil || binaryResult.ExitCode != 0 || binaryResult.Stdout != "AAH/Ymlu\n" {
		t.Fatalf("independent binary evaluation: %+v %v", binaryResult, binaryErr)
	}
	if _, err = os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bridge client key remains")
	}
	if endpoint.ClientSourceFile != "" {
		if _, err := os.Stat(endpoint.ClientSourceFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private helper staging remains")
		}
	}
	if endpoint.KnownHostsSourceFile != "" {
		if info, err := os.Stat(endpoint.KnownHostsSourceFile); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatal("public bridge host-key evidence is missing or has unsafe permissions")
		}
	}
	if info, err := os.Stat(filepath.Join(root, "repeated-task", "bridge", "known_hosts")); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("public host-key evidence missing after revocation")
	}
	api, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if _, err := api.ContainerInspect(ctx, owner.RuntimeID, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("container removal unconfirmed: %v", err)
	}
	if c, err := net.DialTimeout("tcp", endpoint.Address, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("tool listener remains after revocation")
	}
	for _, path := range endpoint.LogPaths {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("local finalized evidence %s: %v", path, err)
		}
	}
	evidence, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{sandbox.ContainerID(), `"task_id":"repeated-task"`, `"exit_code":7`, payload} {
		if !bytes.Contains(evidence, []byte(value)) {
			t.Fatalf("finalized evidence missing %q: %s", value, evidence)
		}
	}
}
