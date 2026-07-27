package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingModelRuntime struct {
	events         []string
	done           chan struct{}
	startErr       error
	stopErr        error
	stopContextErr error
}

func (runtime *recordingModelRuntime) Start(context.Context) error {
	runtime.events = append(runtime.events, "start")
	return runtime.startErr
}

func (runtime *recordingModelRuntime) Stop(ctx context.Context) error {
	runtime.events = append(runtime.events, "stop")
	runtime.stopContextErr = ctx.Err()
	return runtime.stopErr
}

func (runtime *recordingModelRuntime) Done() <-chan struct{} { return runtime.done }

func TestSGLangProcessRuntimePreservesArgvAndStopsProcessGroup(t *testing.T) {
	root := t.TempDir()
	argvPath := filepath.Join(root, "argv")
	dependencyPath := filepath.Join(root, "runtime-dependency")
	if err := os.WriteFile(dependencyPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	dependencyResultPath := filepath.Join(root, "dependency")
	executable := writeRuntimeHelper(t, root, "runtime", `command -v runtime-dependency > `+dependencyResultPath+`
printf '%s\n' "$@" > `+argvPath+`
sleep 300 &
wait
`)
	configPath := filepath.Join(root, "config with spaces.yaml")
	if err := os.WriteFile(configPath, []byte("model-path: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := newSGLangProcessRuntime(sglangProcessOptions{
		Executable: executable,
		ConfigPath: configPath,
		OutputDir:  root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeFile(t, argvPath)
	content, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "-m\nsglang.launch_server\n--config\n"+configPath+"\n" {
		t.Fatalf("argv = %q", content)
	}
	dependency, err := os.ReadFile(dependencyResultPath)
	if err != nil || strings.TrimSpace(string(dependency)) != dependencyPath {
		t.Fatalf("runtime dependency = %q, %v", dependency, err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Stop(stopCtx); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
}

func TestInjectedModelRuntimeWrapsPreflightAndRunFailure(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	_, executablePath := createTestAriesRepository(t)
	runs := filepath.Join(t.TempDir(), "runs")
	configPath := writeCommandConfig(t, runs)
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), `"type":"terminalbench2"`, `"type":"unsupported"`, 1))
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &recordingModelRuntime{}
	doer := &preflightDoer{t: t, replies: []preflightReply{{
		status: 200,
		body:   `{"data":[{"id":"deepseek-v4-flash"}]}`,
	}}}
	err = runCommandWithDependencies(context.Background(), []string{configPath}, io.Discard, commandDependencies{
		executablePath:  executablePath,
		preflightClient: doer,
		modelRuntime:    runtime,
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported benchmark type "unsupported"`) {
		t.Fatalf("run error = %v", err)
	}
	if strings.Join(runtime.events, ",") != "start,stop" {
		t.Fatalf("runtime events = %v", runtime.events)
	}
}

func TestInjectedModelRuntimeStopFailureIsReturned(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	_, executablePath := createTestAriesRepository(t)
	configPath := writeCommandConfig(t, filepath.Join(t.TempDir(), "runs"))
	runtime := &recordingModelRuntime{stopErr: errors.New("stop canary")}
	doer := &preflightDoer{t: t, replies: []preflightReply{{status: 401}}}
	err := runCommandWithDependencies(context.Background(), []string{configPath}, io.Discard, commandDependencies{
		executablePath:  executablePath,
		preflightClient: doer,
		modelRuntime:    runtime,
	})
	if err == nil || !strings.Contains(err.Error(), "stop canary") {
		t.Fatalf("run error = %v", err)
	}
}

func TestRunCommandSelectsManagedSGLangRuntime(t *testing.T) {
	t.Setenv("SGLANG_API_KEY", "dummy")
	root := t.TempDir()
	runs := filepath.Join(root, "runs")
	configPath := writeCommandConfig(t, runs)
	helper := writeRuntimeHelper(t, root, "managed-sglang", "sleep 300 &\nwait\n")
	sglangPath := filepath.Join(filepath.Dir(configPath), "sglang.yaml")
	sglangConfig := `model-path: local
served-model-name: local
host: 0.0.0.0
port: 30000
device: cuda
tensor-parallel-size: 1
context-length: 32768
mem-fraction-static: 0.85
tool-call-parser: qwen
`
	if err := os.WriteFile(sglangPath, []byte(sglangConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := string(content)
	profile = strings.Replace(profile, `"versions_file":"versions.json",`, `"versions_file":"versions.json","sglang_file":"sglang.yaml","model_runtime":{"mode":"managed","executable":`+fmt.Sprintf("%q", helper)+`,"startup_timeout":"10s","stop_timeout":"3s"},`, 1)
	profile = strings.Replace(profile, `"type":"terminalbench2"`, `"type":"unsupported"`, 1)
	profile = strings.Replace(profile, `"provider":"deepseek","base_url":"https://api.deepseek.com","model":"deepseek-v4-flash","api_key_env":"DEEPSEEK_API_KEY"`, `"provider":"sglang","base_url":"http://fake.invalid:30000/v1","model":"local","api_key_env":"SGLANG_API_KEY"`, 1)
	if err := os.WriteFile(configPath, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	doer := &sglangPreflightDoer{t: t, wantURL: "http://fake.invalid:30000/v1/models", body: `{"data":[{"id":"local"}]}`}
	err = runCommandWithDependencies(context.Background(), []string{configPath}, io.Discard, commandDependencies{preflightClient: doer})
	if err == nil || !strings.Contains(err.Error(), `unsupported benchmark type "unsupported"`) {
		t.Fatalf("run error = %v", err)
	}
	entries, err := os.ReadDir(runs)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run entries = %v, %v", entries, err)
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		if _, err := os.Stat(filepath.Join(runs, entries[0].Name(), "sglang", name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestSGLangProcessRuntimeReportsUnexpectedExit(t *testing.T) {
	root := t.TempDir()
	executable := writeRuntimeHelper(t, root, "exit", "exit 7\n")
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("model-path: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := newSGLangProcessRuntime(sglangProcessOptions{
		Executable: executable,
		ConfigPath: configPath,
		OutputDir:  root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not exit")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.Stop(stopCtx); err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestSGLangProcessRuntimeForcesCleanupAfterCancellation(t *testing.T) {
	root := t.TempDir()
	readyPath := filepath.Join(root, "ready")
	executable := writeRuntimeHelper(t, root, "runtime", "touch "+readyPath+"\nsleep 300 &\nwait\n")
	runtime, err := newSGLangProcessRuntime(sglangProcessOptions{
		Executable: executable,
		ConfigPath: filepath.Join(root, "config.yaml"),
		OutputDir:  root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeFile(t, readyPath)
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.Stop(canceledCtx); err != nil {
		t.Fatal(err)
	}
}

func writeRuntimeHelper(t *testing.T, root, name, body string) string {
	t.Helper()
	path := filepath.Join(root, name)
	content := "#!/bin/sh\nset -eu\n" + body
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForRuntimeFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
