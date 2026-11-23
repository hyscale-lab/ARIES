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
	"io"
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

func sourceShellCommand(trial string, call replay.Call) core.Command {
	cmd := core.Command{Path: "/bin/bash", Args: []string{"-lc", call.Script}, Dir: call.Workdir}
	if !call.Login {
		cmd.Args[0] = "-c"
		if trial == "85e3b0db-f1cb-43b5-9f8a-a2333db196c1" || trial == "f0d5e4b5-b20d-40bf-89ec-6483b65525dc" {
			// Both original non-login shells explicitly printed this inherited path.
			cmd.Env = map[string]string{"PATH": "/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/codex-path:/root/.nvm/versions/node/v22.23.2/bin:/usr/local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
		}
	}
	return cmd
}

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
	intraStep := flag.Int("intra-tool-step", 0, "recorded step for experimental intra-tool tracing")
	intraInterval := flag.Duration("intra-tool-interval", time.Second, "running interval between extra trace captures")
	intraWindows := flag.Int("intra-tool-windows", 3, "maximum extra captures for the selected tool")
	hfSourceCPU := flag.Int("hf-host-cpu", -1, "source-qualified HF one-host-core capacity with two guest-visible CPUs")
	hfHostCPU := flag.Int("diagnostic-hf-host-cpu", -1, "verify two HF guest vCPUs on the selected single host CPU in an isolated diagnostic deployment")
	hfHarness := flag.String("hf-harness", "", "directory with the approved Debian utility and curated marketplace archives for the two HF inspection trials")
	grpcTools := flag.String("grpc-tools", "", "signed, date-matched Debian git/patch package bundle for the 69ba source")
	codexTools := flag.String("codex-tools", "", "pinned original Codex and Node guest prefix archive")
	opamPID := flag.Bool("diagnostic-opam-pid", false, "bind the recorded compiler signal-zero check and progress reads to the observed guest installer")
	opamLog := flag.Bool("diagnostic-opam-log", false, "bind the original compiler progress-log name to its observed OPAM file in a diagnostic")
	ipv4Addrs := flag.Bool("diagnostic-grpc-ipv4-addresses-only", false, "require the isolated 69ba guest to have IPv4 addresses only with its IPv6 stack enabled")
	routeOnly := flag.Bool("diagnostic-grpc-network-only", false, "collect the prepared and restored 69ba guest network readbacks without dispatching a recording")
	grpcNetDiag := flag.Bool("diagnostic-grpc-loopback", false, "verify the private 69ba guest with its IPv6 loopback address omitted")
	groupMode := flag.Bool("codex-process-groups", false, "use original Codex Linux pipe process-group lifetime for reviewed Python guest workloads")
	processDrift := flag.Bool("diagnostic-process-drift", false, "continue through observed process timing drift in an uninstrumented reviewed trial")
	videoDifferences := flag.Bool("diagnostic-video-images", false, "record live video image differences and continue an uninstrumented diagnostic")
	imageHelper := flag.String("image-helper", "", "pinned static in-guest PNG/JPEG codec")
	curlPackages := flag.String("curl-packages", "", "pinned Bookworm curl archive for the recorded HTTP client")
	osTools := flag.String("os-tools", "", "pinned task-specific offline package archive")
	rg := flag.String("ripgrep", "", "Codex 0.151.0 Linux x64 bundled rg for guest preparation")
	probe := flag.String("guest-probe", "", "optional guest telemetry executable for diagnostic runs")
	sampler := flag.String("sampler", "", "host sample_vmm.py")
	samples := flag.String("samples", "", "new one-second VMM samples JSONL")
	log := flag.String("log", "", "new private JSONL run log")
	storage := flag.Int("storage-mib", 0, "devmapper disk capacity")
	var o vhive.Options
	o.IdleMode = "checkpoint"
	flag.BoolVar(&o.GuestTaskV3, "guest-task-v3", true, "containerd-v2 stack with guest task v3 and framed streams")
	flag.BoolVar(&o.NativeCheckpoint, "native-checkpoint", false, "optimized native checkpoint and restore API")
	flag.BoolVar(&o.NativeReportingOnly, "native-reporting-only", false, "zero-target free-page reporting on the native DAMON-disabled control")
	flag.StringVar(&o.CNINetwork, "cni-network", "", "task-specific guest network")
	flag.StringVar(&o.CNIConfDir, "cni-conf-dir", "", "task-specific CNI configuration directory")
	flag.StringVar(&o.CNICacheDir, "cni-cache-dir", "", "task-specific CNI cache directory")
	cniBin := flag.String("cni-bin-dir", "", "task-specific CNI binary directory")
	flag.StringVar(&o.Socket, "socket", "", "containerd socket")
	flag.StringVar(&o.Namespace, "namespace", "aries", "containerd namespace")
	flag.StringVar(&o.CheckpointDir, "checkpoint-dir", "", "private host checkpoint directory")
	flag.StringVar(&o.ShimDir, "shim-dir", "", "shim metadata directory")
	flag.StringVar(&o.IODir, "io-dir", "", "runtime FIFO directory")
	flag.IntVar(&o.VCPUs, "vcpus", 0, "guest CPUs")
	flag.IntVar(&o.MemoryMB, "memory-mib", 0, "guest memory")
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
	sourceCPU := *hfSourceCPU >= 0
	if sourceCPU {
		if *hfHostCPU >= 0 || *processDrift {
			return errors.New("source-qualified HF capacity requires strict timing and a single CPU profile")
		}
		*hfHostCPU = *hfSourceCPU
	}
	requestedVCPUs := r.VCPUs
	if *hfHostCPU >= 0 {
		inspection := r.Trial == hfInspectionTrial && *hfHarness != ""
		defaultThreads := r.Trial == hfDefaultThreadsTrial && *hfHarness == ""
		o.GuestSMT = sourceCPU && defaultThreads
		if (!inspection && !defaultThreads) || (!sourceCPU && (*observer != "" || *sampler != "")) {
			return errors.New("two-vCPU diagnostic requires the uninstrumented HF inspection source and fixture")
		}
		requestedVCPUs = 2
	}
	if o.VCPUs == 0 {
		o.VCPUs = requestedVCPUs
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = r.MemoryMiB
	}
	if *storage == 0 {
		*storage = r.StorageMiB
	}
	if o.VCPUs != requestedVCPUs || o.MemoryMB != r.MemoryMiB || *storage != r.StorageMiB {
		return fmt.Errorf("trial %s requires %d vCPUs, %d MiB memory and %d MiB storage", r.Trial, requestedVCPUs, r.MemoryMiB, r.StorageMiB)
	}
	// Both sources explicitly exercise nohup and a separate-session replacement.
	// Every original shell in these trials receives the same backend.
	if *hfHarness != "" && (r.Task != hfTask || (r.Trial != hfLoopbackTrial && r.Trial != hfInspectionTrial)) {
		return errors.New("HF inspection fixture requires its reviewed source")
	}
	useGroups := *groupMode || *hfHarness != "" || r.Trial == hfLoopbackTrial || r.Trial == grpcLoopbackTrial
	if (r.Trial == grpcLoopbackTrial && !*grpcNetDiag) != (*grpcTools != "") {
		return errors.New("normal 69ba replay must provide its signed -grpc-tools fixture")
	}
	if (*routeOnly || *ipv4Addrs) && !*grpcNetDiag {
		return errors.New("69ba network-only readback requires -diagnostic-grpc-loopback")
	}
	if *grpcNetDiag && (r.Trial != grpcLoopbackTrial || *observer != "" || *processDrift) {
		return errors.New("guest loopback calibration requires an uninstrumented 69ba replay with strict process checks")
	}
	if useGroups && r.Task != hfTask && r.Task != kvTask {
		return errors.New("Codex process-group helper requires a reviewed HF or gRPC Python guest")
	}
	if *opamPID && (!*processDrift || r.Trial != compilerWaitTrial) {
		return errors.New("OPAM installer identity mapping requires the uninstrumented 921 process diagnostic")
	}
	if *opamLog && (!*processDrift || r.Trial != compilerLogTrial) {
		return errors.New("OPAM log identity mapping requires the uninstrumented BF3 process diagnostic")
	}
	if *processDrift {
		torchPoll := r.Task == torchTask && (r.Trial == "e047ddea-bb8c-43c0-b71e-e625a611565f" || r.Trial == "ce62b3a5-602c-44c7-b13e-6537dca35d1d")
		grpcPoll := r.Task == kvTask && r.Trial == grpcLoopbackTrial
		financialPoll := r.Task == financialTask && r.Trial == "dce59078-0f61-4eac-92f9-3b2728e4ef28"
		hfPoll := r.Task == hfTask && (r.Trial == hfLoopbackTrial || r.Trial == hfInspectionTrial || r.Trial == hfGunicornLoopbackTrial)
		allowed := grpcPoll || financialPoll || hfPoll || torchPoll || r.Task == "compile-compcert" || r.Task == "video-processing" || r.Task == "circuit-fibsqrt" || r.Task == datasetTask
		if !allowed || *observer != "" || *sampler != "" || *probe != "" || *intraStep != 0 {
			return errors.New("process drift diagnostics require a reviewed uninstrumented recording")
		}
	}
	if *videoDifferences && (r.Task != "video-processing" || *observer != "" || *sampler != "" || *probe != "" || *intraStep != 0) {
		return errors.New("video image diagnostics require a reviewed uninstrumented video recording")
	}
	if *image != r.Image {
		return fmt.Errorf("trial %s requires image %s", r.Trial, r.Image)
	}
	if r.NeedsNetwork != (o.CNINetwork != "") {
		return fmt.Errorf("source %s requires guest network enabled=%t", r.Trial, r.NeedsNetwork)
	}
	if *cniBin != "" {
		o.CNIBinPaths = []string{*cniBin}
	}
	needsImage := false
	for _, c := range r.Plan.Calls {
		if c.Operation == "ViewImage" {
			needsImage = true
			break
		}
	}
	if needsImage && *imageHelper == "" {
		return errors.New("this task requires the pinned in-guest -image-helper")
	}
	readySetup, _ := sourceSetup(r)
	if ((r.Trial == hfLoopbackTrial && *hfHarness == "") || r.Trial == hfDefaultThreadsTrial || r.Trial == datasetPythonTrial) != (*curlPackages != "") {
		return errors.New("provide the pinned curl archive for the source trial requiring the additional HTTP client")
	}
	if r.Task != "fix-git" && (*codexTools == "" || (readySetup.packagesSHA != "" && *osTools == "")) {
		return errors.New("missing pinned guest tool bundle for this task")
	}
	if (*sampler == "") != (*samples == "") {
		return errors.New("provide -sampler and -samples together")
	}
	var intraTool *harbor.IntraToolOptions
	if *intraStep != 0 {
		if *observer == "" {
			return errors.New("intra-tool tracing requires an observer")
		}
		intraTool = &harbor.IntraToolOptions{Step: *intraStep, Interval: *intraInterval, Windows: *intraWindows}
		if err := intraTool.Validate(r); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(*log, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return native(ctx, o, r, *image, *observer, *sampler, *samples, *rg, *probe, *codexTools, *osTools, *curlPackages, *grpcTools, *hfHarness, *imageHelper, *videoDifferences, *processDrift, *opamLog, *opamPID, useGroups, *grpcNetDiag, *routeOnly, *ipv4Addrs, *storage, *hfHostCPU, sourceCPU, intraTool, json.NewEncoder(f))
}

func native(ctx context.Context, o vhive.Options, r *harbor.Recording, image, observer, sampler, samples, rg, probe, codexTools, osTools, curlPackages, grpcTools, hfHarness, imageHelper string, videoDifferences, processDrift, opamLog, opamPID, groupMode, grpcNetDiag, routeOnly, ipv4Addrs bool, storage, hfHostCPU int, sourceCPU bool, intraTool *harbor.IntraToolOptions, log *json.Encoder) (retErr error) {
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
		cpuProfile := map[string]any{}
		if hfHostCPU >= 0 {
			cpuProfile["host_cpu"] = hfHostCPU
			cpuProfile["guest_vcpus"] = o.VCPUs
			cpuProfile["requested_physical_cpus"] = r.VCPUs
			cpuProfile["source_qualified"] = sourceCPU
		}
		return log.Encode(map[string]any{"event": phase, "monotonic_ns": t, "runtime_id": name, "task": r.Task, "cni_network": o.CNINetwork, "trial_id": r.Trial, "job_id": r.Job, "row_id": r.Row, "image": image, "trajectory_sha256": r.TrajectoryHash, "rollout_sha256": r.RolloutHash, "observer": observer, "diagnostic_grpc_loopback": grpcNetDiag, "diagnostic_grpc_ipv4_addresses_only": ipv4Addrs, "diagnostic_video_images": videoDifferences, "diagnostic_process_drift": processDrift, "diagnostic_opam_log": opamLog, "diagnostic_opam_pid": opamPID, "memory_mib": o.MemoryMB, "storage_mib": storage, "vcpus": o.VCPUs, "hf_cpu_profile": cpuProfile, "source_requested_cpus": r.VCPUs})
	}
	if err = lifecycle("create_start"); err != nil {
		return err
	}
	req := deployment.Request{Name: name, AllowNetwork: r.NeedsNetwork, Image: image, StorageMB: storage, Workdir: r.Workdir, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.component": "sandbox", "aries.kind": "task-container", "aries.run": name, "aries.task": r.Task}}
	req.PrivateHostsFile = r.Task == hfTask && hfResolvesHostnameToLoopback(r.Trial)
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
		{"aries-harbor-image", imageHelper, "b06faaf4f96c04e41d6808ee8e38317ea1fd19c68b20693b2045c8fe7e712bc9"},
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
	var groups *shellGroups
	if groupMode {
		groups, err = newShellGroups()
		if err != nil {
			return err
		}
		guest := func(ctx context.Context, command core.Command) (core.CommandResult, error) {
			return m.Exec(ctx, id, command)
		}
		sum, err := installCodexGroups(ctx, guest)
		if err != nil {
			return err
		}
		defer func() {
			retErr = errors.Join(retErr, log.Encode(map[string]any{"event": "codex_process_group_releases", "releases": groups.timings}))
		}()
		if err := log.Encode(map[string]any{"event": "guest_prepared", "helper": "codex_process_groups", "guest_sha256": sum, "source_version": "0.151.0"}); err != nil {
			return err
		}
	}
	prepBegin, err := harbor.Clock()
	if err != nil {
		return err
	}
	if r.Task == datasetTask {
		if err = prepareDatasetEndpoints(ctx, m, id, o.CNIConfDir); err != nil {
			return err
		}
	}
	event, state, err := prepareTask(ctx, m, id, r, codexTools, osTools, curlPackages, grpcTools, hfHarness)
	prepEnd, clockErr := harbor.Clock()
	if err != nil || clockErr != nil {
		return errors.Join(err, clockErr)
	}
	if err = log.Encode(map[string]any{"event": event, "begin_monotonic_ns": prepBegin, "end_monotonic_ns": prepEnd, "state": state}); err != nil {
		return err
	}
	if r.NeedsNetwork {
		if grpcNetDiag || grpcTools != "" {
			guest := func(ctx context.Context, command core.Command) (core.CommandResult, error) {
				return m.Exec(ctx, id, command)
			}
			if err := readGRPCLoopback(ctx, guest, "before_snapshot", log, ipv4Addrs || grpcTools != "", grpcNetDiag); err != nil {
				return err
			}
		}
		if err = networkEvidence(ctx, m, id, "before_snapshot", r, log); err != nil {
			return err
		}
	}
	if hfHostCPU >= 0 {
		if err := checkHFCPU(ctx, m, id, r.Trial, hfHostCPU, sourceCPU, "before_snapshot", log); err != nil {
			return err
		}
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
	if r.NeedsNetwork {
		if grpcNetDiag || grpcTools != "" {
			guest := func(ctx context.Context, command core.Command) (core.CommandResult, error) {
				return m.Exec(ctx, id, command)
			}
			if err := readGRPCLoopback(ctx, guest, "after_restore", log, ipv4Addrs || grpcTools != "", grpcNetDiag); err != nil {
				return err
			}
		}
		if err = networkEvidence(ctx, m, id, "after_restore", r, log); err != nil {
			return err
		}
	}
	if hfHostCPU >= 0 {
		if err := checkHFCPU(ctx, m, id, r.Trial, hfHostCPU, sourceCPU, "after_restore", log); err != nil {
			return err
		}
	}
	if routeOnly {
		return log.Encode(map[string]any{"event": "diagnostic_network_only_complete", "source_tools_dispatched": 0})
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
	sessions := harbor.NewProcessSessions(ctx, func(ctx context.Context, call replay.Call, stdout, stderr io.Writer) (core.CommandResult, error) {
		return m.ExecStream(ctx, id, groups.start(sourceShellCommand(r.Trial, call), call), nil, stdout, stderr)
	})
	defer sessions.Close()
	h := harbor.Hooks{
		DiagnosticVideoImages: videoDifferences,
		IntraTool:             intraTool,
		Record:                func(e harbor.Receipt) error { return log.Encode(e) },
		Exec: func(ctx context.Context, call replay.Call) (core.CommandResult, error) {
			if call.Operation == "StartProcess" || call.Operation == "PollProcess" {
				if processDrift {
					return sessions.ObserveDrift(ctx, call)
				}
				return sessions.Execute(ctx, call)
			}
			if call.Operation == "ApplyPatch" {
				const binary = "/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex"
				return m.Exec(ctx, id, core.Command{Path: binary, Args: []string{"--codex-run-as-apply-patch", string(call.Body)}, Dir: call.Workdir})
			}
			if call.Operation == "ViewImage" {
				return m.Exec(ctx, id, core.Command{Path: "/usr/local/bin/aries-harbor-image", Args: []string{call.Path, fmt.Sprint(call.ImageWidth), fmt.Sprint(call.ImageHeight)}})
			}
			return m.Exec(ctx, id, groups.start(sourceShellCommand(r.Trial, call), call))
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
	if opamPID {
		h.Exec = mapCompilerWait(h.Exec, log.Encode)
	}
	if opamLog {
		h.Exec = mapCompilerLog(h.Exec, log.Encode)
	}
	if processDrift {
		h.FinishObservedProcess = sessions.FinishObservedProcess
		h.WasRetained = sessions.WasRetained
	}
	if groups != nil {
		guest := func(ctx context.Context, command core.Command) (core.CommandResult, error) {
			return m.Exec(ctx, id, command)
		}
		h.Exec = groups.releaseOnExit(h.Exec, guest)
		if h.FinishObservedProcess != nil {
			h.FinishObservedProcess = groups.releaseOnExit(h.FinishObservedProcess, guest)
		}
	}
	if intraTool != nil {
		if err := log.Encode(map[string]any{"event": "intra_tool_diagnostic", "step_id": intraTool.Step,
			"interval_ns": intraTool.Interval.Nanoseconds(), "maximum_captures": intraTool.Windows}); err != nil {
			return err
		}
	}
	if err := harbor.Run(ctx, r, h); err != nil {
		return err
	}
	if r.Task != "fix-git" {
		if err := captureTaskOutput(ctx, m, id, r, log, grpcTools != ""); err != nil {
			return err
		}
		if hfHostCPU >= 0 {
			return checkHFCPU(ctx, m, id, r.Trial, hfHostCPU, sourceCPU, "after_source", log)
		}
	}
	return nil
}
