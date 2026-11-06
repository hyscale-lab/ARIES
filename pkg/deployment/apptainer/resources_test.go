package apptainer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

// fakeProc is a /proc tree with the files ResourceSource reads.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemTotal:       32000000 kB\nMemFree: 1 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fakeProc{t: t, root: root}
}

// process adds a process; times are utime, stime, cutime and cstime in ticks.
func (p *fakeProc) process(pid, parent int, state, namespace, cmdline string, times [4]uint64, start uint64, pssKiB int) {
	p.t.Helper()
	dir := filepath.Join(p.root, fmt.Sprint(pid))
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o700); err != nil {
		p.t.Fatal(err)
	}
	// Fields 3 to 22; the command name holds a space and a parenthesis.
	fields := []string{state, fmt.Sprint(parent), "0", "0", "0", "0", "0", "0", "0", "0", "0",
		fmt.Sprint(times[0]), fmt.Sprint(times[1]), fmt.Sprint(times[2]), fmt.Sprint(times[3]), "20", "0", "1", "0", fmt.Sprint(start)}
	stat := fmt.Sprintf("%d (a b) c) %s 0 0\n", pid, strings.Join(fields, " "))
	files := map[string]string{"stat": stat, "cmdline": cmdline + "\x00"}
	if pssKiB >= 0 {
		files["smaps_rollup"] = fmt.Sprintf("00400000-7ffc [rollup]\nRss: %d kB\nPss: %d kB\n", pssKiB*2, pssKiB)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			p.t.Fatal(err)
		}
	}
	if namespace != "" {
		if err := os.Symlink(namespace, filepath.Join(dir, "ns", "pid")); err != nil {
			p.t.Fatal(err)
		}
	}
}

func writeRuntimeRecord(t *testing.T, stateDir string, current record, exited string) {
	t.Helper()
	dir := filepath.Join(stateDir, current.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, recordFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if exited != "" {
		if err := os.WriteFile(filepath.Join(dir, exitedCPUFile), []byte(exited), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func runtimeLabels(task, component string) map[string]string {
	return map[string]string{"aries.managed": "true", "aries.run": "run-1", "aries.task": task, "aries.component": component, "aries.kind": "task-sandbox"}
}

func TestResourceSourceCountsTheInstanceItsCommandsAndWhatHasExited(t *testing.T) {
	proc := newFakeProc(t)
	host, inside, other := "pid:[4026531836]", "pid:[4026532374]", "pid:[4026532999]"
	proc.process(100, 1, "S", host, "Apptainer instance: root [aries-task-1]", [4]uint64{2, 1, 0, 0}, 500, 1000)
	proc.process(101, 100, "S", inside, "appinit", [4]uint64{3, 1, 40, 5}, 501, 2000) // has reaped orphans
	proc.process(102, 100, "S", host, "/usr/sbin/fuse2fs", [4]uint64{7, 3, 0, 0}, 502, 3000)
	proc.process(200, 50, "S", host, "starter", [4]uint64{90, 90, 90, 90}, 600, 9000) // an exec's launcher
	proc.process(201, 200, "S", inside, "sh", [4]uint64{1, 0, 20, 0}, 601, 400)       // the wrapper, inside
	proc.process(202, 201, "R", inside, "python3", [4]uint64{50, 10, 0, 0}, 602, 5000)
	proc.process(300, 1, "S", other, "appinit", [4]uint64{999, 999, 0, 0}, 700, 99000) // another instance
	stateDir := t.TempDir()
	writeRuntimeRecord(t, stateDir, record{Name: "aries-task-1", Labels: runtimeLabels("occ-1", "sandbox"), InstancePID: 101}, "10\n5\n")
	writeRuntimeRecord(t, stateDir, record{Name: "aries-other", Labels: runtimeLabels("occ-2", "sandbox"), InstancePID: 300}, "")

	source, err := NewResourceSource(ResourceOptions{StateDir: stateDir, RunID: "run-1", TaskIDs: []string{"occ-1"}, ProcRoot: proc.root})
	if err != nil {
		t.Fatal(err)
	}
	readings, err := source.Sample(context.Background())
	if err != nil || len(readings) != 1 {
		t.Fatalf("Sample = %+v, %v", readings, err)
	}
	got := readings[0]
	// Starter 3, appinit 49, fuse2fs 10, wrapper 21, python 60, and 15 from finished commands.
	wantTicks := uint64(3 + 49 + 10 + 21 + 60 + 15)
	if got.CPUUsageNanoseconds != wantTicks*1e7 {
		t.Fatalf("CPU = %d ns, want %d ticks", got.CPUUsageNanoseconds, wantTicks)
	}
	if want := uint64(1000+2000+3000+400+5000) * 1024; got.MemoryUsageBytes != want {
		t.Fatalf("memory = %d, want %d", got.MemoryUsageBytes, want)
	}
	if got.MemoryLimitBytes != 32000000*1024 || got.TaskID != "occ-1" || got.Component != "sandbox" || got.RuntimeID != "aries-task-1" ||
		got.Scope != "task" || got.RunID != "run-1" || got.ObservedAt.IsZero() {
		t.Fatalf("reading = %+v", got)
	}
}

func TestResourceSourceKeepsTheCounterMonotonic(t *testing.T) {
	proc := newFakeProc(t)
	proc.process(101, 1, "S", "pid:[1]", "appinit", [4]uint64{0, 0, 0, 0}, 501, 10)
	proc.process(201, 1, "S", "pid:[1]", "sh", [4]uint64{0, 0, 30, 0}, 601, 10)
	stateDir := t.TempDir()
	writeRuntimeRecord(t, stateDir, record{Name: "aries-task-1", Labels: runtimeLabels("occ-1", "harness"), InstancePID: 101}, "")
	source, err := NewResourceSource(ResourceOptions{StateDir: stateDir, RunID: "run-1", TaskIDs: []string{"occ-1"}, ProcRoot: proc.root})
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.Sample(context.Background())
	if err != nil || len(first) != 1 || first[0].CPUUsageNanoseconds != 30*1e7 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	// The command ended, and its wrapper's report is not recorded yet.
	if err := os.RemoveAll(filepath.Join(proc.root, "201")); err != nil {
		t.Fatal(err)
	}
	second, err := source.Sample(context.Background())
	if err != nil || len(second) != 1 || second[0].CPUUsageNanoseconds != 30*1e7 {
		t.Fatalf("second = %+v, %v", second, err)
	}
}

func TestResourceSourceSelectsOnlyItsRunningInstances(t *testing.T) {
	proc := newFakeProc(t)
	proc.process(101, 1, "S", "pid:[1]", "appinit", [4]uint64{1, 0, 0, 0}, 1, 10)
	proc.process(102, 1, "Z", "pid:[2]", "appinit", [4]uint64{1, 0, 0, 0}, 1, 10)
	stateDir := t.TempDir()
	other := runtimeLabels("occ-1", "sandbox")
	other["aries.run"] = "run-2"
	unmanaged := runtimeLabels("occ-1", "sandbox")
	delete(unmanaged, "aries.managed")
	for _, current := range []record{
		{Name: "other-run", Labels: other, InstancePID: 101},
		{Name: "unmanaged", Labels: unmanaged, InstancePID: 101},
		{Name: "other-task", Labels: runtimeLabels("occ-2", "sandbox"), InstancePID: 101},
		{Name: "a-bridge", Labels: runtimeLabels("occ-1", "bridge"), InstancePID: 101},
		{Name: "stopped", Labels: runtimeLabels("occ-1", "sandbox"), InstancePID: 102},
		{Name: "gone", Labels: runtimeLabels("occ-1", "sandbox"), InstancePID: 103},
		{Name: "not-started", Labels: runtimeLabels("occ-1", "sandbox")},
		{Name: "evaluated", Labels: runtimeLabels("occ-1", "evaluation-sandbox"), InstancePID: 101},
	} {
		writeRuntimeRecord(t, stateDir, current, "")
	}
	source, err := NewResourceSource(ResourceOptions{StateDir: stateDir, RunID: "run-1", TaskIDs: []string{"occ-1"}, ProcRoot: proc.root})
	if err != nil {
		t.Fatal(err)
	}
	readings, err := source.Sample(context.Background())
	if err != nil || len(readings) != 1 || readings[0].RuntimeID != "evaluated" || readings[0].Component != "evaluation-sandbox" {
		t.Fatalf("Sample = %+v, %v", readings, err)
	}
}

func TestRunResourceSourceMeasuresTheBridgeAlone(t *testing.T) {
	proc := newFakeProc(t)
	proc.process(400, 1, "S", "pid:[0]", "aries-bridge", [4]uint64{30, 10, 500, 500}, 900, 7000) // reaped launchers excluded
	proc.process(401, 1, "S", "pid:[0]", "aries-bridge", [4]uint64{1, 1, 0, 0}, 950, 10)
	stateDir := t.TempDir()
	labels := map[string]string{"aries.managed": "true", "aries.run": "run-1", "aries.component": "bridge", "aries.kind": "tool-bridge"}
	for name, file := range map[string]processFile{
		"bridge-run-1":    {Name: "bridge-run-1", Labels: labels, PID: 400, Start: 900},
		"reused-pid":      {Name: "reused-pid", Labels: labels, PID: 401, Start: 949},
		"another-process": {Name: "another-process", Labels: map[string]string{"aries.managed": "true", "aries.run": "run-1"}, PID: 400, Start: 900},
	} {
		raw, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, name+processRecordSuffix), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := NewResourceSource(ResourceOptions{StateDir: stateDir, Scope: "run", RunID: "run-1", ProcRoot: proc.root})
	if err != nil {
		t.Fatal(err)
	}
	readings, err := source.Sample(context.Background())
	if err != nil || len(readings) != 1 {
		t.Fatalf("Sample = %+v, %v", readings, err)
	}
	want := core.ResourceReading{Scope: "run", RunID: "run-1", Component: "bridge", RuntimeID: "bridge-run-1", RuntimeName: "bridge-run-1",
		ObservedAt: readings[0].ObservedAt, CPUUsageNanoseconds: 40 * 1e7, MemoryUsageBytes: 7000 * 1024, MemoryLimitBytes: 32000000 * 1024}
	if readings[0] != want {
		t.Fatalf("reading = %+v, want %+v", readings[0], want)
	}
}

func TestAProcessThatExitsWhileReadIsSkipped(t *testing.T) {
	for _, err := range []error{
		&os.PathError{Op: "open", Path: "/proc/1/smaps_rollup", Err: syscall.ESRCH},
		&os.PathError{Op: "open", Path: "/proc/1/smaps_rollup", Err: syscall.ENOENT},
	} {
		if !processGone(err) {
			t.Fatalf("%v: not treated as an exited process", err)
		}
	}
	if processGone(&os.PathError{Op: "open", Path: "/proc/1/smaps_rollup", Err: syscall.EACCES}) || processGone(nil) {
		t.Fatal("a refused or successful read was treated as an exited process")
	}
	proc := newFakeProc(t)
	if pss, err := proportionalSet(proc.root, 4242); err != nil || pss != 0 {
		t.Fatalf("absent process = %d, %v", pss, err)
	}
}

func TestNewResourceSourceValidatesItsScope(t *testing.T) {
	for name, options := range map[string]ResourceOptions{
		"no run":             {StateDir: "/state", TaskIDs: []string{"a"}},
		"relative state":     {StateDir: "state", RunID: "r", TaskIDs: []string{"a"}},
		"task without tasks": {StateDir: "/state", RunID: "r"},
		"run with tasks":     {StateDir: "/state", RunID: "r", Scope: "run", TaskIDs: []string{"a"}},
		"repeated task":      {StateDir: "/state", RunID: "r", TaskIDs: []string{"a", "a"}},
		"unknown scope":      {StateDir: "/state", RunID: "r", Scope: "host"},
	} {
		if _, err := NewResourceSource(options); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestExitTrailerCarriesTheCommandsCPU(t *testing.T) {
	for raw, want := range map[string][2]uint64{
		"err\x1eARIES_EXEC_EXIT_tok=3:45\x1f":  {3, 45},
		"err\x1eARIES_EXEC_EXIT_tok=0\x1f":     {0, 0},
		"\x1eARIES_EXEC_EXIT_tok=255:0\x1fend": {255, 0},
	} {
		var stderr strings.Builder
		trailer := newExitTrailer(&stderr, "tok")
		if _, err := trailer.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		code, ticks, err := trailer.Finish()
		if err != nil || uint64(code) != want[0] || ticks != want[1] {
			t.Fatalf("%q: %d, %d, %v", raw, code, ticks, err)
		}
	}
	trailer := newExitTrailer(&strings.Builder{}, "tok")
	_, _ = trailer.Write([]byte("\x1eARIES_EXEC_EXIT_tok=0:x\x1f"))
	if _, _, err := trailer.Finish(); err == nil {
		t.Fatal("accepted an invalid CPU time")
	}
}

func TestRecordExitedCPUAppendsToTheRuntime(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "aries-task-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{options: Options{StateDir: stateDir}}
	manager.recordExitedCPU("aries-task-1", 45)
	manager.recordExitedCPU("aries-task-1", 0)
	manager.recordExitedCPU("aries-task-1", 7)
	manager.recordExitedCPU("removed-meanwhile", 9)
	ticks, err := exitedTicks(filepath.Join(stateDir, "aries-task-1", exitedCPUFile))
	if err != nil || ticks != 52 {
		t.Fatalf("ticks = %d, %v", ticks, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "removed-meanwhile")); !os.IsNotExist(err) {
		t.Fatalf("a removed runtime's directory was recreated: %v", err)
	}
}

func TestACPURecordThatFailsFailsTheRuntimesSamples(t *testing.T) {
	proc := newFakeProc(t)
	proc.process(101, 1, "S", "pid:[1]", "appinit", [4]uint64{0, 0, 0, 0}, 501, 10)
	stateDir := t.TempDir()
	writeRuntimeRecord(t, stateDir, record{Name: "aries-task-1", Labels: runtimeLabels("occ-1", "sandbox"), InstancePID: 101}, "")
	// A directory where the record goes makes the append fail.
	exited := filepath.Join(stateDir, "aries-task-1", exitedCPUFile)
	if err := os.RemoveAll(exited); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(exited, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{options: Options{StateDir: stateDir}}
	manager.recordExitedCPU("aries-task-1", 12)
	source, err := NewResourceSource(ResourceOptions{StateDir: stateDir, RunID: "run-1", TaskIDs: []string{"occ-1"}, ProcRoot: proc.root})
	if err != nil {
		t.Fatal(err)
	}
	if readings, err := source.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("Sample = %+v, %v", readings, err)
	}
}

func TestAnInstanceInTheHostsPIDNamespaceIsNotMeasured(t *testing.T) {
	proc := newFakeProc(t)
	host := "pid:[4026531836]"
	proc.process(101, 1, "S", host, "appinit", [4]uint64{1, 0, 0, 0}, 501, 10)
	proc.process(500, 1, "S", host, "another user's job", [4]uint64{900, 0, 0, 0}, 900, 99000)
	if err := os.MkdirAll(filepath.Join(proc.root, "self", "ns"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(proc.root, "self", "ns", "pid")); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	writeRuntimeRecord(t, stateDir, record{Name: "aries-task-1", Labels: runtimeLabels("occ-1", "sandbox"), InstancePID: 101}, "")
	source, err := NewResourceSource(ResourceOptions{StateDir: stateDir, RunID: "run-1", TaskIDs: []string{"occ-1"}, ProcRoot: proc.root})
	if err != nil {
		t.Fatal(err)
	}
	if readings, err := source.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "PID namespace") {
		t.Fatalf("Sample = %+v, %v", readings, err)
	}
}
