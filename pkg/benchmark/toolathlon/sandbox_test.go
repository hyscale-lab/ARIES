package toolathlon

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

// flowSandbox is a scripted sandbox that models just enough container state
// for the adapter's lifecycle: the task directory's direct children, the
// adapter's private files, and which services have been started. It records
// every command so tests can assert ordering.
type flowSandbox struct {
	t                *testing.T
	taskName         string
	needsApplication bool

	commands []core.Command
	events   []string
	uploads  map[string]string // container path -> host source
	files    map[string]bool   // private and grader files that exist
	taskDir  map[string]byte   // task directory children -> kind
	stash    []dirEntry        // what the last `tar -cf` archived

	forwarderStarted bool
	gatewayStarted   bool
	projectInstalled bool
	healthFailures   int
	preprocessExit   int
	evalResult       string
}

func newFlowSandbox(t *testing.T, taskName string, needsApplication bool) *flowSandbox {
	return &flowSandbox{
		t: t, taskName: taskName, needsApplication: needsApplication,
		uploads: map[string]string{}, files: map[string]bool{}, taskDir: map[string]byte{},
		evalResult: `{"pass": true, "details": "All evaluation checks passed"}`,
	}
}

func (s *flowSandbox) taskPath() string { return taskDirectoryPath(s.taskName) }

func (s *flowSandbox) exists(p string) bool {
	if s.files[p] {
		return true
	}
	if p == s.taskPath() {
		return len(s.taskDir) != 0
	}
	if rest, ok := strings.CutPrefix(p, s.taskPath()+"/"); ok {
		_, present := s.taskDir[rest]
		return present
	}
	return false
}

func (s *flowSandbox) remove(p string) {
	delete(s.files, p)
	if p == s.taskPath() {
		s.taskDir = map[string]byte{}
	}
	if rest, ok := strings.CutPrefix(p, s.taskPath()+"/"); ok {
		delete(s.taskDir, rest)
	}
	if p == privateRoot {
		for file := range s.files {
			if strings.HasPrefix(file, privateRoot+"/") {
				delete(s.files, file)
			}
		}
	}
}

func (s *flowSandbox) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	args := command.Args
	switch command.Path {
	case "/bin/rm":
		for _, p := range args[2:] {
			s.remove(p)
		}
		s.events = append(s.events, "rm")
		return core.CommandResult{}, nil
	case "/bin/mkdir":
		return core.CommandResult{}, nil
	case "/bin/sh":
		switch args[2] {
		case "aries-toolathlon-absence":
			for _, p := range args[3:] {
				if s.exists(p) {
					return core.CommandResult{ExitCode: 1}, nil
				}
			}
			return core.CommandResult{}, nil
		case "aries-toolathlon-portfwd":
			s.events = append(s.events, "forwarder")
			s.forwarderStarted = true
			s.files[forwarderReadyPath] = true
			return core.CommandResult{}, nil
		case "aries-toolathlon-portfwd-ready":
			if s.files[forwarderReadyPath] {
				return core.CommandResult{}, nil
			}
			return core.CommandResult{ExitCode: 1}, nil
		case "aries-toolathlon-gateway":
			if !s.files[bundleContainerPath] {
				s.t.Error("gateway started without the bundle in place")
			}
			if args[4] != "10086" {
				s.t.Errorf("gateway port argument = %q", args[4])
			}
			s.events = append(s.events, "gateway")
			s.gatewayStarted = true
			s.files[gatewayLogPath] = true
			return core.CommandResult{}, nil
		case "aries-toolathlon-gateway-health":
			if s.healthFailures > 0 {
				s.healthFailures--
				return core.CommandResult{ExitCode: 7}, nil
			}
			if !s.gatewayStarted {
				return core.CommandResult{ExitCode: 7}, nil
			}
			return core.CommandResult{}, nil
		case "aries-toolathlon-uv":
			return s.execUV(command, args[3:])
		}
	case tarPath:
		switch args[2] {
		case "-xf":
			archive := args[3]
			if !s.files[archive] {
				return core.CommandResult{ExitCode: 2, Stderr: "no such archive"}, nil
			}
			if archive == archiveContainerPath && args[1] == workspaceRoot {
				members, err := archiveMemberNames(s.uploads[archive])
				if err != nil {
					s.t.Fatal(err)
				}
				for _, member := range members {
					if rest, ok := strings.CutPrefix(member, "tasks/"+taskPool+"/"+s.taskName+"/"); ok && rest != "" {
						name, _, _ := strings.Cut(rest, "/")
						kind := byte('f')
						if strings.Contains(rest, "/") {
							kind = 'd'
						}
						s.taskDir[name] = kind
					}
				}
				s.projectInstalled = true
				s.events = append(s.events, "project")
				return core.CommandResult{}, nil
			}
			if archive == stashContainerPath && args[1] == s.taskPath() {
				for _, entry := range s.stash {
					s.taskDir[entry.name] = entry.kind
				}
				s.events = append(s.events, "restore")
				return core.CommandResult{}, nil
			}
		case "-cf":
			s.stash = nil
			for _, name := range args[5:] {
				kind, ok := s.taskDir[name]
				if !ok {
					return core.CommandResult{ExitCode: 2, Stderr: "tar: " + name + ": Cannot stat"}, nil
				}
				s.stash = append(s.stash, dirEntry{kind: kind, name: name})
			}
			s.files[args[3]] = true
			s.events = append(s.events, "stash")
			return core.CommandResult{}, nil
		}
	case findPath:
		names := make([]string, 0, len(s.taskDir))
		for name := range s.taskDir {
			names = append(names, name)
		}
		slices.Sort(names)
		var listing strings.Builder
		for _, name := range names {
			fmt.Fprintf(&listing, "%c\t%s\n", s.taskDir[name], name)
		}
		return core.CommandResult{Stdout: listing.String()}, nil
	}
	return core.CommandResult{}, fmt.Errorf("unscripted command %q %v", command.Path, command.Args)
}

// execUV scripts `uv run python -m <module> ...`, the two Toolathlon phases.
func (s *flowSandbox) execUV(command core.Command, args []string) (core.CommandResult, error) {
	module := args[3]
	if command.Dir != workspaceRoot {
		s.t.Errorf("%s ran in %q, want %q", module, command.Dir, workspaceRoot)
	}
	switch module {
	case "scripts.decoupled.container_preprocess":
		if !s.projectInstalled {
			s.t.Error("preprocess ran before the project was installed")
		}
		if s.needsApplication && !s.forwarderStarted {
			s.t.Error("preprocess ran before the loopback forwarder")
		}
		if command.Env["TOOLATHLON_OPENAI_BASE_URL"] == "" {
			s.t.Error("preprocess ran without the placeholder model URL")
		}
		s.events = append(s.events, "preprocess")
		if s.preprocessExit != 0 {
			return core.CommandResult{ExitCode: s.preprocessExit, Stdout: "boom"}, nil
		}
		s.files[bundleContainerPath] = true
		return core.CommandResult{Stdout: "Preprocess done."}, nil
	case "scripts.decoupled.container_eval":
		if _, ok := s.taskDir["evaluation"]; !ok {
			s.t.Error("evaluator ran without the grader restored")
		}
		if !s.files[trajectoryPath] || !s.files[bundleContainerPath] {
			s.t.Error("evaluator ran without the trajectory stub and bundle")
		}
		if !s.gatewayStarted || (s.needsApplication && !s.forwarderStarted) {
			s.t.Error("evaluator ran without the services still up")
		}
		s.events = append(s.events, "evaluate")
		delete(s.files, bundleContainerPath)
		s.files[evalResultPath] = true
		var parsed evalResult
		_ = json.Unmarshal([]byte(s.evalResult), &parsed)
		if parsed.Pass != nil && *parsed.Pass {
			return core.CommandResult{Stdout: "Pass: True"}, nil
		}
		return core.CommandResult{ExitCode: 1, Stdout: "Pass: False"}, nil
	}
	return core.CommandResult{}, fmt.Errorf("unscripted uv invocation %v", args)
}

func (s *flowSandbox) Upload(_ context.Context, source, destination string) error {
	if _, err := os.Stat(source); err != nil {
		return err
	}
	s.uploads[destination] = source
	s.files[destination] = true
	s.events = append(s.events, "upload:"+path.Base(destination))
	return nil
}

func (s *flowSandbox) Download(_ context.Context, source, destination string) error {
	if !s.files[source] {
		return errors.New("no such file: " + source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	switch source {
	case bundleContainerPath:
		bundle := fmt.Sprintf(`{"schema_version": 2, "task_dir": "%s/%s", "needed_mcp_servers": ["canvas", "memory"],
			"container_paths": {"task_root": %q, "agent_workspace": %q, "log_file": %q},
			"resolved_task_config": {"task_dir": "%s/%s"}}`, taskPool, s.taskName, taskRootPath, agentWorkspacePath, trajectoryPath, taskPool, s.taskName)
		return os.WriteFile(destination, []byte(bundle), 0o600)
	case stashContainerPath:
		file, err := os.Create(destination)
		if err != nil {
			return err
		}
		writer := tar.NewWriter(file)
		for _, entry := range s.stash {
			if entry.kind == 'd' {
				if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: entry.name + "/", Mode: 0o755}); err != nil {
					return err
				}
				continue
			}
			if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: entry.name, Mode: 0o644, Size: 0}); err != nil {
				return err
			}
		}
		if err := writer.Close(); err != nil {
			return err
		}
		return file.Close()
	case gatewayLogPath:
		return os.WriteFile(destination, []byte("[gateway] exposed tools: []\n"), 0o600)
	case evalResultPath:
		return os.WriteFile(destination, []byte(s.evalResult), 0o600)
	}
	return errors.New("unscripted download: " + source)
}

func preparedBenchmark(t *testing.T, sandbox *flowSandbox) (*Benchmark, core.Task) {
	t.Helper()
	root := writeFixture(t)
	options := baseOptions(t, root)
	options.TaskIDs = []string{sandbox.taskName}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return benchmark, tasks[0]
}

func TestPrepareSandboxRunsTheDecoupledStepsInOrder(t *testing.T) {
	sandbox := newFlowSandbox(t, "canvas-list-test", true)
	sandbox.healthFailures = 2
	gatewayReadyDelay, forwarderReadyDelay = 0, 0
	benchmark, task := preparedBenchmark(t, sandbox)

	if err := benchmark.PrepareSandbox(context.Background(), task, sandbox); err != nil {
		t.Fatal(err)
	}

	want := []string{"rm", "upload:project.tar", "project", "rm", "upload:portfwd.py", "forwarder", "preprocess", "stash", "rm", "gateway", "rm"}
	if !slices.Equal(sandbox.events, want) {
		t.Fatalf("events = %v\nwant     %v", sandbox.events, want)
	}
	details := benchmark.details[task.ID]
	if !slices.Equal(details.stashed, []string{"evaluation", "groundtruth_workspace", "preprocess", "README.md"}) {
		t.Fatalf("stashed = %v", details.stashed)
	}
	for _, name := range details.stashed {
		if _, present := sandbox.taskDir[name]; present {
			t.Fatalf("%q still present in the task directory after stash", name)
		}
	}
	for _, name := range []string{"docs", "initial_workspace", "task_config.json", "token_key_session.py"} {
		if _, present := sandbox.taskDir[name]; !present {
			t.Fatalf("%q was removed although it is not protected", name)
		}
	}
	if sandbox.files[bundleContainerPath] {
		t.Fatal("the container bundle survived preparation")
	}
	hostDir := filepath.Join(benchmark.outputDir, task.ID, "toolathlon")
	for _, name := range []string{"task_bundle.json", "artifact-stash.tar", "preprocess.log"} {
		if _, err := os.Stat(filepath.Join(hostDir, name)); err != nil {
			t.Fatalf("host artifact %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(hostDir, "project.tar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the project archive was left on the host")
	}
	if _, err := archiveMemberNames(sandbox.uploads[archiveContainerPath]); err == nil {
		t.Fatal("the uploaded project archive should have been deleted after upload")
	}
}

func TestPrepareSandboxSkipsTheForwarderForLocalOnlyTasks(t *testing.T) {
	sandbox := newFlowSandbox(t, "excel-only", false)
	gatewayReadyDelay, forwarderReadyDelay = 0, 0
	benchmark, task := preparedBenchmark(t, sandbox)
	if err := benchmark.PrepareSandbox(context.Background(), task, sandbox); err == nil || !strings.Contains(err.Error(), "needed_mcp_servers") {
		// The scripted bundle always names canvas and memory, so a local-only
		// task must be caught by the bundle check, after the forwarder was
		// correctly skipped.
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(sandbox.events, "forwarder") {
		t.Fatal("forwarder started for a task with no application server")
	}
}

func TestPrepareSandboxFailsClosedOnPreprocessFailure(t *testing.T) {
	sandbox := newFlowSandbox(t, "canvas-list-test", true)
	sandbox.preprocessExit = 3
	gatewayReadyDelay, forwarderReadyDelay = 0, 0
	benchmark, task := preparedBenchmark(t, sandbox)
	err := benchmark.PrepareSandbox(context.Background(), task, sandbox)
	if err == nil || !strings.Contains(err.Error(), "exit code 3") {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(sandbox.events, "gateway") || slices.Contains(sandbox.events, "stash") {
		t.Fatalf("events after a failed preprocess = %v", sandbox.events)
	}
	log, readErr := os.ReadFile(filepath.Join(benchmark.outputDir, task.ID, "toolathlon", "preprocess.log"))
	if readErr != nil || !strings.Contains(string(log), "boom") {
		t.Fatalf("preprocess log = %q, %v", log, readErr)
	}
}

func TestPrepareSandboxFailsWhenTheGatewayNeverAnswers(t *testing.T) {
	sandbox := newFlowSandbox(t, "canvas-list-test", true)
	sandbox.healthFailures = 1000
	gatewayReadyDelay, forwarderReadyDelay = 0, 0
	gatewayReadyAttempts = 3
	t.Cleanup(func() { gatewayReadyAttempts = 120 })
	benchmark, task := preparedBenchmark(t, sandbox)
	if err := benchmark.PrepareSandbox(context.Background(), task, sandbox); err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("err = %v", err)
	}
}

func TestEvaluateRestoresTheGraderAndScoresTheResult(t *testing.T) {
	for name, testCase := range map[string]struct {
		result    string
		wantScore float64
		wantErr   string
	}{
		"pass": {result: `{"pass": true, "details": "ok"}`, wantScore: 1},
		"fail": {result: `{"pass": false, "failure": "[FILE_MISSING] a.csv"}`, wantScore: 0},
		"null": {result: `{"pass": null, "details": "Task status: failed"}`, wantErr: "did not grade"},
	} {
		t.Run(name, func(t *testing.T) {
			sandbox := newFlowSandbox(t, "canvas-list-test", true)
			sandbox.evalResult = testCase.result
			gatewayReadyDelay, forwarderReadyDelay = 0, 0
			benchmark, task := preparedBenchmark(t, sandbox)
			if err := benchmark.PrepareSandbox(context.Background(), task, sandbox); err != nil {
				t.Fatal(err)
			}
			// The agent planted a fake grader and a fake verdict.
			sandbox.taskDir["evaluation"] = 'd'
			sandbox.taskDir["readme.MD"] = 'f'
			sandbox.files[evalResultPath] = true
			sandbox.files[trajectoryPath] = true
			sandbox.events = nil

			evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"rm", "upload:artifact-stash.tar", "restore", "rm", "rm", "upload:traj_log.json", "upload:task_bundle.json", "evaluate"}
			if !slices.Equal(sandbox.events, want) {
				t.Fatalf("events = %v\nwant     %v", sandbox.events, want)
			}
			if evaluation.Score != testCase.wantScore || evaluation.Reward != testCase.wantScore {
				t.Fatalf("evaluation = %#v", evaluation)
			}
			if testCase.wantScore == 1 && (evaluation.Status != core.StatusSucceeded || evaluation.VerifierStatus != core.StatusSucceeded) {
				t.Fatalf("evaluation = %#v", evaluation)
			}
			if testCase.wantScore == 0 && (evaluation.Status != core.StatusFailed || evaluation.VerifierStatus != core.StatusFailed) {
				t.Fatalf("evaluation = %#v", evaluation)
			}
			if _, present := sandbox.taskDir["readme.MD"]; present {
				t.Fatal("the agent's README spelling survived restore")
			}
			hostDir := filepath.Join(benchmark.outputDir, task.ID, "toolathlon")
			for _, name := range []string{"eval.log", "eval_res.json", "gateway.log"} {
				if !slices.Contains(evaluation.LogPaths, filepath.Join(hostDir, name)) {
					t.Fatalf("log paths = %v, missing %s", evaluation.LogPaths, name)
				}
			}
		})
	}
}

func TestValidateBundleRejectsLayoutDrift(t *testing.T) {
	details := taskDetails{name: "canvas-list-test", servers: []string{"canvas", "memory"}}
	good := fmt.Sprintf(`{"schema_version": 2, "task_dir": "finalpool/canvas-list-test", "needed_mcp_servers": ["memory", "canvas"],
		"container_paths": {"task_root": %q, "agent_workspace": %q, "log_file": %q}, "resolved_task_config": {"id": "x"}}`,
		taskRootPath, agentWorkspacePath, trajectoryPath)
	if err := validateBundle([]byte(good), details); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"schema":     strings.Replace(good, `"schema_version": 2`, `"schema_version": 1`, 1),
		"task dir":   strings.Replace(good, `finalpool/canvas-list-test`, `finalpool/other`, 1),
		"workspace":  strings.Replace(good, agentWorkspacePath, "/tmp/elsewhere", 1),
		"resolved":   strings.Replace(good, `"resolved_task_config": {"id": "x"}`, `"resolved_task_config": null`, 1),
		"servers":    strings.Replace(good, `["memory", "canvas"]`, `["memory"]`, 1),
		"not json":   "{",
		"bad server": strings.Replace(good, `["memory", "canvas"]`, `["memory", "../x"]`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateBundle([]byte(input), details); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
