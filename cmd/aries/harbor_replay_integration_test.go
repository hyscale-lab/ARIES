//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bridgegrpc "github.com/hyscale-lab/aries/pkg/bridge/grpc"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/client"
)

const (
	harborFixtureImage = "docker.io/library/debian:12.9"
	harborFixtureRun   = "synthetic-harbor"
	harborFirst        = "task__first"
	harborLater        = "task__later"
	harborStrict       = "task__strict"
	harborInterval     = 250 * time.Millisecond
	harborStrictOffset = 375 * time.Millisecond
	harborFinalWait    = 2 * time.Second
	harborTestTimeout  = 2 * time.Minute
)

func TestHarborConversionToCLIReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), harborTestTimeout)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	versionsFile := filepath.Join(root, "configs", "versions.json")
	versions, err := config.LoadVersions(versionsFile)
	if err != nil {
		t.Fatal(err)
	}
	api, err := client.New()
	if err != nil {
		t.Skipf("Docker client unavailable: %v", err)
	}
	t.Cleanup(func() { _ = api.Close() })
	for _, image := range []string{versions.Bridge.Image, harborFixtureImage} {
		if _, err := api.ImageInspect(ctx, image); err != nil {
			t.Skipf("Docker or fixture image %q unavailable: %v", image, err)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to convert the Harbor fixture")
	}

	dir := t.TempDir()
	input, source := filepath.Join(dir, "harbor"), filepath.Join(dir, "source")
	runHarborFixtureCommand(t, ctx, python, "testdata/harbor_replay.py", input)
	runHarborFixtureCommand(t, ctx, python, filepath.Join(root, "scripts", "convert_harbor.py"),
		"--harbor-run", input, "--output-dir", source)
	strictMetadata := filepath.Join(source, harborStrict, "bridge", "runtime.json")
	var metadata map[string]any
	readHarborJSON(t, strictMetadata, &metadata)
	// Add a known source image difference to the strict failure case. Harbor did not record an image.
	metadata["image"] = "fixture.invalid/source:recorded"
	writeHarborJSON(t, strictMetadata, metadata)

	catalog := filepath.Join(dir, "catalog")
	taskDir := filepath.Join(catalog, "task")
	if err := os.MkdirAll(filepath.Join(taskDir, "tests"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHarborFile(t, filepath.Join(taskDir, "instruction.md"), []byte("Synthetic replay fixture.\n"))
	writeHarborFile(t, filepath.Join(taskDir, "tests", "test.sh"), []byte("#!/bin/sh\nexit 1\n"))
	writeHarborFile(t, filepath.Join(taskDir, "task.toml"), []byte(fmt.Sprintf(`schema_version = "1.1"
[task]
name = "terminal-bench/task"
[agent]
timeout_sec = 60
[verifier]
timeout_sec = 60
[environment]
build_timeout_sec = 60
docker_image = %q
cpus = 1
memory_mb = 256
storage_mb = 1024
`, harborFixtureImage)))
	runHarborFixtureCommand(t, ctx, "git", "-C", catalog, "init", "-q")
	runHarborFixtureCommand(t, ctx, "git", "-C", catalog, "add", ".")
	runHarborFixtureCommand(t, ctx, "git", "-C", catalog, "-c", "user.name=ARIES", "-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null", "commit", "-qm", "Add synthetic task")
	revision := strings.TrimSpace(runHarborFixtureCommand(t, ctx, "git", "-C", catalog, "rev-parse", "HEAD"))
	versions.TerminalBench2.Revision = revision
	versionsFile = filepath.Join(dir, "versions.json")
	writeHarborJSON(t, versionsFile, versions)

	task := core.Task{ID: "task", Environment: core.Environment{Image: harborFixtureImage}}
	revisions := map[string]string{"revision": revision}
	whole := checkHarborSchedule(t, ctx, source, task, revisions, []time.Duration{0, harborInterval, harborStrictOffset})
	checkHarborSchedule(t, ctx, filepath.Join(source, harborLater), task, revisions, []time.Duration{0})

	binary := filepath.Join(dir, "aries")
	runHarborFixtureCommand(t, ctx, "go", "build", "-o", binary, ".")
	bridgeBinary, err := os.ReadFile(filepath.Join(root, "bin", "aries-bridge"))
	if err != nil {
		t.Skipf("run make bridge-image to prepare the bridge executable: %v", err)
	}
	writeHarborFile(t, filepath.Join(dir, "aries-bridge"), bridgeBinary)
	for _, tc := range []struct {
		name, selectPath string
		occurrences      []string
	}{
		{"whole", "source", []string{harborFirst, harborLater, harborStrict}},
		{"one", filepath.Join("source", harborLater), []string{harborLater}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := map[string]string{"backend": "docker"}
			output := filepath.Join(dir, tc.name)
			profile := output + ".json"
			const concurrency = 2
			writeHarborJSON(t, profile, map[string]any{
				"name": "harbor-fixture", "versions_file": versionsFile, "overrides_file": "",
				"benchmark":  map[string]any{"type": "terminalbench2", "root": catalog, "tasks": []string{task.ID}},
				"sandbox":    map[string]any{"deployment": target},
				"bridge":     map[string]any{"deployment": target, "type": "hermes-grpc", "mode": "managed"},
				"replay":     map[string]any{"deployment": target, "source": tc.selectPath},
				"execution":  map[string]int{"concurrency": concurrency},
				"output_dir": output,
			})
			var stderr bytes.Buffer
			command := exec.CommandContext(ctx, binary, profile)
			command.Dir = root
			command.Stderr = &stderr
			stdout, err := command.Output()
			var exit *exec.ExitError
			wantFailure := tc.name == "whole"
			if wantFailure && !errors.As(err, &exit) || !wantFailure && err != nil {
				t.Fatalf("aries exit: %v\n%s\n%s", err, stdout, stderr.String())
			}
			var result struct {
				core.RunResult
				SavedTasks []struct {
					core.TaskResult
					Grade map[string]json.RawMessage `json:"evaluation"`
				} `json:"tasks"`
			}
			if err := json.Unmarshal(stdout, &result); err != nil {
				t.Fatalf("decode aries output: %v\n%s", err, stderr.String())
			}
			if result.RunID == "" || result.RunID == harborFixtureRun {
				t.Fatalf("invalid execution run ID %q", result.RunID)
			}
			t.Cleanup(func() { checkHarborDockerCleanup(t, api, result.RunID) })
			runDir := filepath.Join(output, result.RunID)
			saved, err := os.ReadFile(filepath.Join(runDir, "run-result.json"))
			if err != nil || !bytes.Equal(saved, stdout) {
				t.Fatalf("saved result differs from stdout: %v", err)
			}
			if result.Infrastructure == nil || result.Infrastructure.Cleanup.Status != core.StatusSucceeded ||
				result.Infrastructure.Error != "" || result.Summary.EvaluationsRun != 0 ||
				len(result.SavedTasks) != len(tc.occurrences) {
				t.Fatalf("unexpected run result: %s", saved)
			}
			runLog, err := os.ReadFile(filepath.Join(runDir, "aries.log"))
			if err != nil {
				t.Fatal(err)
			}
			sandboxes := map[string]bool{}
			var sharedEpoch, firstAdmitted, firstFinished, laterAdmitted, laterFinished time.Time
			for i, got := range result.SavedTasks {
				id := tc.occurrences[i]
				checkHarborGrade(t, id, got.Grade)
				if got.Replay == nil || got.Harness.Status != core.StatusNotStarted || !got.Isolation.CallerStopped ||
					!got.Isolation.BridgeRevoked || got.Cleanup.Status != core.StatusSucceeded {
					t.Fatalf("%s: invalid identity or lifecycle: %+v", id, got)
				}
				expected := []string{"matched", "matched"}
				if id == harborFirst {
					expected[len(expected)-1] = "drifted"
				}
				status := core.StatusSucceeded
				if id == harborStrict {
					status = core.StatusFailed
					expected = []string{"length_mismatch"}
					if !strings.Contains(got.Replay.Error, "Exec output length mismatch") || !strings.Contains(got.Error, got.Replay.Error) {
						t.Fatalf("%s: strict replay error missing: %+v", id, got)
					}
				} else if got.Error != "" || got.Replay.Error != "" {
					t.Fatalf("%s: diagnostic drift failed replay: %+v", id, got)
				}
				responses := got.Replay.Responses
				var checks []string
				if responses != nil {
					for _, call := range responses.Calls {
						checks = append(checks, call.Status)
					}
				}
				if got.Replay.Status != status || !slices.Equal(checks, expected) {
					t.Fatalf("%s: replay = %+v; response checks = %v, want %v", id, got.Replay, checks, expected)
				}
				checkHarborEnvironment(t, runLog, id)
				bridge := filepath.Join(runDir, got.TaskID, "bridge")
				var runtime struct{ TargetRuntimeID string }
				readHarborJSON(t, filepath.Join(bridge, "runtime.json"), &runtime)
				if runtime.TargetRuntimeID == "" || sandboxes[runtime.TargetRuntimeID] {
					t.Fatalf("%s: no fresh sandbox identity: %q", id, runtime.TargetRuntimeID)
				}
				sandboxes[runtime.TargetRuntimeID] = true
				basis := "occurrence_first_start"
				var offset time.Duration
				if wantFailure {
					basis = "shared_replay_origin"
					offset = whole.Occurrences[i].StartedAt.Sub(whole.StartedAt)
				}
				starts := checkHarborStarts(t, filepath.Join(bridge, "tool-calls.jsonl"), got.Replay.Timing, basis, len(expected), offset)
				finished := got.StartedAt.Add(got.Duration)
				latest := slices.MaxFunc(starts, time.Time.Compare)
				if id != harborStrict && finished.Before(latest.Add(harborFinalWait)) {
					t.Fatalf("%s: trial finished before its final wait", id)
				}
				if wantFailure {
					epoch := starts[0].Add(-offset - time.Duration(got.Replay.Timing.Calls[0].DriftNS))
					if i == 0 {
						sharedEpoch, firstAdmitted, firstFinished = epoch, got.StartedAt, finished
					} else if !epoch.Equal(sharedEpoch) {
						t.Fatalf("%s: timing offset reset the shared epoch", id)
					}
					if id == harborLater {
						laterAdmitted, laterFinished = got.StartedAt, finished
					}
					if id == harborStrict && (!got.StartedAt.After(sharedEpoch.Add(offset)) ||
						got.Replay.Timing.Calls[0].DriftNS <= 0) {
						t.Fatal("queued trial did not retain its offset after late admission")
					}
				}
			}
			if wantFailure && !(laterAdmitted.Before(firstFinished) && firstAdmitted.Before(laterFinished)) {
				t.Fatal("first and second occurrences did not overlap")
			}
		})
	}
}

func checkHarborSchedule(t *testing.T, ctx context.Context, source string, task core.Task, revisions map[string]string, offsets []time.Duration) *bridgegrpc.ReplaySelection {
	t.Helper()
	selected, err := bridgegrpc.PreflightReplaySource(ctx, source, []core.Task{task}, "terminalbench2", revisions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Occurrences) != len(offsets) {
		t.Fatalf("selected %d occurrences, want %d", len(selected.Occurrences), len(offsets))
	}
	for i, offset := range offsets {
		plan, err := selected.Load(ctx, i)
		if err != nil {
			t.Fatal(err)
		}
		if plan.OriginType != "harbor" || len(plan.Calls) == 0 ||
			plan.Calls[0].StartOffset != offset || plan.FinalWait != harborFinalWait {
			t.Fatalf("source schedule for %s: %+v", plan.SourceTaskID, plan)
		}
	}
	return selected
}

func TestHarborOccurrenceTimingWithReversedAdmission(t *testing.T) {
	const firstSourceSequence = 1
	const secondSourceSequence = firstSourceSequence + 1
	const admissionGap = harborInterval / 2
	const basis = "occurrence_first_start"
	epoch, err := time.Parse(time.RFC3339, "2026-11-05T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(t.TempDir(), "tool-calls.jsonl")
	writeHarborFile(t, audit, fmt.Appendf(nil,
		"{\"replay_source_sequence\":%d,\"started_at\":%q}\n{\"replay_source_sequence\":%d,\"started_at\":%q}\n",
		firstSourceSequence, epoch.Add(admissionGap).Format(time.RFC3339Nano),
		secondSourceSequence, epoch.Format(time.RFC3339Nano)))
	timing := core.ReplayTiming{Status: "available", Basis: basis, Calls: []core.ReplayCallTiming{
		{SourceSequence: firstSourceSequence, DriftNS: admissionGap.Nanoseconds()},
		{SourceSequence: secondSourceSequence, DriftNS: -harborInterval.Nanoseconds()},
	}}
	checkHarborStarts(t, audit, &timing, basis, len(timing.Calls), 0)
}

func checkHarborStarts(t *testing.T, audit string, timing *core.ReplayTiming, basis string, count int, offset time.Duration) []time.Time {
	t.Helper()
	if timing == nil || timing.Status != "available" || timing.Basis != basis || len(timing.Calls) != count {
		t.Fatalf("unexpected timing for %s: %+v", audit, timing)
	}
	data, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	starts := make([]time.Time, count)
	for decoder := json.NewDecoder(bytes.NewReader(data)); decoder.More(); {
		var call struct {
			Sequence int       `json:"replay_source_sequence"`
			Started  time.Time `json:"started_at"`
		}
		if err := decoder.Decode(&call); err != nil {
			t.Fatal(err)
		}
		if call.Sequence < 1 || call.Sequence > count || !starts[call.Sequence-1].IsZero() {
			t.Fatalf("invalid source sequence: %+v", call)
		}
		starts[call.Sequence-1] = call.Started
	}
	epoch := slices.MinFunc(starts, time.Time.Compare)
	if basis == "shared_replay_origin" {
		epoch = starts[0].Add(-offset - time.Duration(timing.Calls[0].DriftNS))
	}
	for i, observed := range starts {
		scheduled := offset + time.Duration(i)*harborInterval
		if observed.IsZero() || timing.Calls[i].SourceSequence != uint64(i+1) ||
			time.Duration(timing.Calls[i].DriftNS) != observed.Sub(epoch)-scheduled {
			t.Fatalf("call %d: drift does not match the recorded schedule", i+1)
		}
	}
	return starts
}

func checkHarborGrade(t *testing.T, id string, fields map[string]json.RawMessage) {
	t.Helper()
	want := map[string]string{"source_run_id": `"` + harborFixtureRun + `"`, "source_task_id": `"` + id + `"`}
	switch id {
	case harborFirst:
		want["score"], want["reward"] = "0", "1"
	case harborLater:
		want["reward"] = "0"
	}
	gradeStatus := `"unavailable"`
	if want["reward"] != "" {
		gradeStatus, want["reused"] = `"succeeded"`, "true"
	}
	want["status"] = gradeStatus
	got := map[string]string{}
	for _, key := range []string{"source_run_id", "source_task_id", "score", "reward", "reused", "status"} {
		if value, ok := fields[key]; ok {
			got[key] = string(value)
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("%s: saved grade = %v, want %v", id, got, want)
	}
}

func checkHarborEnvironment(t *testing.T, log []byte, id string) {
	t.Helper()
	want := "unverified"
	if id == harborStrict {
		want = "different"
	}
	for decoder := json.NewDecoder(bytes.NewReader(log)); decoder.More(); {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["msg"] == "replay starting environment check" && entry["source_task_id"] == id &&
			entry["source_run_id"] == harborFixtureRun && entry["origin_type"] == "harbor" &&
			entry["attribute"] == "image" && entry["comparison"] == want {
			return
		}
	}
	t.Fatalf("%s: missing %s Harbor environment diagnostic", id, want)
}

func checkHarborDockerCleanup(t *testing.T, api *client.Client, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), harborTestTimeout)
	defer cancel()
	filter := make(client.Filters).Add("label", "aries.run="+runID)
	containers, err := api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filter})
	if err != nil || len(containers.Items) != 0 {
		t.Errorf("run containers remain or could not be checked: %+v, %v", containers.Items, err)
	}
	networks, err := api.NetworkList(ctx, client.NetworkListOptions{Filters: filter})
	if err != nil || len(networks.Items) != 0 {
		t.Errorf("run networks remain or could not be checked: %+v, %v", networks.Items, err)
	}
}

func runHarborFixtureCommand(t *testing.T, ctx context.Context, command string, args ...string) string {
	t.Helper()
	output, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", command, args, err, output)
	}
	return string(output)
}

func readHarborJSON(t *testing.T, path string, into any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func writeHarborJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeHarborFile(t, path, data)
}

func writeHarborFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
