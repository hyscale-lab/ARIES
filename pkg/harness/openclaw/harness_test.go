package openclaw

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

type fakeDocker struct {
	mu                        sync.Mutex
	commands                  [][]string
	secret                    []byte
	secretStaged              bool
	ready                     bool
	agentLaunched             bool
	agentNeverFinishes        bool
	missingAgentStdout        bool
	stopped                   bool
	killed                    bool
	removedContainers         map[string]bool
	removedVolumes            map[string]bool
	containerNames            map[string]string
	volumeLabels              map[string]map[string]string
	failLogsOnce              bool
	failHarnessStart          bool
	failHarnessCreate         bool
	failHarnessInspect        bool
	alwaysFailHarnessInspect  bool
	hiddenHarnessInspects     int
	hiddenInitializerInspects int
	failConfigInspect         bool
	alwaysFailConfigInspect   bool
	hiddenConfigInspects      int
	failGracefulStop          bool
	failHarnessRemove         bool
	failVolumeRemove          bool
	alwaysFailVolume          bool
	foreignHarness            bool
	foreignInitializer        bool
	foreignConfig             bool
	containerRunLabel         string
	omitContainerRunLabel     bool
	volumeRunLabel            string
	omitVolumeRunLabel        bool
}

func newFakeDocker(secret []byte) *fakeDocker {
	return &fakeDocker{
		secret: secret, removedContainers: make(map[string]bool), removedVolumes: make(map[string]bool),
		containerNames: make(map[string]string), volumeLabels: make(map[string]map[string]string),
	}
}

func (fake *fakeDocker) Run(_ context.Context, stdin []byte, args ...string) (commandResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	copyArgs := append([]string(nil), args...)
	fake.commands = append(fake.commands, copyArgs)
	if bytes.Contains([]byte(strings.Join(args, "\x00")), fake.secret) {
		return commandResult{exitCode: 125, stderr: []byte("secret in args")}, errors.New("secret in args")
	}
	joined := strings.Join(args, " ")
	switch {
	case len(args) >= 2 && args[0] == "volume" && args[1] == "create":
		name := args[len(args)-1]
		labels := commandLabels(args)
		if fake.foreignConfig && strings.Contains(name, "-config-") {
			labels["aries.attempt"] = "foreign-attempt"
		}
		if fake.omitVolumeRunLabel {
			delete(labels, "aries.run")
		} else if fake.volumeRunLabel != "" {
			labels["aries.run"] = fake.volumeRunLabel
		}
		fake.volumeLabels[name] = labels
		return commandResult{stdout: []byte(name + "\n")}, nil
	case len(args) >= 3 && args[0] == "container" && args[1] == "create":
		name := commandFlagValue(args, "--name")
		id := "harness-container-id"
		if strings.Contains(name, "-init-") {
			id = "initializer-container-id"
		}
		fake.containerNames[name] = id
		fake.containerNames[id] = id
		if fake.foreignHarness && id == "harness-container-id" {
			return commandResult{exitCode: 125, stderr: []byte("name already in use")}, errors.New("name already in use")
		}
		if fake.foreignInitializer && id == "initializer-container-id" {
			return commandResult{exitCode: 125, stderr: []byte("name already in use")}, errors.New("name already in use")
		}
		if fake.failHarnessCreate && id == "harness-container-id" {
			fake.failHarnessCreate = false
			return commandResult{exitCode: 125, stderr: []byte("ambiguous create response")}, errors.New("ambiguous create response")
		}
		return commandResult{stdout: []byte(id + "\n")}, nil
	case len(args) == 4 && args[0] == "container" && args[1] == "cp" && args[2] == "-":
		if !bytes.Contains(stdin, fake.secret) {
			return commandResult{exitCode: 1, stderr: []byte("secret absent from private stage")}, errors.New("secret absent")
		}
		fake.secretStaged = true
		return commandResult{}, nil
	case len(args) == 3 && args[0] == "container" && args[1] == "start":
		if fake.failHarnessStart && args[2] == "harness-container-id" {
			fake.failHarnessStart = false
			return commandResult{exitCode: 1, stderr: []byte("transient start failure")}, errors.New("transient start failure")
		}
		return commandResult{}, nil
	case len(args) == 4 && args[0] == "container" && args[1] == "cp" && args[3] == "-":
		path := strings.SplitN(args[2], ":", 2)[1]
		switch {
		case path == "/tmp/aries-init-status":
			return commandResult{stdout: tarSingleFile("aries-init-status", []byte("ok"))}, nil
		case strings.HasSuffix(path, "/.aries/ready") && fake.ready:
			return commandResult{stdout: tarSingleFile("ready", []byte(`{"status":200,"ready":true,"uid":1000}`))}, nil
		case strings.HasSuffix(path, "/run/status") && fake.agentLaunched && !fake.agentNeverFinishes:
			return commandResult{stdout: tarSingleFile("status", []byte("0"))}, nil
		case strings.HasSuffix(path, "/run/stdout") && fake.agentLaunched:
			if fake.missingAgentStdout {
				return commandResult{exitCode: 1, stderr: []byte("Could not find the file")}, errors.New("missing")
			}
			return commandResult{stdout: tarSingleFile("stdout", []byte(`{"status":"ok","result":{"payloads":[{"text":"done"}]}}`))}, nil
		case strings.HasSuffix(path, "/run/stderr") && fake.agentLaunched:
			return commandResult{stdout: tarSingleFile("stderr", []byte("diagnostic\n"))}, nil
		case strings.HasSuffix(path, "/agents/main/sessions"):
			return commandResult{exitCode: 1, stderr: []byte("Could not find the file")}, errors.New("missing")
		default:
			return commandResult{exitCode: 1, stderr: []byte("Could not find the file")}, errors.New("missing")
		}
	case len(args) >= 3 && args[0] == "container" && args[1] == "exec" && strings.Contains(joined, "/.aries/ready"):
		fake.ready = true
		return commandResult{}, nil
	case len(args) >= 3 && args[0] == "container" && args[1] == "exec":
		fake.agentLaunched = true
		return commandResult{}, nil
	case len(args) >= 3 && args[0] == "container" && args[1] == "inspect":
		id := fake.containerNames[args[2]]
		if id == "" || fake.removedContainers[id] {
			return commandResult{exitCode: 1, stderr: []byte("No such container")}, errors.New("missing")
		}
		if id == "harness-container-id" && fake.hiddenHarnessInspects > 0 {
			fake.hiddenHarnessInspects--
			return commandResult{exitCode: 1, stderr: []byte("No such container")}, errors.New("missing")
		}
		if id == "initializer-container-id" && fake.hiddenInitializerInspects > 0 {
			fake.hiddenInitializerInspects--
			return commandResult{exitCode: 1, stderr: []byte("No such container")}, errors.New("missing")
		}
		if id == "harness-container-id" && (fake.failHarnessInspect || fake.alwaysFailHarnessInspect) {
			fake.failHarnessInspect = false
			return commandResult{exitCode: 1, stderr: []byte("transient inspect failure")}, errors.New("transient inspect failure")
		}
		kind := "openclaw-harness"
		if id == "initializer-container-id" {
			kind = "openclaw-initializer"
		}
		foreign := fake.foreignHarness && kind == "openclaw-harness" || fake.foreignInitializer && kind == "openclaw-initializer"
		inspection := []containerInspection{fakeInspection(id, kind, foreign, fake.containerRunLabel, fake.omitContainerRunLabel)}
		content, _ := json.Marshal(inspection)
		return commandResult{stdout: content}, nil
	case len(args) >= 3 && args[0] == "container" && args[1] == "top":
		if fake.stopped || fake.killed {
			return commandResult{exitCode: 1, stderr: []byte("container is not running")}, errors.New("not running")
		}
		return commandResult{stdout: []byte("PID ARGS\n42 openclaw\n")}, nil
	case len(args) >= 3 && args[0] == "container" && args[1] == "stop":
		if fake.failGracefulStop {
			fake.failGracefulStop = false
			return commandResult{exitCode: 1, stderr: []byte("graceful stop failed")}, errors.New("graceful stop failed")
		}
		fake.stopped = true
		return commandResult{}, nil
	case len(args) >= 3 && args[0] == "container" && args[1] == "kill":
		fake.killed = true
		fake.stopped = true
		return commandResult{}, nil
	case len(args) >= 3 && args[0] == "container" && args[1] == "logs":
		if fake.failLogsOnce {
			fake.failLogsOnce = false
			return commandResult{exitCode: 1, stderr: []byte("transient log failure")}, errors.New("transient log failure")
		}
		return commandResult{stdout: append([]byte("gateway ready "), fake.secret...)}, nil
	case len(args) >= 4 && args[0] == "container" && args[1] == "rm":
		if fake.failHarnessRemove && args[len(args)-1] == "harness-container-id" {
			fake.failHarnessRemove = false
			return commandResult{exitCode: 1, stderr: []byte("transient container removal failure")}, errors.New("transient container removal failure")
		}
		fake.removedContainers[fake.containerNames[args[len(args)-1]]] = true
		return commandResult{}, nil
	case len(args) == 3 && args[0] == "volume" && args[1] == "rm":
		if fake.alwaysFailVolume || fake.failVolumeRemove {
			fake.failVolumeRemove = false
			return commandResult{exitCode: 1, stderr: []byte("busy")}, errors.New("busy")
		}
		fake.removedVolumes[args[2]] = true
		return commandResult{}, nil
	case len(args) == 3 && args[0] == "volume" && args[1] == "inspect":
		if fake.removedVolumes[args[2]] {
			return commandResult{exitCode: 1, stderr: []byte("No such volume")}, errors.New("missing")
		}
		if strings.Contains(args[2], "-config-") && fake.hiddenConfigInspects > 0 {
			fake.hiddenConfigInspects--
			return commandResult{exitCode: 1, stderr: []byte("No such volume")}, errors.New("missing")
		}
		if strings.Contains(args[2], "-config-") && (fake.failConfigInspect || fake.alwaysFailConfigInspect) {
			fake.failConfigInspect = false
			return commandResult{exitCode: 1, stderr: []byte("transient inspect failure")}, errors.New("transient inspect failure")
		}
		labels, ok := fake.volumeLabels[args[2]]
		if !ok {
			return commandResult{exitCode: 1, stderr: []byte("No such volume")}, errors.New("missing")
		}
		content, _ := json.Marshal([]volumeInspection{{Name: args[2], Labels: labels}})
		return commandResult{stdout: content}, nil
	default:
		return commandResult{exitCode: 125, stderr: []byte("unexpected fake Docker command: " + joined)}, fmt.Errorf("unexpected command")
	}
}

func fakeInspection(id, kind string, foreign bool, runLabel string, omitRunLabel bool) containerInspection {
	var inspection containerInspection
	inspection.ID = id
	inspection.Config.Image = PinnedImage
	inspection.Config.Labels = map[string]string{
		"aries.managed": "true", "aries.milestone": "m5", "aries.task": "fix-git", "aries.run": "test-run", "aries.kind": kind, "aries.attempt": "fixedid",
	}
	if omitRunLabel {
		delete(inspection.Config.Labels, "aries.run")
	} else if runLabel != "" {
		inspection.Config.Labels["aries.run"] = runLabel
	}
	if foreign {
		inspection.Config.Labels["aries.attempt"] = "foreign-attempt"
	}
	if kind == "openclaw-harness" {
		inspection.Config.User = "node"
		inspection.Config.Env = []string{"NODE_ENV=production", "OPENCLAW_CONFIG_PATH=" + configContainerPath}
		inspection.Config.Cmd = append([]string{launcherPath}, gatewayCommand...)
		inspection.Config.Entrypoint = append([]string(nil), upstreamEntrypoint...)
		inspection.NetworkSettings.Networks = map[string]json.RawMessage{"aries-net-test": nil}
		inspection.Mounts = []struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}{
			{Type: "volume", Destination: "/run/aries", RW: false},
			{Type: "volume", Destination: stateContainerPath, RW: true},
			{Type: "bind", Destination: "/opt/aries/bin/aries-ssh", RW: false},
		}
	}
	return inspection
}

func commandFlagValue(args []string, flag string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	return ""
}

func commandLabels(args []string) map[string]string {
	labels := make(map[string]string)
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "--label" {
			continue
		}
		name, value, ok := strings.Cut(args[index+1], "=")
		if ok {
			labels[name] = value
		}
		index++
	}
	return labels
}

func tarSingleFile(name string, content []byte) []byte {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	_ = writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(content))})
	_, _ = writer.Write(content)
	_ = writer.Close()
	return output.Bytes()
}

func newTestManager(t *testing.T, fake *fakeDocker) (*Manager, core.HarnessRequest) {
	t.Helper()
	lookup := func(name string) ([]byte, bool) {
		if name != "ARIES_FAKE_API_KEY" {
			t.Fatalf("lookup environment = %q", name)
		}
		return bytes.Clone(fake.secret), true
	}
	manager, err := New(Options{Image: PinnedImage, OutputDir: t.TempDir(), APIKeyLookup: lookup, CleanupTimeout: 2 * time.Second, StartTimeout: time.Second, AgentTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	manager.cli = fake
	manager.newID = func() (string, error) { return "fixedid", nil }
	dir := t.TempDir()
	client := filepath.Join(dir, "aries-ssh")
	identity := filepath.Join(dir, "id_ed25519")
	knownHosts := filepath.Join(dir, "known_hosts")
	for path, file := range map[string]struct {
		content string
		mode    os.FileMode
	}{
		client:     {content: "static-client", mode: 0o555},
		identity:   {content: "private-key", mode: 0o600},
		knownHosts: {content: "host-key", mode: 0o600},
	} {
		if err := os.WriteFile(path, []byte(file.content), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := testEndpoint()
	endpoint.ClientSourceFile = client
	endpoint.IdentitySourceFile = identity
	endpoint.KnownHostsSourceFile = knownHosts
	return manager, core.HarnessRequest{RunID: "test-run", TaskID: "fix-git", Endpoint: endpoint, Model: testModel(), OutputDir: manager.outputDir}
}

func TestHarnessLifecycleUsesPrivateStageExactArgvAndRedactedArtifacts(t *testing.T) {
	secret := []byte("dummy-secret-never-persist")
	fake := newFakeDocker(secret)
	manager, request := newTestManager(t, fake)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "repair the repository")
	if err != nil || result.Status != core.StatusSucceeded || result.FinalResponse != "done" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !fake.secretStaged {
		t.Fatal("API key was not delivered through the private stdin stage")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("repeated Stop() = %v", err)
	}
	for _, path := range result.LogPaths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, secret) {
			t.Fatalf("secret persisted in %s", path)
		}
	}
	if len(result.LogPaths) != 4 {
		t.Fatalf("harness log paths = %q", result.LogPaths)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	foundAgent := false
	foundTiniPreservingCreate := false
	for _, command := range fake.commands {
		joined := strings.Join(command, "\x00")
		if strings.Contains(joined, "node\x00openclaw.mjs\x00agent\x00--session-key\x00agent:main:aries-fix-git\x00--message\x00repair the repository\x00--json\x00--timeout\x0060") {
			foundAgent = true
		}
		if bytes.Contains([]byte(joined), secret) {
			t.Fatal("secret entered Docker command arguments")
		}
		if len(command) >= 2 && (command[0] == "volume" && command[1] == "create" || command[0] == "container" && command[1] == "create") {
			if got := commandLabels(command)["aries.run"]; got != "test-run" {
				t.Fatalf("resource create run label = %q, want test-run: %q", got, command)
			}
		}
		if strings.Contains(joined, "\x00--thinking\x00") {
			t.Fatal("fake-model agent argv changed with a thinking flag")
		}
		if strings.Contains(joined, "container\x00create\x00--name\x00aries-openclaw-fixedid") {
			if strings.Contains(joined, "\x00--entrypoint\x00") {
				t.Fatal("harness create replaced the upstream tini entrypoint")
			}
			wanted := PinnedImage + "\x00" + launcherPath + "\x00" + strings.Join(gatewayCommand, "\x00")
			foundTiniPreservingCreate = strings.HasSuffix(joined, wanted)
		}
	}
	if !foundAgent {
		t.Fatal("exact one-turn OpenClaw agent argv was not issued")
	}
	if !foundTiniPreservingCreate {
		t.Fatal("harness create did not preserve tini and launch through the private command")
	}
}

func TestHarnessRejectsStatusWithoutCompleteOutput(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	fake.missingAgentStdout = true
	manager, request := newTestManager(t, fake)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "repair")
	if err == nil || result.Status != core.StatusFailed || !strings.Contains(err.Error(), "complete output artifacts") {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessStopRetainsContainerUntilArtifactRetry(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	manager, request := newTestManager(t, fake)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "repair")
	if err != nil {
		t.Fatal(err)
	}
	fake.failLogsOnce = true
	if err := manager.Stop(context.Background()); err == nil {
		t.Fatal("artifact collection failure was hidden")
	}
	fake.mu.Lock()
	removed := fake.removedContainers["harness-container-id"]
	fake.mu.Unlock()
	if removed {
		t.Fatal("harness container was removed before artifact collection succeeded")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop() = %v", err)
	}
	for _, path := range result.LogPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("artifact %q after retry: %v", path, err)
		}
	}
}

func TestHarnessStopCanRecollectIdenticalArtifactsAfterRemovalFailure(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	manager, request := newTestManager(t, fake)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), "repair"); err != nil {
		t.Fatal(err)
	}
	fake.failHarnessRemove = true
	if err := manager.Stop(context.Background()); err == nil {
		t.Fatal("container removal failure was hidden")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop() = %v", err)
	}
}

func TestHarnessCancellationAndRetryableStop(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	fake.agentNeverFinishes = true
	manager, request := newTestManager(t, fake)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := manager.Run(ctx, "repair")
	if !errors.Is(err, context.Canceled) || result.Status != core.StatusCanceled {
		t.Fatalf("canceled Run() = %#v, %v", result, err)
	}
	fake.failVolumeRemove = true
	if err := manager.Stop(context.Background()); err == nil {
		t.Fatal("volume removal failure was hidden")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop() = %v", err)
	}
}

func TestFailedStartRetriesCleanupUntilNoOwnedResourceRemains(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	fake.failHarnessStart = true
	fake.failHarnessRemove = true
	fake.failVolumeRemove = true
	manager, request := newTestManager(t, fake)
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "transient start failure") {
		t.Fatalf("Start() = %v", err)
	}
	if manager.active != nil {
		t.Fatal("successful internal rollback retained an active session")
	}
	for _, id := range []string{"initializer-container-id", "harness-container-id"} {
		if !fake.removedContainers[id] {
			t.Fatalf("owned container %q was orphaned", id)
		}
	}
	for _, name := range []string{"aries-openclaw-config-fixedid", "aries-openclaw-state-fixedid"} {
		if !fake.removedVolumes[name] {
			t.Fatalf("owned volume %q was orphaned", name)
		}
	}
}

func TestCreateOwnershipProofRetriesTransientInspectionFailures(t *testing.T) {
	for name, inject := range map[string]func(*fakeDocker){
		"container": func(fake *fakeDocker) { fake.failHarnessInspect = true },
		"volume":    func(fake *fakeDocker) { fake.failConfigInspect = true },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy-secret"))
			inject(fake)
			manager, request := newTestManager(t, fake)
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatalf("Start() after transient ownership inspection failure = %v", err)
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatalf("Stop() = %v", err)
			}
		})
	}
}

func TestCreateOwnershipProofRetriesDelayedVisibility(t *testing.T) {
	for name, inject := range map[string]func(*fakeDocker){
		"volume":      func(fake *fakeDocker) { fake.hiddenConfigInspects = 1 },
		"initializer": func(fake *fakeDocker) { fake.hiddenInitializerInspects = 1 },
		"harness":     func(fake *fakeDocker) { fake.hiddenHarnessInspects = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy-secret"))
			inject(fake)
			manager, request := newTestManager(t, fake)
			manager.cleanupTimeout = 250 * time.Millisecond
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatalf("Start() after delayed Docker visibility = %v", err)
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatalf("Stop() = %v", err)
			}
		})
	}
}

func TestFailedStartCleansOwnedContainerAfterDelayedVisibility(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	fake.failHarnessCreate = true
	fake.hiddenHarnessInspects = 1
	manager, request := newTestManager(t, fake)
	manager.cleanupTimeout = 250 * time.Millisecond
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "ambiguous create response") {
		t.Fatalf("Start() = %v", err)
	}
	if !fake.removedContainers["harness-container-id"] || manager.active != nil {
		t.Fatal("delayed-visible owned container was not cleaned after failed Start")
	}
}

func TestStopSessionRetriesDelayedTentativeVisibilityBeforeCleanup(t *testing.T) {
	for name, foreign := range map[string]bool{"owned cleanup": false, "foreign untouched": true} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy-secret"))
			fake.containerNames["aries-openclaw-fixedid"] = "harness-container-id"
			fake.containerNames["harness-container-id"] = "harness-container-id"
			fake.hiddenHarnessInspects = 1
			fake.foreignHarness = foreign
			manager, _ := newTestManager(t, fake)
			manager.cleanupTimeout = 250 * time.Millisecond
			active := &session{
				runID: "test-run", taskID: "fix-git", safeTaskID: "fix-git", attemptID: "fixedid",
				containerName: "aries-openclaw-fixedid", containerID: "harness-container-id", containerTentative: true,
				apiKey: []byte("dummy-secret"), gatewayToken: []byte("gateway-secret"),
			}
			ctx, cancel := context.WithTimeout(context.Background(), manager.cleanupTimeout)
			defer cancel()
			if err := manager.stopSession(ctx, active, false); err != nil {
				t.Fatalf("stopSession() = %v", err)
			}
			if foreign {
				if fake.stopped || fake.killed || fake.removedContainers["harness-container-id"] {
					t.Fatal("delayed-visible foreign container was destructively targeted")
				}
			} else if !fake.removedContainers["harness-container-id"] {
				t.Fatal("delayed-visible exact-owned container was not cleaned")
			}
		})
	}
}

func TestFailedStartRetainsTentativeContainerUntilStopCanProveOwnership(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	fake.alwaysFailHarnessInspect = true
	manager, request := newTestManager(t, fake)
	manager.cleanupTimeout = 75 * time.Millisecond
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "rollback partial OpenClaw harness") {
		t.Fatalf("Start() = %v", err)
	}
	if manager.active == nil || !manager.active.containerTentative || manager.active.containerOwned || manager.active.containerID != "harness-container-id" {
		t.Fatalf("tentative container record = %#v", manager.active)
	}
	for _, command := range fake.commands {
		joined := strings.Join(command, " ")
		if strings.Contains(joined, "harness-container-id") && (strings.Contains(joined, "container stop") || strings.Contains(joined, "container kill") || strings.Contains(joined, "container rm")) {
			t.Fatalf("destructive command ran before exact ownership proof: %q", command)
		}
	}
	fake.alwaysFailHarnessInspect = false
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() after ownership inspection recovered = %v", err)
	}
	if !fake.removedContainers["harness-container-id"] || manager.active != nil {
		t.Fatal("re-proven tentative container was not cleaned up")
	}
}

func TestFailedStartRetainsTentativeVolumeUntilStopCanProveOwnership(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	fake.alwaysFailConfigInspect = true
	manager, request := newTestManager(t, fake)
	manager.cleanupTimeout = 75 * time.Millisecond
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "rollback partial OpenClaw harness") {
		t.Fatalf("Start() = %v", err)
	}
	if manager.active == nil || !manager.active.configTentative || manager.active.configOwned {
		t.Fatalf("tentative volume record = %#v", manager.active)
	}
	for _, command := range fake.commands {
		if len(command) >= 2 && command[0] == "volume" && command[1] == "rm" {
			t.Fatalf("destructive command ran before exact volume ownership proof: %q", command)
		}
	}
	fake.alwaysFailConfigInspect = false
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() after volume ownership inspection recovered = %v", err)
	}
	if !fake.removedVolumes["aries-openclaw-config-fixedid"] || manager.active != nil {
		t.Fatal("re-proven tentative volume was not cleaned up")
	}
}

func TestFailedStartRetainsOwnershipWhenRollbackBudgetExpires(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	fake.failHarnessStart = true
	fake.alwaysFailVolume = true
	manager, request := newTestManager(t, fake)
	manager.cleanupTimeout = 75 * time.Millisecond
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "rollback partial OpenClaw harness") {
		t.Fatalf("Start() = %v", err)
	}
	if manager.active == nil || len(manager.active.apiKey) == 0 || len(manager.active.gatewayToken) == 0 {
		t.Fatal("failed rollback discarded ownership or cleanup credentials")
	}
	fake.alwaysFailVolume = false
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() after retained failed-Start ownership = %v", err)
	}
	if manager.active != nil {
		t.Fatal("successful cleanup retained failed-Start ownership")
	}
}

func TestAmbiguousCreateRemovesOnlyNonceOwnedContainer(t *testing.T) {
	fake := newFakeDocker([]byte("dummy-secret"))
	fake.failHarnessCreate = true
	manager, request := newTestManager(t, fake)
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "ambiguous create response") {
		t.Fatalf("Start() = %v", err)
	}
	if !fake.removedContainers["harness-container-id"] {
		t.Fatal("nonce-owned container from ambiguous create was not removed")
	}
}

func TestForeignNameCollisionsAreNeverRemoved(t *testing.T) {
	t.Run("initializer", func(t *testing.T) {
		fake := newFakeDocker([]byte("dummy-secret"))
		fake.foreignInitializer = true
		fake.hiddenInitializerInspects = 1
		manager, request := newTestManager(t, fake)
		if err := manager.Start(context.Background(), request); err == nil {
			t.Fatal("foreign initializer collision was accepted")
		}
		if fake.removedContainers["initializer-container-id"] {
			t.Fatal("foreign initializer collision was removed")
		}
		for _, command := range fake.commands {
			joined := strings.Join(command, " ")
			if strings.Contains(joined, "initializer-container-id") && (strings.Contains(joined, "container stop") || strings.Contains(joined, "container kill") || strings.Contains(joined, "container rm")) {
				t.Fatalf("destructive command targeted foreign initializer: %q", command)
			}
		}
	})

	t.Run("container", func(t *testing.T) {
		fake := newFakeDocker([]byte("dummy-secret"))
		fake.foreignHarness = true
		fake.hiddenHarnessInspects = 1
		manager, request := newTestManager(t, fake)
		if err := manager.Start(context.Background(), request); err == nil {
			t.Fatal("foreign harness collision was accepted")
		}
		if fake.removedContainers["harness-container-id"] {
			t.Fatal("foreign harness collision was removed")
		}
		for _, command := range fake.commands {
			joined := strings.Join(command, " ")
			if strings.Contains(joined, "harness-container-id") && (strings.Contains(joined, "container stop") || strings.Contains(joined, "container kill") || strings.Contains(joined, "container rm")) {
				t.Fatalf("destructive command targeted foreign harness: %q", command)
			}
		}
	})

	t.Run("volume", func(t *testing.T) {
		fake := newFakeDocker([]byte("dummy-secret"))
		fake.foreignConfig = true
		fake.hiddenConfigInspects = 1
		manager, request := newTestManager(t, fake)
		if err := manager.Start(context.Background(), request); err == nil {
			t.Fatal("foreign config-volume collision was accepted")
		}
		if fake.removedVolumes["aries-openclaw-config-fixedid"] {
			t.Fatal("foreign config-volume collision was removed")
		}
	})

	for name, configure := range map[string]func(*fakeDocker){
		"initializer missing run label": func(fake *fakeDocker) { fake.omitContainerRunLabel = true },
		"initializer wrong run label":   func(fake *fakeDocker) { fake.containerRunLabel = "other-run" },
		"volume missing run label":      func(fake *fakeDocker) { fake.omitVolumeRunLabel = true },
		"volume wrong run label":        func(fake *fakeDocker) { fake.volumeRunLabel = "other-run" },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy-secret"))
			configure(fake)
			manager, request := newTestManager(t, fake)
			if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "not owned") {
				t.Fatalf("foreign run label Start() error = %v", err)
			}
			if strings.HasPrefix(name, "initializer") && fake.removedContainers["initializer-container-id"] {
				t.Fatal("initializer with a foreign or missing run label was removed")
			}
			if strings.HasPrefix(name, "volume") && fake.removedVolumes["aries-openclaw-config-fixedid"] {
				t.Fatal("volume with a foreign or missing run label was removed")
			}
		})
	}
}

func TestHarnessGracefulStopUsesKillOnlyAsFallback(t *testing.T) {
	for name, failGraceful := range map[string]bool{"graceful": false, "kill fallback": true} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy-secret"))
			manager, request := newTestManager(t, fake)
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Run(context.Background(), "repair"); err != nil {
				t.Fatal(err)
			}
			fake.failGracefulStop = failGraceful
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !fake.stopped || fake.killed != failGraceful {
				t.Fatalf("stopped/killed = %v/%v, want true/%v", fake.stopped, fake.killed, failGraceful)
			}
		})
	}
}

func TestStopRefusesOwnershipDriftBeforeDestructiveCommands(t *testing.T) {
	t.Run("container labels", func(t *testing.T) {
		fake := newFakeDocker([]byte("dummy-secret"))
		manager, request := newTestManager(t, fake)
		if err := manager.Start(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		fake.foreignHarness = true
		if err := manager.Stop(context.Background()); err == nil {
			t.Fatal("container ownership drift was ignored")
		}
		if fake.stopped || fake.killed || fake.removedContainers["harness-container-id"] {
			t.Fatal("destructive command targeted a container after ownership drift")
		}
		fake.foreignHarness = false
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("volume labels", func(t *testing.T) {
		fake := newFakeDocker([]byte("dummy-secret"))
		manager, request := newTestManager(t, fake)
		if err := manager.Start(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		fake.volumeLabels["aries-openclaw-state-fixedid"]["aries.attempt"] = "foreign-attempt"
		if err := manager.Stop(context.Background()); err == nil {
			t.Fatal("volume ownership drift was ignored")
		}
		if fake.removedVolumes["aries-openclaw-state-fixedid"] {
			t.Fatal("destructive command targeted a volume after ownership drift")
		}
		fake.volumeLabels["aries-openclaw-state-fixedid"]["aries.attempt"] = "fixedid"
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	for name, configure := range map[string]func(*fakeDocker){
		"container missing run label": func(fake *fakeDocker) { fake.omitContainerRunLabel = true },
		"container wrong run label":   func(fake *fakeDocker) { fake.containerRunLabel = "other-run" },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy-secret"))
			manager, request := newTestManager(t, fake)
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			configure(fake)
			if err := manager.Stop(context.Background()); err == nil {
				t.Fatal("container run-label drift was ignored")
			}
			if fake.stopped || fake.killed || fake.removedContainers["harness-container-id"] {
				t.Fatal("destructive command targeted a container after run-label drift")
			}
			fake.omitContainerRunLabel = false
			fake.containerRunLabel = ""
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}

	for name, mutate := range map[string]func(map[string]string){
		"volume missing run label": func(labels map[string]string) { delete(labels, "aries.run") },
		"volume wrong run label":   func(labels map[string]string) { labels["aries.run"] = "other-run" },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy-secret"))
			manager, request := newTestManager(t, fake)
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			labels := fake.volumeLabels["aries-openclaw-state-fixedid"]
			mutate(labels)
			if err := manager.Stop(context.Background()); err == nil {
				t.Fatal("volume run-label drift was ignored")
			}
			if fake.removedVolumes["aries-openclaw-state-fixedid"] {
				t.Fatal("destructive command targeted a volume after run-label drift")
			}
			labels["aries.run"] = "test-run"
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHarnessRejectsMissingInvalidAndLeakedKeys(t *testing.T) {
	for name, test := range map[string]struct {
		key     string
		present bool
	}{
		"missing":   {key: "discard-me", present: false},
		"empty":     {key: "", present: true},
		"newline":   {key: "secret\n", present: true},
		"nul":       {key: "secret\x00", present: true},
		"oversized": {key: strings.Repeat("x", maxAPIKeyBytes+1), present: true},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy"))
			manager, request := newTestManager(t, fake)
			returned := []byte(test.key)
			manager.apiKeyLookup = func(string) ([]byte, bool) { return returned, test.present }
			if err := manager.Start(context.Background(), request); err == nil {
				t.Fatal("invalid key was accepted")
			}
			if !bytes.Equal(returned, make([]byte, len(returned))) {
				t.Fatal("lookup secret buffer was not cleared")
			}
		})
	}
}

func TestAPIKeyLookupIsCopiedAndBothBuffersAreCleared(t *testing.T) {
	secret := []byte("callback-secret")
	fake := newFakeDocker(bytes.Clone(secret))
	manager, request := newTestManager(t, fake)
	returned := bytes.Clone(secret)
	manager.apiKeyLookup = func(name string) ([]byte, bool) {
		if name != request.Model.APIKeyEnv {
			t.Fatalf("lookup name = %q, want %q", name, request.Model.APIKeyEnv)
		}
		return returned, true
	}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(returned, make([]byte, len(returned))) {
		t.Fatal("callback buffer was not cleared after Start copied it")
	}
	active := manager.active
	activeKey := active.apiKey
	if !bytes.Equal(activeKey, secret) {
		t.Fatal("session did not retain an independent API-key copy")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(activeKey, make([]byte, len(activeKey))) {
		t.Fatal("session API-key buffer was not cleared after cleanup")
	}
}

func TestEnvironmentAPIKeyLookupReturnsFreshBytes(t *testing.T) {
	t.Setenv("ARIES_TEST_API_KEY", "environment-secret")
	first, ok := environmentAPIKeyLookup("ARIES_TEST_API_KEY")
	if !ok {
		t.Fatal("default API-key lookup missed an existing value")
	}
	second, ok := environmentAPIKeyLookup("ARIES_TEST_API_KEY")
	if !ok {
		t.Fatal("default API-key lookup missed a repeated value")
	}
	clear(first)
	if string(second) != "environment-secret" || os.Getenv("ARIES_TEST_API_KEY") != "environment-secret" {
		t.Fatal("default API-key lookup did not return fresh bytes")
	}
	clear(second)
}

func TestHarnessRejectsInvalidRunIDBeforeDocker(t *testing.T) {
	for _, runID := range []string{"", ".hidden", "run/escape", strings.Repeat("x", 129)} {
		t.Run(strconv.Quote(runID), func(t *testing.T) {
			fake := newFakeDocker([]byte("dummy-secret"))
			manager, request := newTestManager(t, fake)
			request.RunID = runID
			if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "run ID") {
				t.Fatalf("Start() error = %v, want invalid run ID", err)
			}
			if len(fake.commands) != 0 {
				t.Fatalf("invalid run ID reached Docker: %q", fake.commands)
			}
		})
	}
}

func TestBuildAgentCommandDisablesThinkingOnlyForExactDeepSeekModels(t *testing.T) {
	base := core.ModelConfig{BaseURL: "http://fake-model:8080/v1", Model: "deterministic-model"}
	tests := []struct {
		name     string
		model    core.ModelConfig
		thinking bool
	}{
		{name: "fake unchanged", model: base},
		{name: "flash", model: core.ModelConfig{BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"}, thinking: true},
		{name: "pro", model: core.ModelConfig{BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-pro"}, thinking: true},
		{name: "other model", model: core.ModelConfig{BaseURL: "https://api.deepseek.com", Model: "deepseek-chat"}},
		{name: "nonexact URL", model: core.ModelConfig{BaseURL: "https://api.deepseek.com/", Model: "deepseek-v4-flash"}},
	}
	baseCommand := []string{
		launcherPath, agentWrapperPath, stateContainerPath + "/.aries/run",
		"node", "openclaw.mjs", "agent", "--session-key", "agent:main:aries-fix-git",
		"--message", "repair", "--json", "--timeout", "60",
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			active := &session{safeTaskID: "fix-git", runResultDir: stateContainerPath + "/.aries/run", model: test.model}
			want := append([]string(nil), baseCommand...)
			if test.thinking {
				want = append(want, "--thinking", "off")
			}
			if got := buildAgentCommand(active, "repair", 60); !reflect.DeepEqual(got, want) {
				t.Fatalf("buildAgentCommand() = %q, want %q", got, want)
			}
		})
	}
}

func TestSafeTaskIDIsStableAndBounded(t *testing.T) {
	got := safeTaskID("Fix Git / Candidate !!! " + strings.Repeat("x", 100))
	if got != "fix-git---candidate-----xxxxxxxxxxxxxxxxxxxxxxxx" || len(got) > 48 {
		t.Fatalf("safe task ID = %q (%d)", got, len(got))
	}
}

func TestStageArchiveHasOnlyExpectedPrivateFiles(t *testing.T) {
	files := make(map[string]stagedFile)
	for _, name := range []string{"openclaw.json", "model.key", "gateway.key", "launch", "run-agent", "id_ed25519", "known_hosts", "ssh-config"} {
		files[name] = stagedFile{content: []byte(name), mode: 0o600}
	}
	content, err := stageArchive(files)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(content))
	count := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			count++
			if !strings.HasPrefix(header.Name, "aries-stage/") || header.Size != int64(len(filepath.Base(header.Name))) {
				t.Fatalf("archive header = %#v", header)
			}
		}
	}
	if count != 8 {
		t.Fatalf("staged file count = %d", count)
	}
}

func TestAgentTimeoutSecondsUsesWholeBoundedSeconds(t *testing.T) {
	if got := strconv.Itoa(max(1, int((1500*time.Millisecond)/time.Second))); got != "1" {
		t.Fatal(got)
	}
}

func TestClearSessionSecretsZerosBackingBytes(t *testing.T) {
	apiKey := []byte("model-secret")
	gatewayToken := []byte("gateway-secret")
	active := &session{apiKey: apiKey, gatewayToken: gatewayToken}
	clearSessionSecrets(active)
	if active.apiKey != nil || active.gatewayToken != nil {
		t.Fatal("secret slice references remain")
	}
	if !bytes.Equal(apiKey, make([]byte, len(apiKey))) || !bytes.Equal(gatewayToken, make([]byte, len(gatewayToken))) {
		t.Fatal("secret backing bytes were not zeroed")
	}
}
