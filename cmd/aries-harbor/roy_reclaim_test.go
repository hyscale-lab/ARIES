package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

//go:embed roy_reclaim_guest.py
var royReclaimGuest string

const reclaimPilot = "d16a13fe-8381-4594-b6f6-60bb6f3aa660"

func reclaimRequired(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func reclaimRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	reclaimRequired(t, err)
	return data
}

func reclaimArtifact(path string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(path, func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("nonregular native artifact member %s", file)
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		rel, err := filepath.Rel(path, file)
		if err != nil {
			return err
		}
		files[rel] = fmt.Sprintf("%x", h.Sum(nil))
		return nil
	})
	return files, err
}

func reclaimHost(pid int) (map[string]int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return nil, err
	}
	fields := map[string]int64{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, _ := strings.Cut(line, ":")
		if key != "Rss" && key != "Pss" {
			continue
		}
		kib, err := strconv.ParseInt(strings.Fields(value)[0], 10, 64)
		if err != nil {
			return nil, err
		}
		fields[key] = kib * 1024
	}
	if len(fields) != 2 {
		return nil, fmt.Errorf("missing VMM rollup counters")
	}
	return fields, nil
}

// TestRoyReclaimPair is an opt-in native integration experiment. The only
// accepted inputs are the five calls of the pinned non-networked Roy pilot.
func TestRoyReclaimPair(t *testing.T) {
	stage := os.Getenv("ZB_ROY_RECLAIM_STAGE")
	if stage == "" {
		t.Skip("requires the dedicated native Roy diagnostic stage")
	}
	const runtime = "/local/zb-v5-pilot-01/runtime"
	const unit = "/run/fcmod-zepbound-amd191-v5-pilot-01"
	ctx, cancel := context.WithTimeout(context.Background(), 16*time.Minute)
	defer cancel()
	r, err := harbor.Load(reclaimRead(t, filepath.Join(stage, "trajectory")), reclaimRead(t, filepath.Join(stage, "rollout")))
	reclaimRequired(t, err)
	if r.Trial != reclaimPilot || r.Task != royTask || r.NeedsNetwork || len(r.Plan.Calls) != 5 ||
		r.VCPUs != 2 || r.MemoryMiB != 4096 || r.StorageMiB != 10240 {
		t.Fatal("wrong original Roy occurrence or resources")
	}
	for _, call := range r.Plan.Calls {
		if call.Operation != "Exec" {
			t.Fatal("unexpected operation for the fixed Roy diagnostic")
		}
	}

	o := vhive.Options{Socket: unit + "/containerd.sock", Namespace: "aries", CheckpointDir: runtime + "/checkpoints",
		ShimDir: runtime + "/shim", IODir: unit + "/aries-io", IdleMode: "checkpoint",
		NativeCheckpoint: true, GuestTaskV3: true, VCPUs: r.VCPUs, MemoryMB: r.MemoryMiB}
	m, err := vhive.New(o)
	reclaimRequired(t, err)
	defer m.Close()

	logs := make(map[string]*json.Encoder)
	for _, arm := range []string{"control", "reclaim"} {
		f, err := os.OpenFile(filepath.Join(stage, arm, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		reclaimRequired(t, err)
		defer f.Close()
		logs[arm] = json.NewEncoder(f)
	}
	name := fmt.Sprintf("aries-roy-reclaim-%d", time.Now().UnixNano())
	req := deployment.Request{Name: name, Image: r.Image,
		Workdir: r.Workdir, StorageMB: r.StorageMiB, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"},
		Labels: map[string]string{"aries.managed": "true", "aries.component": "sandbox", "aries.kind": "task-container", "aries.run": name, "aries.task": r.Task}}
	event := func(log *json.Encoder, phase string, data any) error {
		now, err := harbor.Clock()
		if err != nil {
			return err
		}
		return log.Encode(map[string]any{"event": phase, "monotonic_ns": now, "runtime_id": req.Name,
			"task": r.Task, "trial_id": r.Trial, "job_id": r.Job, "image": r.Image, "trajectory_sha256": r.TrajectoryHash,
			"rollout_sha256": r.RolloutHash, "memory_mib": r.MemoryMiB, "storage_mib": r.StorageMiB, "vcpus": r.VCPUs, "data": data})
	}
	for _, log := range logs {
		reclaimRequired(t, event(log, "create_start", nil))
	}
	id, err := m.Create(ctx, req)
	if id != "" {
		defer func() {
			cleanup, done := context.WithTimeout(context.Background(), 2*time.Minute)
			defer done()
			if err := m.Stop(cleanup, id); err != nil {
				t.Error(err)
				return
			}
			for _, log := range logs {
				if err := event(log, "removed", nil); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	reclaimRequired(t, err)
	if id != req.Name {
		t.Fatal("native runtime did not retain requested owner")
	}
	reclaimRequired(t, m.Validate(ctx, id, req, nil))
	kind, ready, err := prepareTask(ctx, m, id, r, "", "", "", "", "", "")
	reclaimRequired(t, err)
	probe := func(ctx context.Context, mode string) (string, error) {
		return checked(ctx, m, id, core.Command{Path: "/opt/venv/bin/python", Args: []string{"-I", "-c", royReclaimGuest, mode}, Dir: r.Workdir})
	}
	capabilities, err := probe(ctx, "capabilities")
	reclaimRequired(t, err)
	if !json.Valid([]byte(capabilities)) {
		t.Fatal("missing guest capability receipt")
	}
	for _, log := range logs {
		reclaimRequired(t, event(log, kind, ready))
		reclaimRequired(t, event(log, "guest_reclaim_capabilities", json.RawMessage(capabilities)))
	}
	reclaimRequired(t, m.Start(ctx, id))
	for _, log := range logs {
		reclaimRequired(t, event(log, "snapshot_start", nil))
	}
	reclaimRequired(t, m.Suspend(ctx, id))
	artifacts, err := filepath.Glob(filepath.Join(o.CheckpointDir, o.Namespace, id, "snapshot-*", "native"))
	reclaimRequired(t, err)
	if len(artifacts) != 1 {
		t.Fatal("expected one native checkpoint artifact")
	}
	baseline, err := reclaimArtifact(artifacts[0])
	reclaimRequired(t, err)
	if baseline["manifest.json"] == "" || baseline["memory"] == "" || baseline["state"] == "" {
		t.Fatal("incomplete native checkpoint")
	}
	var capturedTask string
	var earlierPID int
	for _, arm := range []string{"control", "reclaim"} {
		log := logs[arm]
		sums, err := reclaimArtifact(artifacts[0])
		reclaimRequired(t, err)
		if !reflect.DeepEqual(sums, baseline) {
			t.Fatal("native checkpoint artifact changed before restore")
		}
		reclaimRequired(t, event(log, "snapshot_complete", sums))
		reclaimRequired(t, event(log, "restore_start", nil))
		reclaimRequired(t, m.Resume(ctx, id))
		reclaimRequired(t, event(log, "restore_complete", nil))
		identity, err := checked(ctx, m, id, core.Command{Path: "/bin/sh", Args: []string{"-ec",
			"cat /proc/sys/kernel/random/boot_id; awk '{print $1, $22}' /proc/1/stat"}})
		reclaimRequired(t, err)
		if arm == "control" {
			capturedTask = identity
		}
		if identity == "" || identity != capturedTask {
			t.Fatal("restored guest boot or process identity changed")
		}
		reclaimRequired(t, event(log, "restored_guest_identity", identity))
		socket, err := m.VMMSocket(ctx, id)
		reclaimRequired(t, err)
		dir := filepath.Join(stage, arm)
		reclaimRequired(t, os.Setenv("ZB_CGROUP_EVENTS", filepath.Join(dir, "events.jsonl")))
		samplePath := filepath.Join(dir, "samples.jsonl")
		stop, err := harbor.StartVMMSampler(ctx, filepath.Join(stage, "zb-trace/tb41/sample.py"), socket, samplePath)
		reclaimRequired(t, err)
		func() {
			defer func() { reclaimRequired(t, stop()) }()
			line, _, _ := strings.Cut(string(reclaimRead(t, samplePath)), "\n")
			var first struct {
				PID int `json:"pid"`
			}
			reclaimRequired(t, json.Unmarshal([]byte(line), &first))
			if first.PID <= 0 || first.PID == earlierPID {
				t.Fatal("restores did not have distinct VMM processes")
			}
			earlierPID = first.PID
			reclaimRequired(t, event(log, "sampler_ready", first))
			sessions := harbor.NewCodexProcessSessions(ctx, func(ctx context.Context, call replay.Call, out, stderr io.Writer) (core.CommandResult, error) {
				return m.ExecStream(ctx, id, sourceShellCommand(r.Task, r.Trial, call), nil, out, stderr)
			})
			defer sessions.Close()
			nativeCapture := nativePeriodic(filepath.Join(dir, "boundary.sock"), id, socket)
			hooks := harbor.Hooks{Record: func(rec harbor.Receipt) error { return log.Encode(rec) },
				Exec: sessions.ObserveRelaxed,
				Periodic: func(ctx context.Context, phase string, step int, triggers []string) (harbor.Freeze, error) {
					freeze, err := nativeCapture(ctx, phase, step, triggers)
					if err != nil {
						return freeze, err
					}
					mode := "read"
					switch {
					case phase == "post-tool" && step == 7:
						mode = arm
					case step == 10 && (phase == "pre-tool" || phase == "post-tool"):
					default:
						return freeze, nil
					}
					begin, err := harbor.Clock()
					if err != nil {
						return freeze, err
					}
					hostBefore, err := reclaimHost(first.PID)
					if err != nil {
						return freeze, err
					}
					s, err := probe(ctx, mode)
					if err != nil || !json.Valid([]byte(s)) {
						return freeze, fmt.Errorf("targeted guest sample at %s step %d: %w", phase, step, err)
					}
					hostAfter, err := reclaimHost(first.PID)
					if err != nil {
						return freeze, err
					}
					return freeze, event(log, "reclaim_observation", map[string]any{"step": step, "phase": phase,
						"arm": arm, "probe_host_begin_ns": begin, "host_before_bytes": hostBefore,
						"host_after_bytes": hostAfter, "guest": json.RawMessage(s)})
				}}
			reclaimRequired(t, harbor.Run(ctx, r, hooks))
			reclaimRequired(t, captureTaskOutput(context.WithoutCancel(ctx), m, id, r, log, false))
		}()
		if arm == "control" {
			reclaimRequired(t, m.ParkNativeCheckpoint(ctx, id))
			reclaimRequired(t, event(log, "parked_without_recapture", nil))
		}
	}
	after, err := reclaimArtifact(artifacts[0])
	reclaimRequired(t, err)
	if !reflect.DeepEqual(after, baseline) {
		t.Fatal("native checkpoint artifact changed after the paired replay")
	}
	for _, log := range logs {
		reclaimRequired(t, event(log, "immutable_artifact_verified", after))
	}
}
