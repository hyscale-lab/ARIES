package apptainer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

// A later ARIES process: it shares the state directory and nothing in memory.
func laterProcessRuntime(t *testing.T, earlier *ProcessRuntime, fake *fakeProgram) *ProcessRuntime {
	t.Helper()
	later, err := NewProcessRuntime(earlier.stateDir, earlier.executables)
	if err != nil {
		t.Fatal(err)
	}
	later.start, later.processStart, later.signalGroup = fake.start, fake.processStart, fake.signal
	return later
}

func assertRuntimeFilesGone(t *testing.T, workdir string) {
	t.Helper()
	for _, path := range []string{workdir, workdir + ".stdout.log", workdir + ".stderr.log", processRecordPath(workdir)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s remains: %v", path, err)
		}
	}
}

func TestProcessRuntimeStopsWhatAnEarlierProcessStarted(t *testing.T) {
	earlier, fake, request := newTestProcessRuntime(t)
	ctx := context.Background()
	id, err := earlier.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := earlier.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	later := laterProcessRuntime(t, earlier, fake)
	if running, err := later.Running(ctx, id); err != nil || !running {
		t.Fatalf("Running in a later process = %v, %v", running, err)
	}
	if err := later.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.signals, []syscall.Signal{syscall.SIGTERM}) || fake.alive[7001] {
		t.Fatalf("signals = %v, alive = %v", fake.signals, fake.alive)
	}
	assertRuntimeFilesGone(t, request.Workdir)
	if err := later.Stop(ctx, id); err != nil {
		t.Fatalf("repeated Stop = %v", err)
	}
	if err := laterProcessRuntime(t, earlier, fake).Stop(ctx, id); err != nil {
		t.Fatalf("Stop with no record left = %v", err)
	}
}

// Start has not run, so there is no process, but the workdir must still go.
func TestProcessRuntimeRemovesACreatedRuntimeAnEarlierProcessLeft(t *testing.T) {
	earlier, fake, request := newTestProcessRuntime(t)
	ctx := context.Background()
	id, err := earlier.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	later := laterProcessRuntime(t, earlier, fake)
	if running, err := later.Running(ctx, id); err != nil || running {
		t.Fatalf("Running = %v, %v", running, err)
	}
	if err := later.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(fake.signals) != 0 {
		t.Fatalf("signals = %v", fake.signals)
	}
	assertRuntimeFilesGone(t, request.Workdir)
}

// The PID now names another process: it must not be signaled, and the record's
// files are still removed.
func TestProcessRuntimeLeavesAPIDItNoLongerOwns(t *testing.T) {
	earlier, fake, request := newTestProcessRuntime(t)
	ctx := context.Background()
	id, err := earlier.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := earlier.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(processRecordPath(request.Workdir))
	if err != nil {
		t.Fatal(err)
	}
	var file processFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	file.Start++
	raw, err = json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(processRecordPath(request.Workdir), raw); err != nil {
		t.Fatal(err)
	}
	later := laterProcessRuntime(t, earlier, fake)
	if running, err := later.Running(ctx, id); err != nil || running {
		t.Fatalf("Running = %v, %v", running, err)
	}
	if err := later.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(fake.signals) != 0 || !fake.alive[7001] {
		t.Fatalf("signals = %v, alive = %v", fake.signals, fake.alive)
	}
	assertRuntimeFilesGone(t, request.Workdir)
}

func TestProcessRuntimeStopReportsARecordItCannotTrust(t *testing.T) {
	for name, content := range map[string]func(request deployment.Request) string{
		"unreadable": func(deployment.Request) string { return "{" },
		"another workdir": func(request deployment.Request) string {
			return `{"name":"` + request.Name + `","workdir":"/home/someone","pid":1,"start":1}`
		},
		"no workdir": func(request deployment.Request) string { return `{"name":"` + request.Name + `","pid":1,"start":1}` },
	} {
		t.Run(name, func(t *testing.T) {
			earlier, fake, request := newTestProcessRuntime(t)
			ctx := context.Background()
			id, err := earlier.Create(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if err := writePrivate(processRecordPath(request.Workdir), []byte(content(request))); err != nil {
				t.Fatal(err)
			}
			later := laterProcessRuntime(t, earlier, fake)
			if err := later.Stop(ctx, id); err == nil || !strings.Contains(err.Error(), "process runtime record") {
				t.Fatalf("Stop = %v", err)
			}
			if len(fake.signals) != 0 {
				t.Fatalf("signals = %v", fake.signals)
			}
			if _, err := os.Stat(request.Workdir); err != nil {
				t.Fatalf("workdir removed on a failed lookup: %v", err)
			}
		})
	}
}

func TestProcessRuntimeRefusesAWorkdirOutsideItsStateDirectory(t *testing.T) {
	runtime, _, request := newTestProcessRuntime(t)
	request.Workdir = filepath.Join(t.TempDir(), "bridge-run-1")
	if _, err := runtime.Create(context.Background(), request); err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("Create = %v", err)
	}
	if _, err := NewProcessRuntime("relative/state", nil); err == nil {
		t.Fatal("a relative state directory was accepted")
	}
}

// The same with a real process and real signals.
func TestProcessRuntimeStopEndsARealProcessFromItsRecord(t *testing.T) {
	program, err := sleepProgram()
	if err != nil {
		t.Skip(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	const entrypoint = "/usr/local/bin/aries-bridge"
	earlier, err := NewProcessRuntime(state, map[string]string{entrypoint: program})
	if err != nil {
		t.Fatal(err)
	}
	request := deployment.Request{Name: "aries-bridge-real", Workdir: filepath.Join(state, "bridge-run-1"), Entrypoint: []string{entrypoint}, Args: []string{"300"}, ServicePort: 8443}
	ctx := context.Background()
	id, err := earlier.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := earlier.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(processRecordPath(request.Workdir))
	if err != nil {
		t.Fatal(err)
	}
	var file processFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-file.PID, syscall.SIGKILL) })

	later, err := NewProcessRuntime(state, map[string]string{entrypoint: program})
	if err != nil {
		t.Fatal(err)
	}
	if running, err := later.Running(ctx, id); err != nil || !running {
		t.Fatalf("Running in a later process = %v, %v", running, err)
	}
	if err := later.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(file.PID, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d remains after Stop", file.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertRuntimeFilesGone(t, request.Workdir)
}

func sleepProgram() (string, error) {
	for _, path := range []string{"/bin/sleep", "/usr/bin/sleep"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", errors.New("no sleep program")
}
