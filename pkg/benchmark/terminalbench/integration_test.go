//go:build integration

package terminalbench

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/hyscale-lab/aries/pkg/config"
)

func TestPinnedFixGitCheckout(t *testing.T) {
	versions, err := config.LoadVersions(filepath.Clean(filepath.Join("..", "..", "..", "configs", "versions.json")))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join("..", "..", "..", DefaultRoot))
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			t.Skip("pinned Terminal-Bench checkout is absent; run make setup")
		}
		t.Fatal(err)
	}
	if err := VerifyRevision(context.Background(), root, versions.TerminalBench2.Revision); err != nil {
		t.Fatal(err)
	}
	if err := Setup(context.Background(), root, versions.TerminalBench2.RepositoryURL, versions.TerminalBench2.Revision); err != nil {
		t.Fatalf("idempotent Setup() error = %v", err)
	}
	benchmark, err := New(Options{
		Root: root, TaskIDs: []string{fixGitID}, OutputDir: t.TempDir(),
		Revision: versions.TerminalBench2.Revision, Images: versions.TerminalBench2.Images,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != fixGitID || tasks[0].Environment.Image != versions.TerminalBench2.Images["alexgshaw/fix-git:20251031"] {
		t.Fatalf("Tasks() = %#v", tasks)
	}
}

func TestPinnedSelectedTasksLoadInRequestedOrder(t *testing.T) {
	root, versions := requirePinnedDataset(t)
	selected := []string{
		"fix-git",
		"prove-plus-comm",
		"overfull-hbox",
		"rstan-to-pystan",
		"schemelike-metacircular-eval",
	}
	type expectation struct {
		workdir       string
		cpu           float64
		memoryMB      int
		agentTimeout  string
		verifyTimeout string
		verifierFiles int
	}
	want := map[string]expectation{
		"fix-git":                      {workdir: "/app/personal-site", cpu: 1, memoryMB: 2048, agentTimeout: "15m0s", verifyTimeout: "15m0s", verifierFiles: 2},
		"prove-plus-comm":              {workdir: "/workspace", cpu: 1, memoryMB: 2048, agentTimeout: "15m0s", verifyTimeout: "15m0s", verifierFiles: 2},
		"overfull-hbox":                {workdir: "/app", cpu: 2, memoryMB: 4096, agentTimeout: "12m30s", verifyTimeout: "6m0s", verifierFiles: 5},
		"rstan-to-pystan":              {workdir: "/app", cpu: 4, memoryMB: 8192, agentTimeout: "30m0s", verifyTimeout: "30m0s", verifierFiles: 2},
		"schemelike-metacircular-eval": {workdir: "/app", cpu: 1, memoryMB: 2048, agentTimeout: "40m0s", verifyTimeout: "40m0s", verifierFiles: 67},
	}

	benchmark, err := New(Options{
		Root: root, TaskIDs: selected, OutputDir: t.TempDir(),
		Revision: versions.TerminalBench2.Revision, Images: versions.TerminalBench2.Images,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	loaded := make([]string, 0, len(tasks))
	for _, task := range tasks {
		id := task.ID
		loaded = append(loaded, id)
		details := benchmark.details[id]
		_, pin := pinnedTaskImage(t, root, versions.TerminalBench2.Images, id)
		expected := want[id]
		if task.Environment.Image != pin || task.Environment.Workdir != expected.workdir || task.Environment.CPU != expected.cpu || task.Environment.MemoryMB != expected.memoryMB || task.Environment.StorageMB != 10240 || task.Environment.GPUs != 0 || !task.Environment.AllowNetwork {
			t.Fatalf("task %q environment = %#v, expected %+v with image %q", id, task.Environment, expected, pin)
		}
		if task.Timeout.String() != expected.agentTimeout || details.timeout.String() != expected.verifyTimeout || len(details.verifierFiles) != expected.verifierFiles {
			t.Fatalf("task %q private details timeout=%s verifier_files=%d, expected %+v", id, details.timeout, len(details.verifierFiles), expected)
		}
	}
	if !reflect.DeepEqual(loaded, selected) {
		t.Fatalf("loaded task order = %v, want %v", loaded, selected)
	}
}

func TestEveryTaskInPinnedDatasetLoadsAtGenericBoundary(t *testing.T) {
	root, versions := requirePinnedDataset(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var taskIDs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, entry.Name(), "task.toml")); err == nil {
			taskIDs = append(taskIDs, entry.Name())
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if len(taskIDs) == 0 {
		t.Fatal("pinned Terminal-Bench checkout contains no task directories")
	}
	if len(taskIDs) != len(versions.TerminalBench2.Images) {
		t.Fatalf("pinned dataset has %d tasks but image catalog has %d entries", len(taskIDs), len(versions.TerminalBench2.Images))
	}

	for _, id := range taskIDs {
		_, pin := pinnedTaskImage(t, root, versions.TerminalBench2.Images, id)
		task, details, err := loadTask(root, id, versions.TerminalBench2.Images)
		if err != nil {
			t.Fatalf("load pinned task %q at generic boundary: %v", id, err)
		}
		if task.ID != id || strings.TrimSpace(task.Instruction) == "" {
			t.Fatalf("task %q generic identity/instruction = %#v", id, task)
		}
		if task.Environment.Image != pin || !filepath.IsAbs(task.Environment.Workdir) || task.Environment.CPU <= 0 || task.Environment.MemoryMB <= 0 || task.Environment.StorageMB <= 0 || task.Environment.GPUs < 0 {
			t.Fatalf("task %q invalid generic environment = %#v", id, task.Environment)
		}
		wantVerifierFiles := countRegularVerifierFiles(t, filepath.Join(root, id, "tests"))
		if len(details.verifierFiles) != wantVerifierFiles {
			t.Fatalf("task %q captured %d verifier files, want recursive tree of %d", id, len(details.verifierFiles), wantVerifierFiles)
		}
		if details.workdir != task.Environment.Workdir || details.timeout <= 0 {
			t.Fatalf("task %q private details = %#v", id, details)
		}
	}
}

func requirePinnedDataset(t *testing.T) (string, config.Versions) {
	t.Helper()
	versions, err := config.LoadVersions(filepath.Clean(filepath.Join("..", "..", "..", "configs", "versions.json")))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join("..", "..", "..", DefaultRoot))
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			t.Skip("pinned Terminal-Bench checkout is absent; run make setup")
		}
		t.Fatal(err)
	}
	if err := VerifyRevision(context.Background(), root, versions.TerminalBench2.Revision); err != nil {
		t.Fatal(err)
	}
	return root, versions
}

func pinnedTaskImage(t *testing.T, root string, images map[string]string, id string) (string, string) {
	t.Helper()
	var parsed taskFile
	if _, err := toml.DecodeFile(filepath.Join(root, id, "task.toml"), &parsed); err != nil {
		t.Fatal(err)
	}
	source := parsed.Environment.DockerImage
	if strings.TrimSpace(source) == "" {
		t.Fatalf("task %q has no environment.docker_image", id)
	}
	pin, ok := images[source]
	if !ok {
		t.Fatalf("task %q source image %q has no pin", id, source)
	}
	return source, pin
}

func countRegularVerifierFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}
