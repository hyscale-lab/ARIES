package codex

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const testImage = "debian:bookworm-20260812-slim"
const testRollout = "2026/10/06/rollout-2026-10-06T01-02-03-0199b6c1-aaaa-7bbb-8ccc-000000000001.jsonl"

type fakeDeployment struct {
	mu            sync.Mutex
	request       deployment.Request
	secrets       [][]byte
	calls         []string
	archive       []byte
	commands      []core.Command
	stdout        string
	stderr        string
	exit          int
	rollouts      map[string]string
	downloadPath  string
	createErr     error
	uploadErr     error
	validateErr   error
	refuseRemoval bool
	removed       bool
	id            string
}

func (f *fakeDeployment) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeDeployment) Create(_ context.Context, request deployment.Request) (string, error) {
	f.record("create")
	f.request, f.id = request, "codex-id"
	return f.id, f.createErr
}

func (f *fakeDeployment) Validate(_ context.Context, _ string, _ deployment.Request, secrets [][]byte) error {
	f.record("validate")
	f.secrets = nil
	for _, secret := range secrets {
		f.secrets = append(f.secrets, bytes.Clone(secret))
	}
	return f.validateErr
}

func (f *fakeDeployment) UploadArchive(_ context.Context, _, _ string, archive io.Reader) error {
	f.record("upload")
	if f.uploadErr != nil {
		return f.uploadErr
	}
	content, err := io.ReadAll(archive)
	f.archive = content
	return err
}

func (f *fakeDeployment) DownloadArchive(_ context.Context, _ string, source string) (io.ReadCloser, deployment.FileInfo, error) {
	f.downloadPath = source
	if f.rollouts == nil {
		return nil, deployment.FileInfo{}, fmt.Errorf("sessions: %w", runner.ErrNotFound)
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	_ = writer.WriteHeader(&tar.Header{Name: "sessions/", Typeflag: tar.TypeDir, Mode: 0o700})
	for name, content := range f.rollouts {
		_ = writer.WriteHeader(&tar.Header{Name: "sessions/" + name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(content))})
		_, _ = writer.Write([]byte(content))
	}
	_ = writer.Close()
	return io.NopCloser(&output), deployment.FileInfo{}, nil
}

func (f *fakeDeployment) Start(context.Context, string) error { f.record("start"); return nil }
func (f *fakeDeployment) Running(context.Context, string) (bool, error) {
	return !f.removed, nil
}

func (f *fakeDeployment) Exec(_ context.Context, _ string, command core.Command) (core.CommandResult, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()
	if command.Path == codexPath && slices.Equal(command.Args, []string{"--version"}) {
		return core.CommandResult{Stdout: "codex-cli " + supportedVersion + "\n"}, nil
	}
	return core.CommandResult{ExitCode: f.exit, Stdout: f.stdout, Stderr: f.stderr}, nil
}

func (f *fakeDeployment) ExecStream(context.Context, string, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error) {
	return core.CommandResult{ExitCode: -1}, errors.New("unused")
}
func (f *fakeDeployment) LogsStream(context.Context, string, io.Writer, io.Writer) error { return nil }
func (f *fakeDeployment) Logs(context.Context, string, int) ([]byte, error)              { return nil, nil }
func (f *fakeDeployment) Address(context.Context, string, int) (string, error) {
	return "", errors.New("unused")
}

func (f *fakeDeployment) Stop(context.Context, string) error {
	f.record("stop")
	if f.refuseRemoval {
		return errors.New("deployment remains after removal")
	}
	f.removed = true
	return nil
}
func (f *fakeDeployment) Close() error { return nil }

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

func testManager(t *testing.T, settings ...Options) (*Manager, *fakeDeployment, core.HarnessRequest, []byte) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	writeStaticELF(t, bin)
	key := []byte("test-model-secret")
	var options Options
	if len(settings) != 0 {
		options = settings[0]
	}
	fake := &fakeDeployment{stdout: "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"task done\"}}\n{\"type\":\"turn.completed\"}\n", rollouts: map[string]string{testRollout: "{\"type\":\"session_meta\"}\n"}}
	options.CodexPath, options.CodexVersion = bin, "0.157.1"
	options.Runtime = harness.RuntimeOptions{
		Deployment: fake, Image: testImage, OutputDir: filepath.Join(dir, "runs"), StartTimeout: time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return key, true },
	}
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	endpoint := testEndpoint(t)
	endpoint.ClientSourceFile = filepath.Join(dir, "aries-codex-ssh")
	writeStaticELF(t, endpoint.ClientSourceFile)
	return manager, fake, core.HarnessRequest{RunID: "run-1", TaskID: "task-1", Model: testModel(), Endpoint: endpoint, Connectivity: core.HarnessConnectivity{Placement: core.RuntimePlacement{DockerNetwork: "aries-task-net"}}}, key
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
	r := fake.request
	if r.Labels["aries.kind"] != "codex-harness" || r.Labels["aries.attempt"] == "" || r.Placement.DockerNetwork != "aries-task-net" || *r.MemoryMB != memory || *r.CPU != cpu || !r.NoNewPrivileges || !r.DropCapabilities || r.ServicePort != 0 || len(r.ImageVolumes) != 0 || r.AllowImageVolumes {
		t.Fatalf("unsafe runtime request: %#v", r)
	}
	if strings.Contains(fmt.Sprintf("%+v", r), "test-model-secret") {
		t.Fatal("model key in runtime metadata")
	}
	if len(fake.secrets) != 1 || string(fake.secrets[0]) != "test-model-secret" {
		t.Fatal("runtime validation did not receive the model credential to exclude")
	}
	if !slices.Equal(fake.calls[:4], []string{"create", "validate", "upload", "validate"}) {
		t.Fatalf("isolation was not confirmed before credential upload: %v", fake.calls)
	}
	files := map[string][]byte{}
	reader := tar.NewReader(bytes.NewReader(fake.archive))
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
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
	if err := filepath.Walk(manager.runtime.Options.OutputDir, func(path string, info os.FileInfo, err error) error {
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

func TestStartRejectsUnconfirmedIsolationBeforeCredentialCopy(t *testing.T) {
	manager, fake, request, _ := testManager(t)
	fake.validateErr = errors.New("deployment no-new-privileges is not enabled")
	if err := manager.Start(context.Background(), request); err == nil {
		t.Fatal("accepted unconfirmed runtime isolation")
	}
	if len(fake.archive) != 0 || slices.Contains(fake.calls, "upload") || slices.Contains(fake.calls, "start") {
		t.Fatalf("private runtime was copied or started before confirming isolation: %v", fake.calls)
	}
	if !fake.removed {
		t.Fatal("unconfirmed runtime was not rolled back")
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
	last := fake.commands[len(fake.commands)-1]
	cmd := append([]string{last.Path}, last.Args...)
	if last.Path != agentWrapperPath || last.Dir != "/app" || cmd[len(cmd)-1] != instruction || cmd[len(cmd)-2] != "--" || slices.Contains(cmd, "--ignore-user-config") || slices.Contains(cmd, "--ephemeral") || !slices.Contains(cmd, "--json") {
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
	if fake.downloadPath != codexHome+"/sessions" {
		t.Fatalf("copied %q", fake.downloadPath)
	}
	sessions := filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "sessions")
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

func TestRunRejectsCredentialInExecArguments(t *testing.T) {
	manager, fake, request, _ := testManager(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	before := len(fake.commands)
	if _, err := manager.Run(context.Background(), "request containing test-model-secret"); err == nil {
		t.Fatal("credential-bearing instruction accepted")
	}
	if len(fake.commands) != before {
		t.Fatal("credential-bearing instruction reached exec arguments")
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
			fake.uploadErr = errors.New("copy interrupted")
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
