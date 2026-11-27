package hermes

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
)

// runStateArtifactScript runs stateArtifactScript with the host's python3, so
// its exits, byte limit, and refusal to follow links are checked for real.
func runStateArtifactScript(t *testing.T, directory, name string, limit int) (string, int) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	command := exec.Command(python, "-c", stateArtifactScript, directory, name, strconv.Itoa(limit))
	var stdout strings.Builder
	command.Stdout = &stdout
	code := 0
	if err := command.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), code
}

func TestStateArtifactScriptReadsOnlyRegularFilesUnderTheLimit(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "events.jsonl")
	if err := os.WriteFile(file, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stdout, code := runStateArtifactScript(t, directory, "events.jsonl", 10); code != 0 || stdout != "0123456789" {
		t.Fatalf("whole file: exit %d, stdout %q", code, stdout)
	}
	if stdout, code := runStateArtifactScript(t, directory, "events.jsonl", 4); code != stateArtifactTruncated || stdout != "0123" {
		t.Fatalf("over limit: exit %d, stdout %q", code, stdout)
	}
	if stdout, code := runStateArtifactScript(t, directory, "absent", 10); code != stateArtifactMissing || stdout != "" {
		t.Fatalf("absent: exit %d, stdout %q", code, stdout)
	}
	// A link to a file outside the directory is refused, not followed.
	outside := filepath.Join(t.TempDir(), "model.key")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if stdout, code := runStateArtifactScript(t, directory, "link.jsonl", 10); code != stateArtifactNotRegular || stdout != "" {
		t.Fatalf("symlink: exit %d, stdout %q", code, stdout)
	}
	if err := os.Mkdir(filepath.Join(directory, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if stdout, code := runStateArtifactScript(t, directory, "sessions", 10); code != stateArtifactNotRegular || stdout != "" {
		t.Fatalf("directory: exit %d, stdout %q", code, stdout)
	}
	// A state directory replaced by a link is refused as well.
	swapped := filepath.Join(t.TempDir(), "hermes")
	if err := os.Symlink(directory, swapped); err != nil {
		t.Fatal(err)
	}
	if stdout, code := runStateArtifactScript(t, swapped, "events.jsonl", 10); code != stateArtifactNotRegular || stdout != "" {
		t.Fatalf("linked directory: exit %d, stdout %q", code, stdout)
	}
}

func newStateTestManager(t *testing.T, fake *fakeDeployment, artifacts ...string) *Manager {
	t.Helper()
	t.Cleanup(fake.closeGateway)
	manager, err := New(Options{
		Runtime: harnesscommon.RuntimeOptions{
			Deployment: fake, Image: testHermesImage, OutputDir: t.TempDir(),
			APIKeyLookup: func(string) ([]byte, bool) { return []byte("model-secret"), true },
		},
		Env:            map[string]string{"HERMES_FEATURE_MODE": "ours", "HERMES_FEATURE": "1"},
		StateArtifacts: artifacts,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.newID = func() (string, error) { return "attempt", nil }
	return manager
}

// Profile env is exported after the harness's own variables; a file under
// HERMES_HOME comes back redacted as a state artifact, and an absent one is
// left out without failing the run.
func TestRunExportsProfileEnvAndRetainsStateArtifacts(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.stateResults = map[string]core.CommandResult{
		"feature_events.jsonl": {Stdout: `{"kind":"accepted","token":"model-secret"}` + "\n"},
		"large.jsonl":          {Stdout: "first bytes", ExitCode: stateArtifactTruncated},
	}
	manager := newStateTestManager(t, fake, "feature_events.jsonl", "absent.jsonl", "large.jsonl")
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	env := fake.created.Env
	if len(env) < 2 || env[len(env)-2] != "HERMES_FEATURE=1" || env[len(env)-1] != "HERMES_FEATURE_MODE=ours" {
		t.Fatalf("profile env not exported last and sorted: %#v", env)
	}
	result, err := manager.Run(context.Background(), "task")
	if err != nil || result.Status != core.StatusSucceeded {
		t.Fatalf("err=%v result=%#v", err, result)
	}
	state := filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "state")
	content, err := os.ReadFile(filepath.Join(state, "feature_events.jsonl"))
	if err != nil {
		t.Fatalf("state artifact missing: %v", err)
	}
	if !bytes.Contains(content, []byte(`"kind":"accepted"`)) || bytes.Contains(content, []byte("model-secret")) {
		t.Fatalf("state artifact content = %q", content)
	}
	if content, err := os.ReadFile(filepath.Join(state, "large.jsonl")); err != nil || string(content) != "first bytes" {
		t.Fatalf("truncated state artifact = %q, %v", content, err)
	}
	if !slices.Contains(result.LogPaths, filepath.Join(state, "feature_events.jsonl")) || !slices.Contains(result.LogPaths, filepath.Join(state, "large.jsonl")) {
		t.Fatalf("state artifacts not listed in log paths: %#v", result.LogPaths)
	}
	if _, statErr := os.Stat(filepath.Join(state, "absent.jsonl")); statErr == nil || slices.Contains(result.LogPaths, filepath.Join(state, "absent.jsonl")) {
		t.Fatal("absent state artifact was retained")
	}
	// The directory, name and limit are separate arguments of a fixed script.
	index := slices.IndexFunc(fake.execs, func(c core.Command) bool { return len(c.Args) == 5 && c.Args[1] == stateArtifactScript })
	if index < 0 || fake.execs[index].Path != "python3" || !slices.Equal(fake.execs[index].Args[2:], []string{stateContainerPath, "feature_events.jsonl", strconv.Itoa(stateArtifactLimit)}) {
		t.Fatalf("state artifact exec = %#v", fake.execs)
	}
}

// A link or other non-regular file where a state artifact is expected is
// reported instead of read, and the other artifacts are still retained.
func TestNonRegularStateArtifactFailsTheRun(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.stateResults = map[string]core.CommandResult{
		"link.jsonl": {ExitCode: stateArtifactNotRegular},
		"kept.jsonl": {Stdout: "kept"},
	}
	manager := newStateTestManager(t, fake, "link.jsonl", "kept.jsonl")
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "link.jsonl is not a regular file") || result.Status != core.StatusFailed {
		t.Fatalf("err=%v status=%q", err, result.Status)
	}
	state := filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "state")
	if _, statErr := os.Stat(filepath.Join(state, "link.jsonl")); statErr == nil {
		t.Fatal("non-regular state artifact was retained")
	}
	if !slices.Contains(result.LogPaths, filepath.Join(state, "kept.jsonl")) {
		t.Fatalf("later state artifact not retained: %#v", result.LogPaths)
	}
}

func TestNewRejectsStateArtifactPaths(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../model.key", "sub/events.jsonl", "/etc/passwd"} {
		_, err := New(Options{
			Runtime:        harnesscommon.RuntimeOptions{Deployment: newFakeDeployment(t), Image: testHermesImage, OutputDir: t.TempDir()},
			StateArtifacts: []string{name},
		})
		if err == nil {
			t.Fatalf("state artifact %q was accepted", name)
		}
	}
}
