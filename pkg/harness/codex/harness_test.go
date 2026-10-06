package codex

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const testImage = "debian:bookworm-20260812-slim"
const testRollout = "2026/10/06/rollout-2026-10-06T01-02-03-0199b6c1-aaaa-7bbb-8ccc-000000000001.jsonl"

type fakeDocker struct {
	mu                sync.Mutex
	created           client.ContainerCreateOptions
	info              container.InspectResponse
	archive           []byte
	execs             []client.ExecCreateOptions
	removed           bool
	refuseRemoval     bool
	copyErr           error
	createErr         error
	inspectHostConfig func(*container.HostConfig)
	stopCalls         int
	removeCalls       int
	stdout            string
	stderr            string
	exit              int
	rollouts          map[string]string
	copyFromPath      string
}

func (f *fakeDocker) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = options
	f.info = container.InspectResponse{ID: "codex-container", Config: options.Config, HostConfig: options.HostConfig, State: &container.State{}}
	if f.createErr != nil {
		return client.ContainerCreateResult{}, f.createErr
	}
	return client.ContainerCreateResult{ID: f.info.ID}, nil
}
func (f *fakeDocker) CopyToContainer(_ context.Context, id string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if id != "codex-container" || options.DestinationPath != "/" || !options.CopyUIDGID {
		return client.CopyToContainerResult{}, errors.New("wrong staging target")
	}
	content, err := io.ReadAll(options.Content)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archive = content
	return client.CopyToContainerResult{}, errors.Join(err, f.copyErr)
}
func (f *fakeDocker) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.info.State.Running = true
	return client.ContainerStartResult{}, nil
}
func (f *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removed || f.info.ID == "" || id != f.info.ID && id != f.created.Name {
		return client.ContainerInspectResult{}, errdefs.ErrNotFound
	}
	info := f.info
	if f.inspectHostConfig != nil && info.HostConfig != nil {
		host := *info.HostConfig
		f.inspectHostConfig(&host)
		info.HostConfig = &host
	}
	return client.ContainerInspectResult{Container: info}, nil
}
func (f *fakeDocker) ExecCreate(_ context.Context, id string, options client.ExecCreateOptions) (client.ExecCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != f.info.ID || !f.info.State.Running {
		return client.ExecCreateResult{}, errdefs.ErrNotFound
	}
	f.execs = append(f.execs, options)
	return client.ExecCreateResult{ID: fmt.Sprint(len(f.execs))}, nil
}
func (f *fakeDocker) ExecAttach(_ context.Context, id string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	f.mu.Lock()
	var index int
	_, _ = fmt.Sscan(id, &index)
	options := f.execs[index-1]
	stdout, stderr, exitCode := f.stdout, f.stderr, f.exit
	if slices.Contains(options.Cmd, "--version") {
		stdout, stderr, exitCode = "codex-cli 0.157.1\n", "", 0
	}
	f.mu.Unlock()
	read, write := net.Pipe()
	go func() {
		defer write.Close()
		_ = writeMux(write, stdcopy.Stdout, []byte(stdout))
		_ = writeMux(write, stdcopy.Stderr, []byte(stderr))
		_ = writeMux(write, stdcopy.Stderr, []byte(fmt.Sprintf("\x1eARIES_CODEX_EXIT_%s=%d\x1f", options.Cmd[4], exitCode)))
	}()
	return client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(read, "application/vnd.docker.multiplexed-stream")}, nil
}

func (f *fakeDocker) CopyFromContainer(_ context.Context, id string, options client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyFromPath = options.SourcePath
	if id != f.info.ID || f.rollouts == nil {
		return client.CopyFromContainerResult{}, errdefs.ErrNotFound
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	_ = writer.WriteHeader(&tar.Header{Name: "sessions/", Typeflag: tar.TypeDir, Mode: 0o700})
	names := make([]string, 0, len(f.rollouts))
	for name := range f.rollouts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		_ = writer.WriteHeader(&tar.Header{Name: "sessions/" + name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(f.rollouts[name]))})
		_, _ = writer.Write([]byte(f.rollouts[name]))
	}
	_ = writer.Close()
	return client.CopyFromContainerResult{Content: io.NopCloser(&archive)}, nil
}

func writeMux(writer io.Writer, stream stdcopy.StdType, content []byte) error {
	var header [8]byte
	header[0] = byte(stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(content)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(content)
	return err
}
func (f *fakeDocker) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	f.info.State.Running = false
	return client.ContainerStopResult{}, nil
}
func (f *fakeDocker) ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.info.State.Running = false
	return client.ContainerKillResult{}, nil
}
func (f *fakeDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	if !f.refuseRemoval {
		f.removed = true
	}
	return client.ContainerRemoveResult{}, nil
}

func writeStaticELF(t *testing.T, path string) {
	t.Helper()
	content := make([]byte, 64)
	copy(content, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(content[16:], 2)
	binary.LittleEndian.PutUint16(content[18:], 62)
	binary.LittleEndian.PutUint32(content[20:], 1)
	binary.LittleEndian.PutUint16(content[52:], 64)
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
}

func testManager(t *testing.T, settings ...Options) (*Manager, *fakeDocker, core.HarnessRequest, []byte) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	writeStaticELF(t, bin)
	key := []byte("test-model-secret")
	var options Options
	if len(settings) != 0 {
		options = settings[0]
	}
	options.Image, options.CodexPath, options.CodexVersion = testImage, bin, "0.157.1"
	options.OutputDir = filepath.Join(dir, "runs")
	options.APIKeyLookup = func(string) ([]byte, bool) { return key, true }
	options.StartTimeout = time.Second
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	fake := &fakeDocker{stdout: "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"task done\"}}\n{\"type\":\"turn.completed\"}\n", rollouts: map[string]string{testRollout: "{\"type\":\"session_meta\"}\n"}}
	manager.client = fake
	endpoint := testEndpoint(t)
	endpoint.ClientSourceFile = filepath.Join(dir, "aries-codex-ssh")
	writeStaticELF(t, endpoint.ClientSourceFile)
	return manager, fake, core.HarnessRequest{RunID: "run-1", TaskID: "task-1", Model: testModel(), Endpoint: endpoint}, key
}

func TestHarnessStagesSeparateContainerWithoutModelCredentialMetadata(t *testing.T) {
	manager, fake, request, sourceKey := testManager(t)
	cpu, memory := 2.5, 512
	request.CPU, request.MemoryMB = &cpu, &memory
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if !bytes.Equal(sourceKey, make([]byte, len(sourceKey))) {
		t.Fatal("lookup buffer was not cleared")
	}
	if fake.created.Config.Labels["aries.kind"] != "codex-harness" || fake.created.HostConfig.NetworkMode != container.NetworkMode(request.Endpoint.Network) || len(fake.created.HostConfig.Binds) != 0 || len(fake.created.HostConfig.Mounts) != 0 || fake.created.HostConfig.Resources.Memory != int64(memory)<<20 || fake.created.HostConfig.Resources.NanoCPUs != 2500000000 || !slices.Equal(fake.created.HostConfig.CapDrop, []string{"ALL"}) || len(fake.created.HostConfig.CapAdd) != 0 || !slices.Equal(fake.created.HostConfig.SecurityOpt, []string{"no-new-privileges=true"}) {
		t.Fatalf("unsafe container config: %#v", fake.created)
	}
	if strings.Contains(fmt.Sprintf("%+v %+v", fake.created.Config, fake.created.HostConfig), "test-model-secret") {
		t.Fatal("model key in Docker metadata")
	}
	files := map[string][]byte{}
	r := tar.NewReader(bytes.NewReader(fake.archive))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = content
	}
	if string(files[strings.TrimPrefix(modelKeyPath, "/")]) != "test-model-secret" || len(files[strings.TrimPrefix(clientPath, "/")]) == 0 || len(files[strings.TrimPrefix(codexPath, "/")]) == 0 {
		t.Fatal("missing private runtime")
	}
	if _, ok := files["app"]; !ok {
		t.Fatal("missing local empty workdir used by --cd validation")
	}
	if err := filepath.Walk(manager.outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if bytes.Contains(content, []byte("test-model-secret")) {
			t.Errorf("credential retained at %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStartRejectsUnconfirmedPrivilegesBeforeCredentialCopy(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*container.HostConfig)
	}{
		{"missing no-new-privileges", func(host *container.HostConfig) { host.SecurityOpt = nil }},
		{"disabled no-new-privileges", func(host *container.HostConfig) { host.SecurityOpt = []string{"no-new-privileges=false"} }},
		{"conflicting no-new-privileges", func(host *container.HostConfig) {
			host.SecurityOpt = []string{"no-new-privileges=true", "no-new-privileges:false"}
		}},
		{"malformed no-new-privileges", func(host *container.HostConfig) { host.SecurityOpt = []string{"no-new-privileges=invalid"} }},
		{"missing capability drop", func(host *container.HostConfig) { host.CapDrop = nil }},
		{"partial capability drop", func(host *container.HostConfig) { host.CapDrop = []string{"NET_RAW"} }},
		{"capability added back", func(host *container.HostConfig) { host.CapAdd = []string{"SYS_ADMIN"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, fake, request, _ := testManager(t)
			fake.inspectHostConfig = test.mutate
			if err := manager.Start(context.Background(), request); err == nil {
				t.Fatal("accepted unconfirmed container privileges")
			}
			if len(fake.archive) != 0 || fake.info.State.Running {
				t.Fatal("private runtime was copied or started before confirming privileges")
			}
			if !fake.removed {
				t.Fatal("unconfirmed container was not rolled back")
			}
		})
	}
}

func TestStartAcceptsDockerNoNewPrivilegesSpellings(t *testing.T) {
	for _, option := range []string{"no-new-privileges", "no-new-privileges=true", "no-new-privileges:true"} {
		t.Run(option, func(t *testing.T) {
			manager, fake, request, _ := testManager(t)
			fake.inspectHostConfig = func(host *container.HostConfig) { host.SecurityOpt = []string{option} }
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunPreservesArgvAndRetainsRedactedNativeTrajectory(t *testing.T) {
	manager, fake, request, _ := testManager(t)
	fake.stdout = "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"task done test-model-secret\"}}\n{\"type\":\"turn.completed\"}\n"
	fake.stderr = "diagnostic test-model-secret"
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	instruction := "line one\n'quoted' $(echo unsafe) --dangerously-bypass"
	result, err := manager.Run(context.Background(), instruction)
	if err != nil || result.Status != core.StatusSucceeded || result.FinalResponse != "task done [REDACTED]" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	cmd := fake.execs[len(fake.execs)-1].Cmd
	if cmd[len(cmd)-1] != instruction || cmd[len(cmd)-2] != "--" || slices.Contains(cmd, "--ignore-user-config") || slices.Contains(cmd, "--ephemeral") || !slices.Contains(cmd, "--json") {
		t.Fatalf("unexpected argv: %#v", cmd)
	}
	for _, path := range result.LogPaths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, []byte("test-model-secret")) {
			t.Fatalf("secret in %s", path)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("nonprivate artifact %s", path)
		}
	}
	if _, err := manager.Run(context.Background(), "again"); err == nil {
		t.Fatal("second task accepted")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunRetainsCompleteRedactedNativeRollouts(t *testing.T) {
	manager, fake, request, _ := testManager(t)
	child := "2026/10/06/rollout-2026-10-06T01-02-04-0199b6c1-aaaa-7bbb-8ccc-000000000002.jsonl"
	parent := "{\"type\":\"session_meta\",\"payload\":{\"base_instructions\":\"full prompt\"}}\n{\"type\":\"response_item\",\"payload\":{\"output\":\"tool said test-model-secret\"}}\n"
	fake.rollouts = map[string]string{testRollout: parent, child: "{\"type\":\"session_meta\"}\n", "2026/10/06/notes.txt": "ignored"}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "do it")
	if err != nil || result.Status != core.StatusSucceeded {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if fake.copyFromPath != codexHome+"/sessions" {
		t.Fatalf("copied %q", fake.copyFromPath)
	}
	sessions := filepath.Join(manager.outputDir, request.TaskID, "harness", "sessions")
	for name, want := range map[string]string{filepath.Base(testRollout): strings.ReplaceAll(parent, "test-model-secret", "[REDACTED]"), filepath.Base(child): "{\"type\":\"session_meta\"}\n"} {
		filename := filepath.Join(sessions, name)
		content, err := os.ReadFile(filename)
		if err != nil || string(content) != want {
			t.Fatalf("%s = %q, %v", name, content, err)
		}
		if !slices.Contains(result.LogPaths, filename) {
			t.Fatalf("rollout %s missing from log paths %v", name, result.LogPaths)
		}
	}
	if entries, _ := os.ReadDir(sessions); len(entries) != 2 {
		t.Fatalf("unexpected session artifacts: %v", entries)
	}
}

func TestRunRequiresRolloutOnlyAfterSuccessfulExec(t *testing.T) {
	manager, fake, request, _ := testManager(t)
	fake.rollouts = nil
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "do it")
	if err == nil || result.Status != core.StatusFailed || !strings.Contains(err.Error(), "retain Codex rollout") {
		t.Fatalf("missing rollout accepted: result=%#v err=%v", result, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	for name, rollouts := range map[string]map[string]string{"absent sessions": nil, "empty sessions": {}} {
		t.Run(name, func(t *testing.T) {
			manager, fake, request, _ := testManager(t)
			fake.rollouts, fake.exit = rollouts, 1
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			defer manager.Stop(context.Background())
			_, err := manager.Run(context.Background(), "do it")
			if err == nil || strings.Contains(err.Error(), "rollout") {
				t.Fatalf("failed exec error should not blame an absent rollout: %v", err)
			}
		})
	}
}

func TestRunRejectsCredentialInDockerExecArguments(t *testing.T) {
	manager, fake, request, _ := testManager(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	before := len(fake.execs)
	if _, err := manager.Run(context.Background(), "request containing test-model-secret"); err == nil {
		t.Fatal("credential-bearing instruction accepted")
	}
	if len(fake.execs) != before {
		t.Fatal("credential-bearing instruction reached Docker exec metadata")
	}
}

func TestStopRequiresPositiveAbsenceAndSupportsRetry(t *testing.T) {
	manager, fake, request, _ := testManager(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fake.refuseRemoval = true
	if err := manager.Stop(context.Background()); err == nil {
		t.Fatal("unconfirmed removal succeeded")
	}
	fake.refuseRemoval = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("fresh cleanup context failed: %v", err)
	}
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !fake.removed || manager.active != nil {
		t.Fatal("container/session remains")
	}
}

func TestPartialStartAndAmbiguousCreateCleanOwnedContainer(t *testing.T) {
	for _, createFailure := range []bool{false, true} {
		manager, fake, request, _ := testManager(t)
		if createFailure {
			fake.createErr = errors.New("create response lost")
		} else {
			fake.copyErr = errors.New("copy interrupted")
		}
		if err := manager.Start(context.Background(), request); err == nil {
			t.Fatal("partial start accepted")
		}
		if !fake.removed {
			t.Fatal("partial container leaked")
		}
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStopRefusesContainerWithWrongOwnership(t *testing.T) {
	manager, fake, request, _ := testManager(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fake.info.Config.Labels["aries.attempt"] = "another-attempt"
	if err := manager.Stop(context.Background()); err == nil {
		t.Fatal("unowned stop accepted")
	}
	if fake.removeCalls != 0 || fake.stopCalls != 0 {
		t.Fatal("unowned container was mutated")
	}
}
