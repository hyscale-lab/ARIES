package sglang

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/internal/procstat"
)

func TestStartRecordsTheServersProcessGroup(t *testing.T) {
	output := t.TempDir()
	runtime, err := New(Options{Executable: helper(t, "sleep 60"), ConfigPath: "config", OutputDir: output, BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid, running, err := RecordedServer(output)
	if err != nil || !running || pid != runtime.command.Process.Pid {
		t.Fatalf("pid=%d running=%v err=%v; started %d", pid, running, err, runtime.command.Process.Pid)
	}
	assertMode(t, filepath.Join(output, "sglang", processRecordName), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), forcedProcessWait)
	defer cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, running, err := RecordedServer(output); err != nil || running {
		t.Fatalf("after Stop: running=%v err=%v", running, err)
	}
}

func TestStopRecordedServerEndsTheGroupARunLeft(t *testing.T) {
	output := t.TempDir()
	runtime, err := New(Options{Executable: helper(t, "sleep 60 & wait"), ConfigPath: "config", OutputDir: output, BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := runtime.command.Process.Pid
	// Only the record is used, as by a cleanup after ARIES was killed.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := StopRecordedServer(ctx, output, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the server's group remains: %v", err)
	}
	if _, running, err := RecordedServer(output); err != nil || running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	if err := StopRecordedServer(ctx, output, time.Second); err != nil {
		t.Fatalf("a repeated stop: %v", err)
	}
	select {
	case <-runtime.Done():
	case <-time.After(forcedProcessWait):
		t.Fatal("the runtime did not observe the exit")
	}
}

func TestRecordedServerIgnoresALaterProcessWithTheSamePID(t *testing.T) {
	output := t.TempDir()
	if err := os.Mkdir(filepath.Join(output, "sglang"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := exec.Command("sleep", "60")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	start, alive, err := procstat.StartTime(other.Process.Pid)
	if err != nil || !alive {
		t.Fatalf("alive=%v err=%v", alive, err)
	}
	content, _ := json.Marshal(processRecord{PID: other.Process.Pid, Start: start + 1})
	if err := os.WriteFile(filepath.Join(output, "sglang", processRecordName), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, running, err := RecordedServer(output); err != nil || running {
		t.Fatalf("a process that only reuses the PID looked like the server: running=%v err=%v", running, err)
	}
	if err := StopRecordedServer(context.Background(), output, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(other.Process.Pid, 0); err != nil {
		t.Fatalf("a process that only reuses the PID was signaled: %v", err)
	}
}

func TestARunWithoutARecordHasNoServer(t *testing.T) {
	output := t.TempDir()
	if pid, running, err := RecordedServer(output); err != nil || running || pid != 0 {
		t.Fatalf("pid=%d running=%v err=%v", pid, running, err)
	}
	if err := StopRecordedServer(context.Background(), output, time.Second); err != nil {
		t.Fatal(err)
	}
}
