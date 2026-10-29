// compare-exec compares the existing Docker and vHive sandbox exec paths.
// It requires a prepared local Firecracker/containerd worker and Docker image.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/distribution/reference"
	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

type options struct {
	Socket, Namespace, IODir, CheckpointDir, ShimDir, Image, ReferenceBinary, OutputDir, Workdir string
	Samples, CheckpointSamples, NativeSamples, Warmups                                           int
}

type nativeResult struct {
	SamplesMS []float64 `json:"samples_ms"`
	Output    string    `json:"output"`
}

type environment struct {
	Name           string
	Deployment     deployment.Deployment
	Backend        target.Backend
	ExtraTransport interface{ Close() error }
	ID             string
	Bridge         *bridgessh.Manager
	Borrowed       *target.Borrowed
	Endpoint       core.ToolEndpoint
	ExpectedOutput string
	Native         nativeResult
	Kernel         string
	Ordinal        int
}

type sample struct {
	Mode         string              `json:"mode"`
	Index        int                 `json:"index"`
	Warmup       bool                `json:"warmup"`
	Sequence     uint64              `json:"sequence"`
	ClientWallMS float64             `json:"client_wall_ms"`
	ExitCode     int                 `json:"exit_code"`
	Output       string              `json:"output"`
	Stderr       string              `json:"stderr,omitempty"`
	Error        string              `json:"error,omitempty"`
	Timing       *core.CommandTiming `json:"timing,omitempty"`
}

type distribution struct {
	N      int      `json:"n"`
	Min    float64  `json:"min_ms"`
	Median float64  `json:"median_ms"`
	Max    float64  `json:"max_ms"`
	P95    *float64 `json:"p95_ms,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "compare-exec:", err)
		os.Exit(1)
	}
}

func run() (retErr error) {
	var o options
	flag.StringVar(&o.Socket, "socket", "/run/firecracker-containerd/containerd.sock", "prepared Firecracker/containerd Unix socket")
	flag.StringVar(&o.Namespace, "namespace", "aries-vhive-smoke", "prepared containerd image namespace")
	flag.StringVar(&o.IODir, "io-dir", "/run/firecracker-containerd/aries-io", "shared host exec FIFO directory")
	flag.StringVar(&o.CheckpointDir, "checkpoint-dir", "/var/lib/firecracker-containerd/aries-checkpoints", "disk checkpoint directory")
	flag.StringVar(&o.ShimDir, "shim-dir", "/var/lib/firecracker-containerd/shim-base", "worker shim_base_dir")
	flag.StringVar(&o.Image, "image", "docker.io/alexgshaw/fix-git:20260403", "same prepared image in Docker and containerd")
	flag.StringVar(&o.ReferenceBinary, "reference-binary", "", "static exec-reference helper binary to upload")
	flag.StringVar(&o.OutputDir, "output-dir", "", "new private evidence directory (required)")
	flag.StringVar(&o.Workdir, "workdir", "/app/personal-site", "existing sandbox workdir")
	flag.IntVar(&o.Samples, "samples", 30, "measured Docker and persistent VM samples each")
	flag.IntVar(&o.CheckpointSamples, "checkpoint-samples", 5, "measured checkpoint VM samples")
	flag.IntVar(&o.NativeSamples, "native-samples", 50, "local child-exec reference samples per environment")
	flag.IntVar(&o.Warmups, "warmups", 2, "excluded warmup commands per environment and reference loop")
	flag.Parse()
	if flag.NArg() != 0 || o.OutputDir == "" || o.ReferenceBinary == "" || o.Samples < 1 || o.CheckpointSamples < 1 || o.NativeSamples < 1 || o.Warmups < 0 {
		return errors.New("output-dir and reference-binary are required; sample counts must be positive and warmups nonnegative")
	}
	if !filepath.IsAbs(o.Workdir) || filepath.Clean(o.Workdir) != o.Workdir || strings.ContainsRune(o.Workdir, 0) {
		return errors.New("workdir must be a clean absolute sandbox path")
	}
	var err error
	o.OutputDir, err = filepath.Abs(o.OutputDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(o.OutputDir), 0700); err != nil {
		return err
	}
	if err = os.Mkdir(o.OutputDir, 0700); err != nil {
		return fmt.Errorf("create fresh output directory: %w", err)
	}
	referenceBytes, err := os.ReadFile(o.ReferenceBinary)
	if err != nil {
		return err
	}
	helperDigest := sha256.Sum256(referenceBytes)
	body := "cd " + quote(o.Workdir) + ` && printf "RESTORED_CALL\n"; cat /proc/sys/kernel/random/boot_id; kill -0 "$(cat /tmp/aries-checkpoint-pid)" || exit; test "$(cat /tmp/aries-checkpoint-value)" = preserved || exit; cat /tmp/aries-checkpoint-pid`
	env := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin", "HOME": "/tmp", "LANG": "C.UTF-8", "OPENCLAW_SHELL": "exec"}
	wire := canonical([]string{"env", "PATH=" + env["PATH"], "HOME=/tmp", "LANG=C.UTF-8", "OPENCLAW_SHELL=exec", "/bin/sh", "-c", body})
	runID := "compare-exec-" + time.Now().UTC().Format("20060102t150405") + "-" + fmt.Sprintf("%x", time.Now().UnixNano())
	metadata := map[string]any{"run_id": runID, "started_at": time.Now().UTC(), "options": o, "command": body, "wire_command": wire, "environment": env, "reference_binary_sha256": hex.EncodeToString(helperDigest[:]), "reference_definition": "in-sandbox monotonic time around native child /bin/sh -c launch, stdout/stderr capture and wait; excludes helper startup and outer exec transport", "bridge_placement": "host loopback SSH bridge using the existing OpenClaw dialect; identical placement for every provider", "requested_cpu": 1, "requested_memory_mb": 2048, "vhive_storage_mb": 10240, "docker_storage_mb": 0, "limitations": []string{"Docker overlayfs has no requested disk quota here; VM devmapper disk is fixed at 10 GiB", "Docker CPU quota and VM vCPU are different scheduling mechanisms", "Same image rootfs does not imply the same host/guest kernel", "Small checkpoint sample count supports median/range, not a reliable p95", "No full agent/LLM or benchmark evaluation is run"}}
	var uname unix.Utsname
	if err = unix.Uname(&uname); err == nil {
		metadata["host_kernel"] = strings.TrimRight(string(uname.Release[:]), "\x00")
	}
	hostBoot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	metadata["host_boot_id"] = strings.TrimSpace(string(hostBoot))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	var environments []*environment
	var samples []*sample
	cleanup := map[string]any{}
	journal, err := os.OpenFile(filepath.Join(o.OutputDir, "client-samples.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		// Stop bridge admission and join handlers before removing any sandbox.
		var cleanupErrors []string
		for i := len(environments) - 1; i >= 0; i-- {
			e := environments[i]
			if e.Bridge != nil {
				c, x := context.WithTimeout(context.Background(), 30*time.Second)
				stopErr := e.Bridge.Stop(c)
				x()
				if stopErr != nil {
					cleanupErrors = append(cleanupErrors, e.Name+" bridge: "+stopErr.Error())
				}
				if e.Borrowed != nil {
					e.Borrowed.Revoke()
				}
			}
		}
		if auditErr := joinAudit(o.OutputDir, environments, samples); auditErr != nil {
			retErr = errors.Join(retErr, auditErr)
		}
		for i := len(environments) - 1; i >= 0; i-- {
			e := environments[i]
			if e.ID != "" {
				c, x := context.WithTimeout(context.Background(), time.Minute)
				stopErr := e.Deployment.Stop(c, e.ID)
				x()
				cleanup[e.Name] = map[string]any{"runtime_id": e.ID, "removed": stopErr == nil}
				if stopErr != nil {
					cleanupErrors = append(cleanupErrors, e.Name+" runtime: "+stopErr.Error())
				}
			}
			if e.ExtraTransport != nil {
				if closeErr := e.ExtraTransport.Close(); closeErr != nil {
					cleanupErrors = append(cleanupErrors, e.Name+" bridge transport: "+closeErr.Error())
				}
			}
			if e.Deployment != nil {
				if closeErr := e.Deployment.Close(); closeErr != nil {
					cleanupErrors = append(cleanupErrors, e.Name+" transport: "+closeErr.Error())
				}
			}
		}
		if closeErr := journal.Close(); closeErr != nil {
			retErr = errors.Join(retErr, closeErr)
		}
		cleanup["errors"] = cleanupErrors
		cleanup["succeeded"] = len(cleanupErrors) == 0
		cleanup["completed_at"] = time.Now().UTC()
		if len(cleanupErrors) > 0 {
			retErr = errors.Join(retErr, errors.New(strings.Join(cleanupErrors, "; ")))
		}
		if writeErr := writeJSON(filepath.Join(o.OutputDir, "cleanup.json"), cleanup); writeErr != nil {
			retErr = errors.Join(retErr, writeErr)
		}
		metadata["completed_at"] = time.Now().UTC()
		if retErr != nil {
			metadata["error"] = retErr.Error()
		}
		native := map[string]nativeResult{}
		for _, e := range environments {
			native[e.Name] = e.Native
		}
		metadata["native_reference"] = native
		if writeErr := writeJSON(filepath.Join(o.OutputDir, "metadata.json"), metadata); writeErr != nil {
			retErr = errors.Join(retErr, writeErr)
		}
		if writeErr := writeSummary(o.OutputDir, environments, samples); writeErr != nil {
			retErr = errors.Join(retErr, writeErr)
		}
	}()
	if err = writeJSON(filepath.Join(o.OutputDir, "metadata.json"), metadata); err != nil {
		return err
	}
	imageIdentity, err := matchingImages(ctx, o)
	if err != nil {
		return err
	}
	metadata["image_identity"] = imageIdentity
	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	dm, err := docker.New(docker.Options{Logger: logger})
	if err != nil {
		return err
	}
	environments = append(environments, &environment{Name: "docker", Deployment: dm, Backend: dm})
	vmOptions := vhive.Options{Socket: o.Socket, Namespace: o.Namespace, IODir: o.IODir, CheckpointDir: o.CheckpointDir, ShimDir: o.ShimDir, Logger: logger}
	pm, err := vhive.New(vmOptions)
	if err != nil {
		return err
	}
	environments = append(environments, &environment{Name: "vhive-persistent", Deployment: pm, Backend: pm})
	vmOptions.IdleMode = "checkpoint"
	cm, err := vhive.New(vmOptions)
	if err != nil {
		return err
	}
	checkpoint := &environment{Name: "vhive-checkpoint", Deployment: cm}
	environments = append(environments, checkpoint)
	vmOptions.CheckpointAfterExec = true
	cb, err := vhive.New(vmOptions)
	if err != nil {
		return err
	}
	checkpoint.Backend = cb
	checkpoint.ExtraTransport = cb
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := gossh.NewSignerFromKey(privateKey)
	if err != nil {
		return err
	}
	for _, e := range environments {
		cpu, memory := float64(1), 2048
		request := deployment.Request{Name: runID + "-" + e.Name, Image: o.Image, Workdir: o.Workdir, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, CPU: &cpu, MemoryMB: &memory, Labels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": runID, "aries.task": e.Name}}
		if e.Name == "docker" {
			request.Placement.AttachmentID = "none"
		} else {
			request.StorageMB = 10240
		}
		e.ID, err = e.Deployment.Create(ctx, request)
		if err != nil {
			return fmt.Errorf("create %s: %w", e.Name, err)
		}
		if err = e.Deployment.Start(ctx, e.ID); err != nil {
			return fmt.Errorf("start %s: %w", e.Name, err)
		}
		if err = e.Deployment.Validate(ctx, e.ID, request, nil); err != nil {
			return fmt.Errorf("validate %s: %w", e.Name, err)
		}
		seed := `sleep 900 >/dev/null 2>&1 & echo $! >/tmp/aries-checkpoint-pid; echo preserved >/tmp/aries-checkpoint-value`
		if _, err = checkedExec(ctx, e, core.Command{Path: "/bin/sh", Args: []string{"-c", seed}, Dir: o.Workdir, User: "0:0", Env: env}); err != nil {
			return err
		}
		kernel, err := checkedExec(ctx, e, core.Command{Path: "/bin/uname", Args: []string{"-r"}, Dir: o.Workdir, User: "0:0", Env: env})
		if err != nil {
			return err
		}
		e.Kernel = strings.TrimSpace(kernel.Stdout)
		archive, err := helperArchive(referenceBytes)
		if err != nil {
			return err
		}
		if err = e.Deployment.UploadArchive(ctx, e.ID, "/tmp", bytes.NewReader(archive)); err != nil {
			return fmt.Errorf("upload native reference %s: %w", e.Name, err)
		}
		baseline, err := checkedExec(ctx, e, core.Command{Path: "/tmp/aries-exec-reference", Args: []string{"-command", body, "-n", fmt.Sprint(o.NativeSamples), "-warmups", fmt.Sprint(o.Warmups)}, Dir: o.Workdir, User: "0:0", Env: env, Timeout: 2 * time.Minute})
		if err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(baseline.Stdout), &e.Native); err != nil {
			return fmt.Errorf("parse native reference %s: %w", e.Name, err)
		}
		if len(e.Native.SamplesMS) != o.NativeSamples || e.Native.Output == "" {
			return fmt.Errorf("incomplete native reference for %s", e.Name)
		}
		for _, ms := range e.Native.SamplesMS {
			if ms < 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
				return errors.New("invalid native reference duration")
			}
		}
		e.ExpectedOutput = e.Native.Output
		lines := strings.Split(strings.TrimSpace(e.ExpectedOutput), "\n")
		if len(lines) != 3 || lines[0] != "RESTORED_CALL" {
			return fmt.Errorf("unexpected native diagnostic output for %s: %q", e.Name, e.ExpectedOutput)
		}
		if e.Name == "docker" && lines[1] != strings.TrimSpace(string(hostBoot)) {
			return errors.New("Docker host boot identity differs")
		}
		if e.Name != "docker" && lines[1] == strings.TrimSpace(string(hostBoot)) {
			return errors.New("VM boot identity matches host")
		}
		if e.Name == "vhive-checkpoint" {
			if err = cm.Suspend(ctx, e.ID); err != nil {
				return err
			}
		}
		descriptor := core.BridgeTarget{Backend: "docker", RuntimeID: e.ID, SandboxID: request.Name, RunID: runID, TaskID: e.Name, Workdir: o.Workdir, ExecUser: "0:0"}
		if e.Name != "docker" {
			descriptor.Backend = "vhive"
		}
		e.Borrowed, err = target.New(ctx, descriptor, e.Backend)
		if err != nil {
			return err
		}
		e.Bridge, err = bridgessh.New(bridgessh.Options{Dialect: openclaw.Dialect{}, HostSigner: signer, SandboxID: request.Name, OutputDir: filepath.Join(o.OutputDir, e.Name), Logger: logger, ResolveListen: func(context.Context) (core.BridgeListen, error) {
			return core.BridgeListen{BindHost: "127.0.0.1", BindPort: 0}, nil
		}})
		if err != nil {
			return err
		}
		e.Endpoint, err = e.Bridge.StartTarget(ctx, e.Borrowed)
		if err != nil {
			return fmt.Errorf("start bridge %s: %w", e.Name, err)
		}
		metadata[e.Name] = map[string]any{"runtime_id": e.ID, "runtime_name": request.Name, "kernel": e.Kernel, "diagnostic_output": e.ExpectedOutput, "endpoint": e.Endpoint}
		fmt.Fprintf(os.Stderr, "prepared %s (%s), native median %.3f ms\n", e.Name, e.ID, stats(e.Native.SamplesMS).Median)
	}
	measure := func(e *environment, index int, warmup bool) error {
		e.Ordinal++
		c, x := context.WithTimeout(ctx, 2*time.Minute)
		defer x()
		row := &sample{Mode: e.Name, Index: index, Warmup: warmup, Sequence: uint64(e.Ordinal)}
		row.ClientWallMS, row.ExitCode, row.Output, row.Stderr, err = sshExec(c, e.Endpoint, wire)
		if err != nil {
			row.Error = err.Error()
		} else if row.ExitCode != 0 || row.Output != e.ExpectedOutput || row.Stderr != "" {
			err = fmt.Errorf("diagnostic output/status differs for %s", e.Name)
			row.Error = err.Error()
		}
		samples = append(samples, row)
		data, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			return marshalErr
		}
		if _, writeErr := journal.Write(append(data, '\n')); writeErr != nil {
			return writeErr
		}
		fmt.Fprintf(os.Stderr, "%s sample %d warmup=%v client %.3f ms\n", e.Name, index, warmup, row.ClientWallMS)
		return err
	}
	// Alternating order reduces systematic cache/host-load drift for the fast paths.
	for round := 0; round < o.Samples+o.Warmups; round++ {
		order := []int{0, 1}
		if round%2 == 1 {
			order = []int{1, 0}
		}
		for _, index := range order {
			if err = measure(environments[index], round-o.Warmups, round < o.Warmups); err != nil {
				return err
			}
		}
	}
	for round := 0; round < o.CheckpointSamples+o.Warmups; round++ {
		if err = measure(checkpoint, round-o.Warmups, round < o.Warmups); err != nil {
			return err
		}
	}
	return nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
func canonical(argv []string) string {
	parts := make([]string, len(argv))
	for i, s := range argv {
		parts[i] = quote(s)
	}
	return strings.Join(parts, " ")
}

func matchingImages(ctx context.Context, o options) (map[string]any, error) {
	api, err := client.New(client.WithHost("unix:///var/run/docker.sock"))
	if err != nil {
		return nil, err
	}
	defer api.Close()
	d, err := api.ImageInspect(ctx, o.Image)
	if err != nil {
		return nil, fmt.Errorf("inspect prepared Docker image: %w", err)
	}
	native, err := containerd.New(o.Socket, containerd.WithDefaultNamespace(o.Namespace))
	if err != nil {
		return nil, err
	}
	defer native.Close()
	named, err := reference.ParseNormalizedNamed(o.Image)
	if err != nil {
		return nil, err
	}
	image, err := native.GetImage(ctx, reference.TagNameOnly(named).String())
	if err != nil {
		return nil, fmt.Errorf("get preimported native image: %w", err)
	}
	config, err := image.Config(ctx)
	if err != nil {
		return nil, err
	}
	nativeManifest := image.Target().Digest.String()
	verifiedBy := ""
	if d.ID == config.Digest.String() {
		verifiedBy = "image config digest"
	}
	if d.ID == nativeManifest {
		verifiedBy = "image manifest digest"
	}
	for _, digest := range d.RepoDigests {
		if _, suffix, ok := strings.Cut(digest, "@"); ok && suffix == nativeManifest {
			verifiedBy = "image manifest digest"
		}
	}
	if verifiedBy == "" {
		return nil, fmt.Errorf("prepared image identity mismatch: Docker ID=%s repo digests=%v; native manifest=%s config=%s", d.ID, d.RepoDigests, nativeManifest, config.Digest)
	}
	return map[string]any{"docker_image_id": d.ID, "containerd_image_manifest_digest": nativeManifest, "containerd_image_config_digest": config.Digest.String(), "verified_by": verifiedBy, "docker_repo_digests": d.RepoDigests, "architecture": d.Architecture, "os": d.Os}, nil
}

func helperArchive(content []byte) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "aries-exec-reference", Mode: 0755, Uid: 0, Gid: 0, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(content); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func checkedExec(ctx context.Context, e *environment, c core.Command) (core.CommandResult, error) {
	if c.Timeout == 0 {
		c.Timeout = time.Minute
	}
	r, err := e.Deployment.Exec(ctx, e.ID, c)
	if err != nil {
		return r, fmt.Errorf("%s fixture exec: %w", e.Name, err)
	}
	if r.ExitCode != 0 {
		return r, fmt.Errorf("%s fixture exited %d: %s", e.Name, r.ExitCode, r.Stderr)
	}
	return r, nil
}

func sshExec(ctx context.Context, endpoint core.ToolEndpoint, wire string) (float64, int, string, string, error) {
	// Transport connection/handshake are deliberately outside the per-command
	// wall interval. Bridge records provide the comparable server-side boundary.
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", endpoint.Address)
	if err != nil {
		return 0, -1, "", "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	cc, ch, req, err := gossh.NewClientConn(conn, endpoint.Address, &gossh.ClientConfig{User: endpoint.Username, HostKeyCallback: gossh.InsecureIgnoreHostKey()})
	if err != nil {
		return 0, -1, "", "", err
	}
	_ = conn.SetDeadline(time.Time{})
	client := gossh.NewClient(cc, ch, req)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return 0, -1, "", "", err
	}
	defer session.Close()
	var out, stderr bytes.Buffer
	session.Stdout = &out
	session.Stderr = &stderr
	session.Stdin = bytes.NewReader(nil)
	started := time.Now()
	err = session.Run(wire)
	elapsed := float64(time.Since(started)) / float64(time.Millisecond)
	code := 0
	var exit *gossh.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitStatus()
		err = nil
	} else if err != nil {
		code = -1
	}
	return elapsed, code, out.String(), stderr.String(), err
}

func joinAudit(output string, environments []*environment, samples []*sample) error {
	rows := map[string][]json.RawMessage{}
	for _, e := range environments {
		if e.Bridge == nil {
			continue
		}
		f, err := os.Open(filepath.Join(output, e.Name, "tool-calls.jsonl"))
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			rows[e.Name] = append(rows[e.Name], bytes.Clone(scanner.Bytes()))
		}
		scanErr := scanner.Err()
		closeErr := f.Close()
		if err = errors.Join(scanErr, closeErr); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(filepath.Join(output, "samples.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, s := range samples {
		if int(s.Sequence) > len(rows[s.Mode]) {
			return fmt.Errorf("missing bridge audit record %s/%d", s.Mode, s.Sequence)
		}
		var r struct {
			Sequence uint64              `json:"sequence"`
			Timing   *core.CommandTiming `json:"timing"`
			ExitCode int                 `json:"exit_code"`
			Status   string              `json:"status"`
		}
		if err = json.Unmarshal(rows[s.Mode][s.Sequence-1], &r); err != nil {
			return err
		}
		if r.Sequence != s.Sequence || r.ExitCode != s.ExitCode || r.Timing == nil {
			return fmt.Errorf("bridge audit disagrees with sample %s/%d", s.Mode, s.Sequence)
		}
		s.Timing = r.Timing
		if s.Error == "" {
			if r.Status != "completed" {
				return fmt.Errorf("bridge execution not completed: %s/%d", s.Mode, s.Sequence)
			}
			marks := r.Timing.Milestones
			if _, ok := marks["bridge.response_sent"]; !ok {
				return errors.New("bridge did not confirm response send")
			}
			if _, ok := marks["command.completed"]; !ok {
				return errors.New("provider command completion timing unavailable")
			}
			if s.Mode == "vhive-checkpoint" {
				if _, ok := marks["restore.requested"]; !ok {
					return errors.New("checkpoint sample did not restore")
				}
				if _, ok := marks["checkpoint.completed"]; !ok {
					return errors.New("checkpoint sample did not suspend")
				}
			}
			if s.Mode == "vhive-persistent" {
				for _, span := range r.Timing.Stages {
					if strings.HasPrefix(span.Name, "restore.") || strings.HasPrefix(span.Name, "checkpoint.") {
						return errors.New("persistent sample unexpectedly restored or checkpointed")
					}
				}
			}
		}
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if _, err = file.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return file.Sync()
}

func stats(values []float64) distribution {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if len(sorted) == 0 {
		return distribution{}
	}
	n := len(sorted)
	median := sorted[n/2]
	if n%2 == 0 {
		median = (sorted[n/2-1] + median) / 2
	}
	d := distribution{N: n, Min: sorted[0], Median: median, Max: sorted[n-1]}
	if n >= 20 {
		p := sorted[int(math.Ceil(.95*float64(n)))-1]
		d.P95 = &p
	}
	return d
}

func writeSummary(output string, environments []*environment, samples []*sample) error {
	summary := map[string]any{}
	for _, e := range environments {
		values := map[string][]float64{}
		for _, s := range samples {
			if s.Mode != e.Name || s.Warmup || s.Error != "" || s.Timing == nil {
				continue
			}
			values["client_command_wall"] = append(values["client_command_wall"], s.ClientWallMS)
			for _, name := range []string{"command.completed", "bridge.response_sent"} {
				if p, ok := s.Timing.Milestones[name]; ok {
					values["request_to_"+name] = append(values["request_to_"+name], p.OffsetMS)
				}
			}
			perSample := map[string]float64{}
			for _, stage := range s.Timing.Stages {
				perSample["stage."+stage.Name] += stage.DurationMS
			}
			for name, value := range perSample {
				values[name] = append(values[name], value)
			}
		}
		distributions := map[string]distribution{}
		for name, samples := range values {
			distributions[name] = stats(samples)
		}
		var nativeStats *distribution
		if len(e.Native.SamplesMS) > 0 {
			d := stats(e.Native.SamplesMS)
			nativeStats = &d
		}
		summary[e.Name] = map[string]any{"native_spawn_capture_wait": nativeStats, "metrics": distributions, "kernel": e.Kernel, "note": "Stage parent/child and concurrent spans overlap; do not sum all stage distributions. Native reference is measured inside this environment."}
	}
	return writeJSON(filepath.Join(output, "summary.json"), summary)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
