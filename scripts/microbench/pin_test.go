// Package microbench checks the pin of the packed microbenchmark tasks.
package microbench

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
)

func readVersions(t *testing.T, path string) map[string]any {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var versions map[string]any
	if err := json.Unmarshal(content, &versions); err != nil {
		t.Fatal(err)
	}
	return versions
}

// The microbenchmark versions file differs from the main one only in the
// Terminal-Bench pin, so the other images and datasets cannot drift apart.
func TestMicrobenchVersionsDifferOnlyInTheTaskPin(t *testing.T) {
	main := readVersions(t, "../../configs/versions.json")
	micro := readVersions(t, "../../configs/microbench-versions.json")
	delete(main, "terminalbench2")
	delete(micro, "terminalbench2")
	if !reflect.DeepEqual(main, micro) {
		t.Fatal("configs/microbench-versions.json differs from configs/versions.json outside terminalbench2; copy the change across")
	}
}

// The pin names exactly the committed tasks, so a task edit cannot run under
// an old pin.
func TestMicrobenchPinMatchesTheCommittedTasks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := exec.Command("git", "-C", "../..", "rev-parse", "HEAD").Run(); err != nil {
		t.Skip("not a git checkout")
	}
	output, err := exec.Command("bash", "pack.sh", "--print-revision").CombinedOutput()
	if err != nil {
		t.Fatalf("pack.sh: %v: %s", err, output)
	}
	packed := strings.TrimSpace(string(output))
	pin := readVersions(t, "../../configs/microbench-versions.json")["terminalbench2"].(map[string]any)["revision"]
	if packed != pin {
		t.Fatalf("the committed tasks pack to %s but configs/microbench-versions.json pins %v; update the pin", packed, pin)
	}
}

// The packed tasks load through ARIES's own Terminal-Bench loader, so a task
// file the loader rejects fails here rather than on a cluster.
func TestPackedTasksLoad(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := exec.Command("git", "-C", "../..", "rev-parse", "HEAD").Run(); err != nil {
		t.Skip("not a git checkout")
	}
	checkout := filepath.Join(t.TempDir(), "microbench")
	if output, err := exec.Command("bash", "pack.sh", checkout).CombinedOutput(); err != nil {
		t.Fatalf("pack.sh: %v: %s", err, output)
	}
	ids := []string{"mb-noop", "mb-one-call", "mb-ten-calls", "mb-cpu", "mb-output"}
	pin := readVersions(t, "../../configs/microbench-versions.json")["terminalbench2"].(map[string]any)["revision"].(string)
	benchmark, err := terminalbench.New(terminalbench.Options{Root: checkout, TaskIDs: ids, OutputDir: t.TempDir(), Revision: pin})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != len(ids) {
		t.Fatalf("loaded %d tasks, want %d", len(tasks), len(ids))
	}
	for _, task := range tasks {
		if task.Environment.Image != "python:3.12.12-slim-bookworm" || task.Environment.AllowNetwork {
			t.Fatalf("task %s environment = %+v", task.ID, task.Environment)
		}
	}
}
