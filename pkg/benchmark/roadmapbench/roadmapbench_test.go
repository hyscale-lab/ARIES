package roadmapbench

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fixtureTaskID = "opt-3.0.0-roadmap"

const fixtureTaskTOML = `version = "1"

[metadata]
name = "Optimize a roadmap feature"
description = "A task from the public RoadmapBench format"
category = "software-engineering"
difficulty = "hard"

[verifier]
timeout_sec = 1800

[agent]
timeout_sec = 7200

[environment]
docker_image = "znpt/roadmapbench-opt-3.0.0-roadmap"
build_timeout_sec = 600
cpus = 2
memory_mb = 4096
storage_mb = 10240
`

func TestLoadTaskMapsUpstreamDefaultsAndKeepsVerifierPrivate(t *testing.T) {
	root := writeFixture(t)
	task, private, err := loadTask(root, fixtureTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != fixtureTaskID || task.Instruction != "Implement the requested roadmap feature." || task.Timeout != 2*time.Hour {
		t.Fatalf("task = %#v", task)
	}
	environment := task.Environment
	if environment.Image != "znpt/roadmapbench-opt-3.0.0-roadmap:latest" || environment.Workdir != "/app" || environment.CPU != 2 || environment.MemoryMB != 4096 || environment.StorageMB != 10240 || !environment.AllowNetwork {
		t.Fatalf("environment = %#v", environment)
	}
	if private.timeout != 30*time.Minute || private.workdir != "/app" || len(private.verifierFiles) != 3 {
		t.Fatalf("private details = %#v", private)
	}
	for _, file := range private.verifierFiles {
		if strings.Contains(file.source, "solution") || file.destination != "/tests/"+file.name {
			t.Fatalf("verifier file = %#v", file)
		}
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private verifier", "private solution", "test.sh", "hidden.patch", "test_outputs.py", root} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("core.Task exposed %q: %s", secret, encoded)
		}
	}
}

func TestTasksPreserveSelectionAndExecutionIdentity(t *testing.T) {
	root := writeFixture(t)
	secondID := "another.roadmap"
	writeTaskFixture(t, root, secondID, fixtureTaskTOML)
	commitFixture(t, root)
	options := testOptions(root, []string{secondID, fixtureTaskID}, t.TempDir())
	options.ExecutionTaskIDs = []string{secondID + "-004", fixtureTaskID + "-005"}
	options.VerifierTimeoutFloor = time.Hour
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	options.TaskIDs[0] = "mutated"
	options.ExecutionTaskIDs[0] = "mutated"
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ID != secondID+"-004" || tasks[1].ID != fixtureTaskID+"-005" {
		t.Fatalf("tasks = %#v", tasks)
	}
	for _, task := range tasks {
		private, ok := benchmark.details[task.ID]
		if !ok || private.timeout != time.Hour || task.Timeout != 2*time.Hour {
			t.Fatalf("execution details = %#v; task=%#v", private, task)
		}
	}
	if _, ok := benchmark.details[fixtureTaskID]; ok {
		t.Fatal("logical task ID retained as a private execution key")
	}
}

func TestVerifierTimeoutFloorNeverLowersTaskBudget(t *testing.T) {
	root := writeFixture(t)
	options := testOptions(root, []string{fixtureTaskID}, t.TempDir())
	options.VerifierTimeoutFloor = time.Minute
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if benchmark.details[fixtureTaskID].timeout != 30*time.Minute {
		t.Fatalf("verifier timeout = %v", benchmark.details[fixtureTaskID].timeout)
	}
}

func TestLoadTaskPreservesExplicitNetworkAndTaggedImages(t *testing.T) {
	root := writeFixture(t)
	config := strings.Replace(fixtureTaskTOML, "[environment]", "[environment]\nallow_internet = false", 1)
	config = strings.Replace(config, "znpt/roadmapbench-opt-3.0.0-roadmap", "docker.io/example/roadmap:Version_1", 1)
	config += "\n[environment.env]\nTask_mode = \"agent\"\n[verifier.env]\nCHECK_MODE = \"private verifier\"\n"
	writeFile(t, filepath.Join(root, fixtureTaskID, "task.toml"), config)
	task, private, err := loadTask(root, fixtureTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Environment.AllowNetwork || task.Environment.Image != "docker.io/example/roadmap:Version_1" || !reflect.DeepEqual(task.Environment.Env, map[string]string{"Task_mode": "agent"}) || !reflect.DeepEqual(private.verifierEnv, map[string]string{"CHECK_MODE": "private verifier"}) {
		t.Fatalf("task=%#v private=%#v", task, private)
	}
}

func TestLoadTaskAllowsDescriptiveMetadataAndRejectsExecutionChanges(t *testing.T) {
	tests := []struct {
		name, old, replacement, want string
	}{
		{"metadata", "[metadata]", "[metadata]\nfuture_notes = [\"descriptive\"]", ""},
		{"version", `version = "1"`, `version = "2"`, "version"},
		{"unknown environment", "[environment]", "[environment]\nprivileged = true", "unsupported field"},
		{"unknown verifier", "[verifier]", "[verifier]\ncommand = \"echo 1\"", "unsupported field"},
		{"unknown agent", "[agent]", "[agent]\nnetwork = true", "unsupported field"},
		{"unknown root", `version = "1"`, "version = \"1\"\nmounts = [\"/\"]", "unsupported field"},
		{"agent timeout", "timeout_sec = 7200", "timeout_sec = 0", "agent.timeout_sec"},
		{"verifier timeout", "timeout_sec = 1800", "timeout_sec = inf", "verifier.timeout_sec"},
		{"build timeout", "build_timeout_sec = 600", "build_timeout_sec = -1", "build_timeout_sec"},
		{"cpu", "cpus = 2", "cpus = nan", "environment.cpus"},
		{"memory", "memory_mb = 4096", "memory_mb = 0", "resources"},
		{"storage", "storage_mb = 10240", "storage_mb = -1", "resources"},
		{"gpus", "[environment]", "[environment]\ngpus = -1", "resources"},
		{"mcp", "[environment]", "[environment]\nmcp_servers = [\"server\"]", "mcp_servers"},
		{"invalid environment", "[environment]", "[environment]\nenv = {\"BAD-NAME\" = \"value\"}", "environment.env"},
		{"invalid verifier environment", "[verifier]", "[verifier]\nenv = {\"9BAD\" = \"value\"}", "verifier.env"},
		{"empty image", "znpt/roadmapbench-opt-3.0.0-roadmap", "", "docker_image"},
		{"malformed image", "znpt/roadmapbench-opt-3.0.0-roadmap", "not an image", "docker_image"},
		{"digest image", "znpt/roadmapbench-opt-3.0.0-roadmap", "example/task@sha256:" + strings.Repeat("a", 64), "docker_image"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeTaskFixture(t, root, fixtureTaskID, strings.Replace(fixtureTaskTOML, test.old, test.replacement, 1))
			_, _, err := loadTask(root, fixtureTaskID)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadTask error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadTaskRejectsMissingMalformedAndSymlinkInputs(t *testing.T) {
	for _, relative := range []string{"task.toml", "instruction.md", "tests/test.sh", "tests/nested/hidden.patch", "environment/Dockerfile", "tests", "environment", "."} {
		t.Run(relative, func(t *testing.T) {
			root := writeFixture(t)
			file := filepath.Join(root, fixtureTaskID, relative)
			target := filepath.Join(t.TempDir(), "target")
			if err := os.Rename(file, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, file); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadTask(root, fixtureTaskID); err == nil {
				t.Fatalf("accepted symlink at %q", relative)
			}
		})
	}
	for name, mutate := range map[string]func(string){
		"missing script": func(root string) {
			if err := os.Remove(filepath.Join(root, fixtureTaskID, "tests/test.sh")); err != nil {
				t.Fatal(err)
			}
		},
		"empty instruction": func(root string) { writeFile(t, filepath.Join(root, fixtureTaskID, "instruction.md"), "  \n") },
		"malformed TOML":    func(root string) { writeFile(t, filepath.Join(root, fixtureTaskID, "task.toml"), "[broken") },
	} {
		t.Run(name, func(t *testing.T) {
			root := writeFixture(t)
			mutate(root)
			if _, _, err := loadTask(root, fixtureTaskID); err == nil {
				t.Fatal("accepted invalid task")
			}
		})
	}
}

func TestNewRejectsUnsafeSelectionAndExecutionIDs(t *testing.T) {
	base := Options{Root: "root", TaskIDs: []string{fixtureTaskID}, OutputDir: "out", Revision: strings.Repeat("a", 40)}
	for name, mutate := range map[string]func(*Options){
		"root":                func(o *Options) { o.Root = "" },
		"output":              func(o *Options) { o.OutputDir = "" },
		"revision":            func(o *Options) { o.Revision = "" },
		"empty tasks":         func(o *Options) { o.TaskIDs = nil },
		"traversal":           func(o *Options) { o.TaskIDs = []string{"../outside"} },
		"control":             func(o *Options) { o.TaskIDs = []string{"task\nname"} },
		"duplicate":           func(o *Options) { o.TaskIDs = []string{fixtureTaskID, fixtureTaskID} },
		"execution count":     func(o *Options) { o.ExecutionTaskIDs = []string{} },
		"execution traversal": func(o *Options) { o.ExecutionTaskIDs = []string{fixtureTaskID + "/001"} },
		"execution mismatch":  func(o *Options) { o.ExecutionTaskIDs = []string{"other-001"} },
		"execution zero":      func(o *Options) { o.ExecutionTaskIDs = []string{fixtureTaskID + "-000"} },
		"negative floor":      func(o *Options) { o.VerifierTimeoutFloor = -time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			options := base
			mutate(&options)
			if _, err := New(options); err == nil {
				t.Fatal("accepted invalid options")
			}
		})
	}
}

func TestTasksRejectDirtyCheckoutAndCancellation(t *testing.T) {
	root := writeFixture(t)
	benchmark, err := New(testOptions(root, []string{fixtureTaskID}, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := benchmark.Tasks(canceled); err == nil {
		t.Fatal("accepted canceled context")
	}
	writeFile(t, filepath.Join(root, fixtureTaskID, "instruction.md"), "changed")
	if _, err := benchmark.Tasks(context.Background()); err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("dirty Tasks error = %v", err)
	}
}

func TestCheckedDurationSecondsRejectsOverflow(t *testing.T) {
	threshold := math.Exp2(63) / float64(time.Second)
	for _, seconds := range []float64{threshold, math.Inf(1), math.NaN(), 0, -1, 1e-10} {
		if _, err := checkedDurationSeconds(seconds); err == nil {
			t.Fatalf("accepted %g", seconds)
		}
	}
	if duration, err := checkedDurationSeconds(math.Nextafter(threshold, 0)); err != nil || duration <= 0 {
		t.Fatalf("below-threshold duration = %v, %v", duration, err)
	}
}

func TestFinalWorkdir(t *testing.T) {
	for name, test := range map[string]struct{ dockerfile, want string }{
		"absolute":     {"FROM base\nWORKDIR /app\n", "/app"},
		"relative":     {"FROM base\nWORKDIR app\nWORKDIR nested\n", "/app/nested"},
		"case":         {"FrOm base\n workdir /workspace \n", "/workspace"},
		"multistage":   {"FROM base AS build\nWORKDIR /build\nFROM --platform=linux/amd64 final\nWORKDIR /app\n", "/app"},
		"final stage":  {"FROM base\nWORKDIR /build\nFROM final\n", "/"},
		"continuation": {"FROM base\nRUN printf x \\\n  WORKDIR /ignored\nWORKDIR /app\n", "/app"},
		"variable":     {"FROM base\nWORKDIR ${ROOT}/app\n", "/"},
		"shell":        {"FROM base\nWORKDIR /app;id\n", "/"},
		"ambiguous":    {"FROM base\nWORKDIR /app child\n", "/"},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "Dockerfile")
			writeFile(t, file, test.dockerfile)
			if got, err := finalWorkdir(file); err != nil || got != test.want {
				t.Fatalf("workdir = %q, %v", got, err)
			}
		})
	}
	if got, err := finalWorkdir(filepath.Join(t.TempDir(), "missing")); err != nil || got != "/" {
		t.Fatalf("missing workdir = %q, %v", got, err)
	}
}

func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTaskFixture(t, root, fixtureTaskID, fixtureTaskTOML)
	commitFixture(t, root)
	return root
}

func writeTaskFixture(t *testing.T, root, id, config string) {
	t.Helper()
	taskDir := filepath.Join(root, id)
	writeFile(t, filepath.Join(taskDir, "task.toml"), config)
	writeFile(t, filepath.Join(taskDir, "instruction.md"), "Implement the requested roadmap feature.\n")
	writeFile(t, filepath.Join(taskDir, "environment", "Dockerfile"), "FROM example/base:fixture\nWORKDIR /app\n")
	writeFile(t, filepath.Join(taskDir, "tests", "test.sh"), "#!/bin/bash\n# private verifier\n")
	writeFile(t, filepath.Join(taskDir, "tests", "test_outputs.py"), "# private verifier\n")
	writeFile(t, filepath.Join(taskDir, "tests", "nested", "hidden.patch"), "private verifier\n")
	writeFile(t, filepath.Join(taskDir, "solution", "solve.sh"), "private solution\n")
	writeFile(t, filepath.Join(taskDir, "environment", "repo", "large-tree"), "vendored repository\n")
}

func commitFixture(t *testing.T, root string) {
	t.Helper()
	fixtureGit(t, root, "init", "--quiet")
	fixtureGit(t, root, "add", ".")
	fixtureGit(t, root, "-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "commit", "--quiet", "-m", "fixture")
}

func fixtureGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func fixtureGitRevision(root string) string {
	output, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testOptions(root string, ids []string, output string) Options {
	return Options{Root: root, TaskIDs: ids, OutputDir: output, Revision: fixtureGitRevision(root)}
}
