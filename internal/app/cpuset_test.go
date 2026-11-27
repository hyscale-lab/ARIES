package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// stepCPUs stands in for a scheduler allocation of CPUs 28-55 on a larger host.
func stepCPUs() ([]int, error) {
	cpus := make([]int, 0, 28)
	for cpu := 28; cpu <= 55; cpu++ {
		cpus = append(cpus, cpu)
	}
	return cpus, nil
}

func TestResolveCPUSetInheritsOrStaysWithinProcessCPUs(t *testing.T) {
	for _, test := range []struct {
		value, want string
		count       int
	}{
		{value: "", want: "", count: 0},
		{value: "inherit", want: "28-55", count: 28},
		{value: "28-31,40", want: "28-31,40", count: 5},
		{value: "31,30,28-29,29", want: "28-31", count: 4},
	} {
		got, cpus, err := resolveCPUSet(test.value, stepCPUs)
		if err != nil || got != test.want || len(cpus) != test.count {
			t.Fatalf("resolveCPUSet(%q) = %q, %v, %v; want %q with %d CPUs", test.value, got, cpus, err, test.want, test.count)
		}
	}
}

// A list copied from another allocation, or naming CPUs the host lacks, is
// rejected before any container is created.
func TestResolveCPUSetRejectsCPUsOutsideTheProcess(t *testing.T) {
	for value, want := range map[string]string{
		"0-27":  "names CPU 0, outside the CPUs aries may run on (28-55)",
		"50-60": "names CPU 56",
		"0-127": "names CPU 0",
		"7-0":   "is descending",
	} {
		if _, _, err := resolveCPUSet(value, stepCPUs); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("resolveCPUSet(%q) error = %v, want %q", value, err, want)
		}
	}
	failing := func() ([]int, error) { return nil, errors.New("no affinity") }
	if _, _, err := resolveCPUSet("inherit", failing); err == nil || !strings.Contains(err.Error(), "no affinity") {
		t.Fatalf("affinity failure error = %v", err)
	}
	called := false
	if _, _, err := resolveCPUSet("", func() ([]int, error) { called = true; return nil, nil }); err != nil || called {
		t.Fatalf("empty cpuset consulted the host: called=%v err=%v", called, err)
	}
}

func TestProcessCPUsReadsThisProcessAffinity(t *testing.T) {
	cpus, err := processCPUs()
	if err != nil {
		t.Fatal(err)
	}
	if len(cpus) == 0 || !slices.IsSorted(cpus) || !slices.Equal(slices.Compact(slices.Clone(cpus)), cpus) {
		t.Fatalf("process CPUs = %v", cpus)
	}
}

// The kernel's mask is as long as the host's CPU count, which can exceed the
// 1024 CPUs of unix.CPUSet; every set bit is a CPU, however high.
func TestMaskCPUsReadsBeyondTheFixedCPUSet(t *testing.T) {
	mask := make([]uint, 2048/bits.UintSize)
	for _, cpu := range []int{0, 3, 1023, 1024, 1500, 2047} {
		mask[cpu/bits.UintSize] |= 1 << (cpu % bits.UintSize)
	}
	if got, want := maskCPUs(mask), []int{0, 3, 1023, 1024, 1500, 2047}; !slices.Equal(got, want) {
		t.Fatalf("maskCPUs = %v, want %v", got, want)
	}
}

// The grown mask reads the same affinity the fixed-size call does.
func TestProcessCPUsMatchesSchedGetaffinity(t *testing.T) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		t.Skipf("sched_getaffinity with a 1024-CPU mask: %v", err)
	}
	cpus, err := processCPUs()
	if err != nil {
		t.Fatal(err)
	}
	if len(cpus) != set.Count() {
		t.Fatalf("process CPUs = %v, want %d", cpus, set.Count())
	}
	for _, cpu := range cpus {
		if !set.IsSet(cpu) {
			t.Fatalf("process CPUs = %v, CPU %d not in the affinity", cpus, cpu)
		}
	}
}

// The sandbox and harness of a task share the cpuset, so the task's sandbox
// quota plus the harness quota must fit in it; the overrides replace the task's own
// sandbox quota, as the Runner applies them.
func TestCheckCPUSetFitRejectsTasksLargerThanTheCPUSet(t *testing.T) {
	tasks := []core.Task{{ID: "small", Environment: core.Environment{CPU: 1}}, {ID: "large", Environment: core.Environment{CPU: 4}}, {ID: "undeclared"}}
	two, three, five := 2.0, 3.0, 5.0
	for _, test := range []struct {
		name      string
		overrides config.RuntimeOverrides
		count     int
		want      string
	}{
		{name: "fits", count: 4},
		{name: "task too large", count: 2, want: `task "large" needs 4 CPUs (sandbox 4, harness 0), more than the 2`},
		{name: "harness quota counts", count: 5, overrides: config.RuntimeOverrides{HarnessResources: config.ResourceOverrides{CPU: &two}}, want: `task "large" needs 6 CPUs (sandbox 4, harness 2)`},
		{name: "sandbox override replaces the task quota", count: 3, overrides: config.RuntimeOverrides{AgentSandboxResources: config.ResourceOverrides{CPU: &three}}},
		{name: "sandbox override too large", count: 4, overrides: config.RuntimeOverrides{AgentSandboxResources: config.ResourceOverrides{CPU: &five}}, want: `task "small" needs 5 CPUs`},
	} {
		err := checkCPUSetFit(tasks, test.overrides, "0-63", test.count)
		if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
			t.Fatalf("%s: error = %v, want %q", test.name, err, test.want)
		}
	}
	if err := checkCPUSetFit(tasks, config.RuntimeOverrides{}, "", 0); err != nil {
		t.Fatalf("unpinned run rejected: %v", err)
	}
}

func TestFormatCPUSetJoinsRuns(t *testing.T) {
	for want, cpus := range map[string][]int{"": nil, "3": {3}, "0-3": {0, 1, 2, 3}, "0-1,3,5-7": {0, 1, 3, 5, 6, 7}} {
		if got := formatCPUSet(cpus); got != want {
			t.Fatalf("formatCPUSet(%v) = %q, want %q", cpus, got, want)
		}
	}
}

// writeCPUSetProfile writes the command test profile with execution.cpuset_cpus.
func writeCPUSetProfile(t *testing.T, runs, cpuset string) string {
	t.Helper()
	profile := writeCommandProfile(t, runs)
	content, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	content = bytes.Replace(content, []byte(`"benchmark":`), []byte(`"execution":{"concurrency":1,"cpuset_cpus":"`+cpuset+`"},"benchmark":`), 1)
	if err := os.WriteFile(profile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return profile
}

// "inherit" reaches every occurrence as this process's concrete CPU list,
// and a task too large for it stops the run before any occurrence or output.
func TestRunResolvesTheCPUSetBeforeAnyOccurrence(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	allowed, err := processCPUs()
	if err != nil {
		t.Fatal(err)
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	for _, test := range []struct {
		name    string
		taskCPU float64
		want    string
	}{
		{name: "fits", taskCPU: 1, want: "occurrence canary"},
		{name: "too large", taskCPU: float64(len(allowed) + 1), want: `task "a" needs`},
	} {
		runs := filepath.Join(t.TempDir(), "runs")
		profile := writeCPUSetProfile(t, runs, "inherit")
		var occurrenceCPUSet []string
		wiring := failingRunWiring(nil, new([]string), errors.New("occurrence canary"))
		wiring.LoadBenchmark = testBenchmarkLoader(func(context.Context, config.Config, []string, func(string) ([]byte, bool)) ([]core.Task, error) {
			return []core.Task{{ID: "a", Environment: core.Environment{Image: "task:1", CPU: test.taskCPU}}}, nil
		}, func(cfg config.Config, _, _, _ string, _ func(string) ([]byte, bool)) (runner.Benchmark, error) {
			occurrenceCPUSet = append(occurrenceCPUSet, cfg.Execution.CpusetCpus)
			return nil, errors.New("occurrence canary")
		})
		doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
		err := runCommandForTest(context.Background(), profile, io.Discard, Dependencies{Logger: logger, PreflightClient: doer, Wiring: wiring})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: run error = %v, want %q", test.name, err, test.want)
		}
		if test.name == "fits" && !slices.Equal(occurrenceCPUSet, []string{formatCPUSet(allowed)}) {
			t.Fatalf("occurrence cpusets = %q, want [%q]", occurrenceCPUSet, formatCPUSet(allowed))
		}
		if test.name == "too large" {
			if occurrenceCPUSet != nil {
				t.Fatalf("oversized task reached an occurrence: %q", occurrenceCPUSet)
			}
			if _, err := os.Stat(runs); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("output created: %v", err)
			}
		}
	}
}

func TestRunRejectsACPUSetOutsideTheProcessBeforePreparation(t *testing.T) {
	prepared := false
	wiring := failingRunWiring(nil, new([]string), nil)
	wiring.PrepareBackend = func(cfg config.Config, _ string) (PreparedBackend, error) {
		prepared = true
		return PreparedBackend{Model: cfg.CoreModel()}, nil
	}
	profile := writeCPUSetProfile(t, filepath.Join(t.TempDir(), "runs"), "65535")
	err := Run(context.Background(), profile, io.Discard, Dependencies{Wiring: wiring})
	if err == nil || !strings.Contains(err.Error(), "outside the CPUs aries may run on") || prepared {
		t.Fatalf("err = %v, prepared = %t", err, prepared)
	}
}
