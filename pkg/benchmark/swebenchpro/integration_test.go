//go:build integration

package swebenchpro

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/parquet-go/parquet-go"
	"github.com/sirupsen/logrus"
)

const dockerE2EInstanceID = "instance_qutebrowser__qutebrowser-f91ace96223cac8161c16dd061907e138fe85111-v059c6fdc75567943479b23ebca7c07b5e9a7f34c"

func TestPinnedSourcesSetupIsIdempotent(t *testing.T) {
	root, versions := requirePinnedSources(t)
	if err := Setup(
		context.Background(),
		root,
		versions.SWEbenchPro.DatasetRepositoryURL,
		versions.SWEbenchPro.DatasetRevision,
		versions.SWEbenchPro.EvaluatorRepositoryURL,
		versions.SWEbenchPro.EvaluatorRevision,
	); err != nil {
		t.Fatalf("idempotent Setup() error = %v", err)
	}
}

func TestPinnedDatasetSchemaUsesOptionalStrings(t *testing.T) {
	root, _ := requirePinnedSources(t)
	path := filepath.Join(root, "dataset", datasetParquetPath)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	parquetFile, err := parquet.OpenFile(file, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range parquetFile.Schema().Columns() {
		leaf, ok := parquetFile.Schema().Lookup(column...)
		if !ok || leaf.Node.Required() || leaf.Node.Type().String() != parquet.String().Type().String() {
			t.Fatalf("pinned column %v is not an optional string", column)
		}
	}
}

func TestPinnedAllPublicTasksLoad(t *testing.T) {
	root, versions := requirePinnedSources(t)
	records, err := loadDataset(filepath.Join(root, "dataset"), filepath.Join(root, "evaluator"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != publicTaskCount {
		t.Fatalf("len(records) = %d, want %d", len(records), publicTaskCount)
	}
	taskIDs := make([]string, len(records))
	for index, record := range records {
		taskIDs[index] = record.row.InstanceID
	}

	benchmark, err := New(Options{
		Root: root, TaskIDs: taskIDs, OutputDir: t.TempDir(),
		DatasetRevision: versions.SWEbenchPro.DatasetRevision, EvaluatorRevision: versions.SWEbenchPro.EvaluatorRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != publicTaskCount {
		t.Fatalf("len(tasks) = %d, want %d", len(tasks), publicTaskCount)
	}

	seen := make(map[string]struct{}, len(tasks))
	for index, task := range tasks {
		if task.ID != taskIDs[index] {
			t.Fatalf("tasks[%d].ID = %q, want %q", index, task.ID, taskIDs[index])
		}
		if _, duplicate := seen[task.ID]; duplicate {
			t.Fatalf("duplicate task ID %q", task.ID)
		}
		seen[task.ID] = struct{}{}
		if strings.TrimSpace(task.Instruction) == "" {
			t.Fatalf("task %q has an empty instruction", task.ID)
		}
		if !strings.HasPrefix(task.Environment.Image, "docker.io/jefzda/sweap-images:") ||
			task.Environment.Workdir != repositoryPath ||
			task.Environment.CPU != 4 || task.Environment.MemoryMB != 30*1024 ||
			task.Environment.StorageMB != 20*1024 || task.Environment.GPUs != 0 ||
			!task.Environment.AllowNetwork || task.Timeout != defaultAgentTimeout {
			t.Fatalf("task %q environment/timeout = %#v %s", task.ID, task.Environment, task.Timeout)
		}
		details, ok := benchmark.details[task.ID]
		if !ok || details.baseCommit == "" || details.goldCommit == "" ||
			strings.TrimSpace(details.testPatch) == "" || len(details.failToPass) == 0 ||
			len(details.selectedTests) == 0 || len(details.verifierFiles) == 0 ||
			details.runScript == "" || details.parser == "" ||
			details.snapshot != "" || details.ignoredSnapshot != "" {
			t.Fatalf("task %q has incomplete private details", task.ID)
		}
	}
}

func TestPinnedDockerGoldAndEmptyPatch(t *testing.T) {
	if os.Getenv("ARIES_SWEBENCHPRO_DOCKER_E2E") != "1" {
		t.Skip("set ARIES_SWEBENCHPRO_DOCKER_E2E=1 to pull and run the pinned task image")
	}
	root, versions := requirePinnedSources(t)
	records, err := loadDataset(filepath.Join(root, "dataset"), filepath.Join(root, "evaluator"))
	if err != nil {
		t.Fatal(err)
	}
	var goldPatch string
	for _, record := range records {
		if record.row.InstanceID == dockerE2EInstanceID {
			goldPatch = record.row.Patch
			break
		}
	}
	if strings.TrimSpace(goldPatch) == "" {
		t.Fatalf("pinned task %q has no gold patch", dockerE2EInstanceID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	probe, err := New(Options{
		Root: root, TaskIDs: []string{dockerE2EInstanceID}, OutputDir: t.TempDir(),
		DatasetRevision: versions.SWEbenchPro.DatasetRevision, EvaluatorRevision: versions.SWEbenchPro.EvaluatorRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	probeTasks, err := probe.Tasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := dockersandbox.PullImages(ctx, []string{probeTasks[0].Environment.Image}); err != nil {
		t.Fatalf("pull pinned SWE-bench Pro image: %v", err)
	}

	for _, testCase := range []struct {
		name               string
		patch              string
		wantScore          float64
		wantStatus         string
		wantVerifierStatus string
	}{
		{name: "gold", patch: goldPatch, wantScore: 1, wantStatus: core.StatusSucceeded, wantVerifierStatus: core.StatusSucceeded},
		{name: "empty", wantScore: 0, wantStatus: core.StatusFailed, wantVerifierStatus: core.StatusFailed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			outputDir := t.TempDir()
			benchmark, err := New(Options{
				Root: root, TaskIDs: []string{dockerE2EInstanceID}, OutputDir: outputDir,
				DatasetRevision: versions.SWEbenchPro.DatasetRevision, EvaluatorRevision: versions.SWEbenchPro.EvaluatorRevision,
			})
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := benchmark.Tasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := dockersandbox.New(dockersandbox.Options{
				OutputDir: outputDir, CleanupTimeout: 2 * time.Minute, Logger: logrus.New(),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			live, err := manager.Start(ctx, core.SandboxRequest{
				RunID: "swebenchpro-e2e", TaskID: tasks[0].ID, Environment: tasks[0].Environment,
			})
			if err != nil {
				t.Fatal(err)
			}
			stopped := false
			t.Cleanup(func() {
				if stopped {
					return
				}
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cleanupCancel()
				_ = manager.Stop(cleanupCtx, live)
			})
			if err := benchmark.PrepareSandbox(ctx, tasks[0], live); err != nil {
				t.Fatal(err)
			}
			if testCase.patch != "" {
				result, execErr := live.Exec(ctx, core.Command{
					Path: "/usr/bin/git", Args: []string{"-C", repositoryPath, "apply", "-v", "-"}, Stdin: []byte(testCase.patch),
				})
				if execErr != nil || result.ExitCode != 0 {
					t.Fatalf("apply gold patch: result=%#v error=%v", result, execErr)
				}
			}
			background, backgroundErr := live.Exec(ctx, core.Command{
				Path: "/bin/sh", Args: []string{"-c", "/bin/sleep 3600 >/dev/null 2>&1 &"},
			})
			if backgroundErr != nil || background.ExitCode != 0 {
				t.Fatalf("start background agent process: result=%#v error=%v", background, backgroundErr)
			}
			evaluation, evalErr := benchmark.Evaluate(ctx, tasks[0], live)
			if evalErr != nil {
				t.Fatal(evalErr)
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			err = manager.Stop(cleanupCtx, live)
			cleanupCancel()
			if err != nil {
				t.Fatal(err)
			}
			stopped = true
			if evaluation.Score != testCase.wantScore || evaluation.Reward != testCase.wantScore || evaluation.Status != testCase.wantStatus || evaluation.VerifierStatus != testCase.wantVerifierStatus || evaluation.Error != "" {
				var artifacts strings.Builder
				for _, artifactPath := range evaluation.LogPaths {
					contents, readErr := os.ReadFile(artifactPath)
					if len(contents) > 4096 {
						contents = contents[:4096]
					}
					fmt.Fprintf(&artifacts, "\n%s (read error: %v):\n%s", artifactPath, readErr, contents)
				}
				t.Fatalf("%s patch evaluation = %#v, want score/reward %.0f; artifacts:%s", testCase.name, evaluation, testCase.wantScore, artifacts.String())
			}
		})
	}
}

func requirePinnedSources(t *testing.T) (string, config.Versions) {
	t.Helper()
	versions, err := config.LoadVersions(filepath.Clean(filepath.Join("..", "..", "..", "configs", "versions.json")))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join("..", "..", "..", DefaultRoot))
	if err := Setup(
		context.Background(),
		root,
		versions.SWEbenchPro.DatasetRepositoryURL,
		versions.SWEbenchPro.DatasetRevision,
		versions.SWEbenchPro.EvaluatorRepositoryURL,
		versions.SWEbenchPro.EvaluatorRevision,
	); err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if err := VerifyRevision(context.Background(), filepath.Join(root, "dataset"), versions.SWEbenchPro.DatasetRevision); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRevision(context.Background(), filepath.Join(root, "evaluator"), versions.SWEbenchPro.EvaluatorRevision); err != nil {
		t.Fatal(err)
	}
	return root, versions
}
