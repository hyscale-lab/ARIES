//go:build apptainer

package apptainer

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// ownTicks makes a shell report its own CPU ticks, user and system, from /proc.
const ownTicks = `read -r line </proc/$$/stat; set -- ${line##*) }; echo $((${12} + ${13}))`

func sampleRuntime(t *testing.T, source *ResourceSource, id string) core.ResourceReading {
	t.Helper()
	readings, err := source.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	for _, reading := range readings {
		if reading.RuntimeID == id {
			return reading
		}
	}
	t.Fatalf("no reading for %s in %+v", id, readings)
	return core.ResourceReading{}
}

// TestRealResourcesCountCommandsShorterThanASample runs commands that each end
// well within one sampling interval, and an orphan, and samples only before and
// after: their CPU can only come from the wrappers' reports and the init's
// reaped time. Each command also reports its own CPU, which is the truth the
// measurement must cover.
func TestRealResourcesCountCommandsShorterThanASample(t *testing.T) {
	manager := newRealManager(t)
	ctx := context.Background()
	request := realRequest(t)
	id, err := manager.Create(ctx, request)
	t.Cleanup(func() {
		if err := manager.Stop(context.Background(), id); err != nil {
			t.Errorf("cleanup Stop: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	source, err := NewResourceSource(ResourceOptions{StateDir: manager.options.StateDir, RunID: "run-1", TaskIDs: []string{"task-1"}})
	if err != nil {
		t.Fatal(err)
	}
	before := sampleRuntime(t, source, id)

	burn := func(iterations int) string {
		return "i=0; while [ $i -lt " + strconv.Itoa(iterations) + " ]; do i=$((i+1)); done; " + ownTicks
	}
	execs := 0
	run := func(script string) string {
		t.Helper()
		execs++
		result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", script}})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("Exec %q = %+v, %v", script, result, err)
		}
		return strings.TrimSpace(result.Stdout)
	}
	parse := func(text string) uint64 {
		t.Helper()
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			t.Fatalf("reported ticks %q: %v", text, err)
		}
		return value
	}
	const commands = 40
	var truth uint64
	for range commands {
		truth += parse(run(burn(100000)))
	}
	// The orphan outlives the command that started it; its init reaps it. It
	// is a shell of its own, so its $$ is its own process.
	run("sh -c '" + burn(400000) + "' >/tmp/orphan.ticks 2>/dev/null </dev/null & exit 0")
	deadline := time.Now().Add(time.Minute)
	for {
		if out := run("cat /tmp/orphan.ticks 2>/dev/null || true"); out != "" {
			truth += parse(out)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the orphan did not finish")
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Second) // the orphan's shell exits after writing
	after := sampleRuntime(t, source, id)
	measured := (after.CPUUsageNanoseconds - before.CPUUsageNanoseconds) / (1e9 / clockTicksPerSecond)
	t.Logf("commands' own CPU %d ticks; measured %d ticks over %d commands", truth, measured, execs)
	// Each self-report truncates to whole ticks; the measurement adds the
	// wrappers inside the instance, at most a launcher's cost per command.
	if measured+commands < truth {
		t.Fatalf("measured %d ticks, less than the commands' own %d", measured, truth)
	}
	if measured > truth+uint64(execs)*6 {
		t.Fatalf("measured %d ticks, more than the commands' %d and their wrappers", measured, truth)
	}

	// Memory held by a command counts while it runs. The shell has work left
	// after sleep, so it does not replace itself with sleep and drop the value.
	run("(x=$(head -c 40000000 /dev/zero | tr '\\0' a); sleep 4; echo ${#x}) >/dev/null 2>&1 </dev/null & exit 0")
	time.Sleep(2 * time.Second)
	holding := sampleRuntime(t, source, id)
	t.Logf("memory %d MiB before, %d MiB while a command holds 40 MB", before.MemoryUsageBytes>>20, holding.MemoryUsageBytes>>20)
	if holding.MemoryUsageBytes < before.MemoryUsageBytes+30_000_000 {
		t.Fatalf("memory %d while 40 MB is held, %d before", holding.MemoryUsageBytes, before.MemoryUsageBytes)
	}
	if holding.CPUUsageNanoseconds < after.CPUUsageNanoseconds {
		t.Fatal("the CPU counter went down")
	}
}
