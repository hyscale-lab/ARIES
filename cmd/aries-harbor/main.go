// aries-harbor inspects or replays a reviewed Harbor occurrence on a vHive VM.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

//go:embed fix_git_ready.py
var initialGitState string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := command(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(ctx context.Context) error {
	atif := flag.String("trajectory", "", "original Harbor agent/trajectory.json")
	rollout := flag.String("rollout", "", "original Codex rollout JSONL")
	image := flag.String("image", "", "prepared benchmark image in containerd (empty prints the source timeline only)")
	observer := flag.String("observer", "", "synchronous host trace executable; empty selects uninstrumented replay")
	codexTools := flag.String("codex-tools", "", "pinned original Codex and Node guest prefix archive")
	osTools := flag.String("os-tools", "", "pinned task-specific offline Ubuntu package archive")
	rg := flag.String("ripgrep", "", "Codex 0.151.0 Linux x64 bundled rg for guest preparation")
	probe := flag.String("guest-probe", "", "optional guest telemetry executable for diagnostic runs")
	sampler := flag.String("sampler", "", "host sample_vmm.py")
	samples := flag.String("samples", "", "new one-second VMM samples JSONL")
	log := flag.String("log", "", "new private JSONL run log")
	storage := flag.Int("storage-mib", 10240, "devmapper disk capacity")
	var o vhive.Options
	o.IdleMode = "checkpoint"
	flag.BoolVar(&o.GuestTaskV3, "guest-task-v3", true, "containerd-v2 stack with guest task v3 and framed streams")
	flag.BoolVar(&o.NativeCheckpoint, "native-checkpoint", false, "optimized native checkpoint and restore API")
	flag.BoolVar(&o.NativeReportingOnly, "native-reporting-only", false, "zero-target free-page reporting on the native DAMON-disabled control")
	flag.StringVar(&o.Socket, "socket", "", "containerd socket")
	flag.StringVar(&o.Namespace, "namespace", "aries", "containerd namespace")
	flag.StringVar(&o.CheckpointDir, "checkpoint-dir", "", "private host checkpoint directory")
	flag.StringVar(&o.ShimDir, "shim-dir", "", "shim metadata directory")
	flag.StringVar(&o.IODir, "io-dir", "", "runtime FIFO directory")
	flag.IntVar(&o.VCPUs, "vcpus", 1, "guest CPUs")
	flag.IntVar(&o.MemoryMB, "memory-mib", 2048, "guest memory")
	flag.Parse()
	t, err := os.ReadFile(*atif)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(*rollout)
	if err != nil {
		return err
	}
	r, err := harbor.Load(t, b)
	if err != nil {
		return err
	}
	if *image == "" {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	if *image != r.Image {
		return fmt.Errorf("trial %s requires image %s", r.Trial, r.Image)
	}
	if r.Task != "fix-git" && (*codexTools == "" || *osTools == "") {
		return errors.New("this task requires -codex-tools and -os-tools for the source guest environment")
	}
	if (*sampler == "") != (*samples == "") {
		return errors.New("provide -sampler and -samples together")
	}
	f, err := os.OpenFile(*log, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return native(ctx, o, r, *image, *observer, *sampler, *samples, *rg, *probe, *codexTools, *osTools, *storage, json.NewEncoder(f))
}

func native(ctx context.Context, o vhive.Options, r *harbor.Recording, image, observer, sampler, samples, rg, probe, codexTools, osTools string, storage int, log *json.Encoder) (retErr error) {
	m, err := vhive.New(o)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, m.Close()) }()
	name := fmt.Sprintf("aries-%s-%d", r.Task, time.Now().UnixNano())
	lifecycle := func(phase string) error {
		t, err := harbor.Clock()
		if err != nil {
			return err
		}
		return log.Encode(map[string]any{"event": phase, "monotonic_ns": t, "runtime_id": name, "task": r.Task, "trial_id": r.Trial, "job_id": r.Job, "row_id": r.Row, "image": image, "trajectory_sha256": r.TrajectoryHash, "rollout_sha256": r.RolloutHash, "observer": observer, "memory_mib": o.MemoryMB, "storage_mib": storage, "vcpus": o.VCPUs})
	}
	if err = lifecycle("create_start"); err != nil {
		return err
	}
	req := deployment.Request{Name: name, Image: image, StorageMB: storage, Workdir: r.Workdir, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.component": "sandbox", "aries.kind": "task-container", "aries.run": name, "aries.task": r.Task}}
	id, err := m.Create(ctx, req)
	if id != "" {
		defer func() {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer done()
			stopErr := m.Stop(cleanup, id)
			retErr = errors.Join(retErr, stopErr)
			if stopErr == nil {
				retErr = errors.Join(retErr, lifecycle("removed"))
			}
		}()
	}
	if err != nil {
		return err
	}
	validateCtx, timings := core.WithCommandTimings(ctx)
	if err = m.Validate(validateCtx, id, req, nil); err != nil {
		logErr := log.Encode(map[string]any{"event": "validation_error", "error": err.Error(), "command_timing": timings.Snapshot()})
		return errors.Join(err, logErr)
	}
	for _, helper := range []struct{ name, path, expected string }{
		{"rg", rg, "e62198eb19b136b88c330af83647b5a962cb99b6b1f066758568f12de1974849"},
		{"zb-guest", probe, ""},
	} {
		if helper.path == "" {
			continue
		}
		data, err := os.ReadFile(helper.path)
		if err != nil {
			return err
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(data))
		if helper.expected != "" && sum != helper.expected {
			return fmt.Errorf("incorrect %s helper digest", helper.name)
		}
		archive, err := harness.StageArchive(nil, map[string]harness.ArchiveFile{helper.name: {Content: data, Mode: 0755}})
		if err != nil {
			return err
		}
		if err = m.UploadArchive(ctx, id, "/usr/local/bin", bytes.NewReader(archive)); err != nil {
			return fmt.Errorf("prepare guest %s: %w", helper.name, err)
		}
		check, err := m.Exec(ctx, id, core.Command{Path: "/usr/bin/sha256sum", Args: []string{"/usr/local/bin/" + helper.name}})
		if err != nil || check.ExitCode != 0 || !strings.HasPrefix(check.Stdout, sum+" ") {
			return errors.Join(fmt.Errorf("verify guest helper %s", helper.name), err)
		}
		if err = log.Encode(map[string]any{"event": "guest_prepared", "helper": helper.name, "guest_sha256": sum}); err != nil {
			return err
		}
	}
	prepBegin, err := harbor.Clock()
	if err != nil {
		return err
	}
	event, state, err := prepareTask(ctx, m, id, r, codexTools, osTools)
	prepEnd, clockErr := harbor.Clock()
	if err != nil || clockErr != nil {
		return errors.Join(err, clockErr)
	}
	if err = log.Encode(map[string]any{"event": event, "begin_monotonic_ns": prepBegin, "end_monotonic_ns": prepEnd, "state": state}); err != nil {
		return err
	}
	if err = m.Start(ctx, id); err != nil {
		return err
	}
	if err = lifecycle("snapshot_start"); err != nil {
		return err
	}
	if err = m.Suspend(ctx, id); err != nil {
		return err
	}
	if err = lifecycle("snapshot_complete"); err != nil {
		return err
	}
	if err = lifecycle("restore_start"); err != nil {
		return err
	}
	if err = m.Resume(ctx, id); err != nil {
		return err
	}
	if err = lifecycle("restore_complete"); err != nil {
		return err
	}
	socket, err := m.VMMSocket(ctx, id)
	if err != nil {
		return err
	}
	if sampler != "" {
		stop, err := startSampler(ctx, sampler, socket, samples)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, stop()) }()
		if err = lifecycle("sampler_ready"); err != nil {
			return err
		}
	}
	h := harbor.Hooks{
		Record: func(e harbor.Receipt) error { return log.Encode(e) },
		Exec: func(ctx context.Context, call replay.Call) (core.CommandResult, error) {
			return m.Exec(ctx, id, core.Command{Path: "/bin/bash", Args: []string{"-lc", call.Script}, Dir: call.Workdir})
		},
		Boundary: func(ctx context.Context, phase string, step int) error {
			if observer != "" {
				cmd := exec.CommandContext(ctx, observer, phase, id, fmt.Sprint(step), socket)
				cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
				cmd.WaitDelay = 30 * time.Second
				cmd.Stdout = os.Stderr
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return err
				}
			}
			if probe != "" && (phase == "post-tool" || phase == "session-start") {
				begin, err := harbor.Clock()
				if err != nil {
					return err
				}
				out, err := m.Exec(ctx, id, core.Command{Path: "/usr/local/bin/zb-guest", Args: []string{"snapshot"}})
				end, clockErr := harbor.Clock()
				if err != nil || clockErr != nil || out.ExitCode != 0 || !json.Valid([]byte(out.Stdout)) {
					return errors.Join(errors.New("guest memory probe failed"), err, clockErr)
				}
				return log.Encode(map[string]any{"event": "guest_probe", "after": phase, "step_id": step, "begin_monotonic_ns": begin, "end_monotonic_ns": end, "sample": json.RawMessage(out.Stdout)})
			}
			return nil
		},
	}
	if err := harbor.Run(ctx, r, h); err != nil {
		return err
	}
	if r.Task != "fix-git" {
		return captureTaskOutput(ctx, m, id, r, log)
	}
	return nil
}
