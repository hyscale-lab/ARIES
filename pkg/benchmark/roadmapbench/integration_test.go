//go:build integration

package roadmapbench

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
)

func TestPinnedAllRoadmapBenchTasksLoad(t *testing.T) {
	root := filepath.Join("..", "..", "..", DefaultRoot)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		t.Skip("pinned RoadmapBench checkout is absent; run aries setup with a RoadmapBench profile")
	} else if err != nil {
		t.Fatal(err)
	}
	versions, err := config.LoadVersions(filepath.Join("..", "..", "..", "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	pin := versions.RoadmapBench
	if err := Setup(context.Background(), root, pin.RepositoryURL, pin.Revision); err != nil {
		t.Fatalf("idempotent pinned setup: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != ".git" {
			ids = append(ids, entry.Name())
		}
	}
	if len(ids) != 115 {
		t.Fatalf("pinned task count = %d, want 115", len(ids))
	}
	b, err := New(Options{Root: root, TaskIDs: ids, OutputDir: t.TempDir(), Revision: pin.Revision})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := b.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for index, task := range tasks {
		if task.ID != ids[index] || task.Instruction == "" || task.Timeout != 2*time.Hour || task.Environment.Workdir != "/app" || task.Environment.CPU != 2 || !task.Environment.AllowNetwork || task.Environment.Image != "znpt/roadmapbench-"+task.ID+":latest" {
			t.Fatalf("pinned task %s has unexpected mapping", task.ID)
		}
		if len(b.details[task.ID].verifierFiles) == 0 {
			t.Fatalf("task %s has no private verifier", task.ID)
		}
		for _, excluded := range []string{"environment/repo", "solution"} {
			if _, err := os.Lstat(filepath.Join(root, task.ID, excluded)); !os.IsNotExist(err) {
				t.Fatalf("sparse checkout unexpectedly contains %s/%s: %v", task.ID, excluded, err)
			}
		}
	}
}

func TestDockerEvaluationPreservesLiveEditsAndCleansUp(t *testing.T) {
	const image = "docker.io/library/debian:12.12-slim"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := dockersandbox.PullImages(ctx, []string{image}); err != nil {
		t.Fatal(err)
	}
	for _, score := range []float64{0, 0.5, 1} {
		t.Run(fmt.Sprint(score), func(t *testing.T) {
			root := writeFixture(t)
			writeFile(t, filepath.Join(root, fixtureTaskID, "task.toml"), strings.ReplaceAll(fixtureTaskTOML, "znpt/roadmapbench-opt-3.0.0-roadmap", image))
			writeFile(t, filepath.Join(root, fixtureTaskID, "tests", "test.sh"), `#!/bin/bash
set -eu
reward=0
if [ -f /tmp/opt_test_results/stale_reward ]; then reward=$(cat /tmp/opt_test_results/stale_reward); fi
if [ -f /app/partial ]; then reward=0.5; fi
if [ -f /app/complete ]; then reward=1; fi
printf '%s\n' "$reward" > /logs/verifier/reward.txt
printf '{"reward":%s}\n' "$reward" > /logs/verifier/reward.json
`)
			commitFixture(t, root)
			output := t.TempDir()
			b, err := New(testOptions(root, []string{fixtureTaskID}, output))
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := b.Tasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			logger := logrus.New()
			logger.SetOutput(io.Discard)
			manager, err := dockersandbox.New(dockersandbox.Options{OutputDir: output, Logger: logger})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			live, err := manager.Start(ctx, core.SandboxRequest{RunID: fmt.Sprintf("roadmapbench-integration-%d", time.Now().UnixNano()), TaskID: fixtureTaskID, Environment: tasks[0].Environment})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, done := context.WithTimeout(context.Background(), time.Minute)
				defer done()
				if err := manager.Stop(cleanup, live); err != nil {
					t.Errorf("sandbox cleanup: %v", err)
				}
			})
			if err := execChecked(ctx, live, core.Command{Path: "/bin/mkdir", Args: []string{"-p", "/tests", "/solution", "/opt/polars-upgrade"}}); err != nil {
				t.Fatal(err)
			}
			if err := b.PrepareSandbox(ctx, tasks[0], live); err != nil {
				t.Fatal(err)
			}
			// Model the official Optuna script's stale-result behavior after a
			// failed pytest call, while retaining unrelated dependency caches.
			if err := execChecked(ctx, live, core.Command{Path: "/bin/sh", Args: []string{"-c", "mkdir -p /tmp/opt_test_results /app/build/_deps; printf 1 > /tmp/opt_test_results/stale_reward; touch /app/build/_deps/keep"}}); err != nil {
				t.Fatal(err)
			}
			if score > 0 {
				file := "/app/partial"
				if score == 1 {
					file = "/app/complete"
				}
				if err := execChecked(ctx, live, core.Command{Path: "/usr/bin/touch", Args: []string{file}}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := b.Evaluate(ctx, tasks[0], live)
			if err != nil || got.Reward != score {
				t.Fatalf("live evaluation = %+v, %v", got, err)
			}
			if err := execChecked(ctx, live, core.Command{Path: "/usr/bin/test", Args: []string{"-f", "/app/build/_deps/keep"}}); err != nil {
				t.Fatalf("dependency cache removed: %v", err)
			}
			if err := manager.Stop(ctx, live); err != nil {
				t.Fatalf("positive cleanup: %v", err)
			}
		})
	}
}
