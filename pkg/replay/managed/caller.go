package managed

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"sync/atomic"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	workdir        = "/tmp"
	launchFile     = "aries-replay-launch.json"
	readyFile      = "aries-replay-ready"
	planFile       = "aries-replay-plan.json"
	planReadyFile  = "aries-replay-plan.ready"
	resultFile     = "aries-replay-result.json"
	startTimeout   = time.Minute
	pollInterval   = 50 * time.Millisecond
	maxStatusBytes = 1 << 20
)

// Options gives the caller an independent runtime and the bridge's private attachment.
type Options struct {
	Runtime         deployment.Runtime
	Image           string
	Backend         string
	BridgeBackend   string
	BridgePlacement core.RuntimePlacement
	CleanupTimeout  time.Duration
	CallTimeout     time.Duration
}

// Caller owns a single task-attached runtime. Runner owns its stop order.
type Caller struct {
	options       Options
	id            string
	started       bool
	ran           atomic.Bool
	removed       bool
	closed        bool
	allocationErr error
}

var _ runner.ReplayCaller = (*Caller)(nil)

func New(options Options) (*Caller, error) {
	if options.Runtime == nil || options.Image == "" || options.Backend == "" || options.BridgeBackend == "" || options.BridgePlacement.AttachmentID == "" {
		return nil, errors.New("replay caller requires a runtime, pinned image and bridge placement")
	}
	if options.CallTimeout < 0 {
		return nil, errors.New("replay caller timeout must not be negative")
	}
	if options.CallTimeout == 0 {
		options.CallTimeout = harness.DefaultAgentTimeout
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = startTimeout
	}
	return &Caller{options: options}, nil
}

func (caller *Caller) Start(ctx context.Context, endpoint core.ToolEndpoint, connectivity core.HarnessConnectivity, runID, taskID string) error {
	if caller.started {
		return errors.New("replay caller cannot be restarted")
	}
	caller.started = true
	if caller.options.Backend != caller.options.BridgeBackend || connectivity.Placement.AttachmentID == "" || connectivity.Placement != caller.options.BridgePlacement {
		return errors.New("replay caller and bridge require the same private task attachment and backend")
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host == "" || port == "" || endpoint.Protocol != "grpc" || runID == "" || taskID == "" {
		return errors.New("replay caller requires a private gRPC bridge endpoint and task identity")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	request := deployment.Request{
		Name: "aries-replay-" + hex.EncodeToString(nonce[:]), Image: caller.options.Image,
		Workdir: workdir, Entrypoint: []string{"/usr/local/bin/aries-bridge"}, Args: []string{"replay", path.Join(workdir, launchFile)},
		Placement: connectivity.Placement, DisableServiceAccountToken: true,
		Labels: map[string]string{"aries.managed": "true", "aries.component": "replay-caller", "aries.kind": "replay-caller", "aries.run": runID, "aries.task": taskID},
	}
	caller.id, err = caller.options.Runtime.Create(ctx, request)
	if errors.Is(err, deployment.ErrAllocationUnconfirmed) {
		caller.allocationErr = err
	}
	if caller.id == "" && err == nil {
		caller.allocationErr = deployment.ErrAllocationUnconfirmed
		return caller.allocationErr
	}
	if err != nil {
		return fmt.Errorf("create replay caller: %w", err)
	}
	launch, err := json.Marshal(childLaunch{Endpoint: endpoint.Address, CallTimeout: caller.options.CallTimeout})
	if err != nil {
		return err
	}
	if err := caller.upload(ctx, launchFile, launch); err != nil {
		return fmt.Errorf("stage replay caller bridge endpoint: %w", err)
	}
	if err := caller.options.Runtime.Validate(ctx, caller.id, request, nil); err != nil {
		return fmt.Errorf("validate replay caller: %w", err)
	}
	if err := caller.options.Runtime.Start(ctx, caller.id); err != nil {
		return fmt.Errorf("start replay caller runtime: %w", err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	for {
		if _, err := caller.download(readyCtx, readyFile); err == nil {
			return nil
		} else if !errors.Is(err, runner.ErrNotFound) {
			return fmt.Errorf("check replay caller readiness: %w", err)
		}
		if data, err := caller.download(readyCtx, resultFile); err == nil {
			return fmt.Errorf("replay caller cannot reach private bridge: %s", data)
		} else if !errors.Is(err, runner.ErrNotFound) {
			return fmt.Errorf("check replay caller startup: %w", err)
		}
		running, err := caller.options.Runtime.Running(readyCtx, caller.id)
		if err != nil || !running {
			return errors.Join(errors.New("replay caller exited before bridge readiness"), err)
		}
		if err := poll(readyCtx); err != nil {
			return fmt.Errorf("wait for replay caller readiness: %w", err)
		}
	}
}

func (caller *Caller) Run(ctx context.Context, plan replay.Plan) error {
	if caller.ran.Swap(true) {
		return errors.New("replay caller has already run")
	}
	if caller.id == "" || caller.removed {
		return errors.New("replay caller is not active")
	}
	// The validated source schedule is finite. Each call receives a bounded allowance.
	wait, err := executionTimeout(plan, caller.options.CallTimeout)
	// Allow plan acceptance and result transfer outside the child's execution budget.
	const headroom = startTimeout + startTimeout
	if err != nil || wait > time.Duration(1<<63-1)-headroom {
		return errors.New("replay schedule exceeds the supported deadline")
	}
	data, err := replay.EncodePlan(plan, replay.MaxPlanBytes)
	if err != nil {
		return err
	}
	stageCtx, cancelStage := context.WithTimeout(ctx, startTimeout)
	defer cancelStage()
	if err := caller.upload(stageCtx, planFile, data); err != nil {
		return fmt.Errorf("stage replay plan: %w", err)
	}
	if err := caller.upload(stageCtx, planReadyFile, nil); err != nil {
		return fmt.Errorf("publish replay plan: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, wait+headroom)
	defer cancel()
	for {
		if data, err := caller.download(runCtx, resultFile); err == nil {
			var result childResult
			if json.Unmarshal(data, &result) != nil || !result.Done {
				return errors.New("replay caller returned an invalid result")
			}
			if result.Error != "" {
				return errors.New(result.Error)
			}
			return nil
		} else if !errors.Is(err, runner.ErrNotFound) {
			return fmt.Errorf("read replay caller result: %w", err)
		}
		running, err := caller.options.Runtime.Running(runCtx, caller.id)
		if err != nil || !running {
			return errors.Join(errors.New("replay caller exited without a result"), err)
		}
		if err := poll(runCtx); err != nil {
			return fmt.Errorf("wait for replay caller result: %w", err)
		}
	}
}

func (caller *Caller) Stop(ctx context.Context) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, caller.options.CleanupTimeout)
	defer cancel()
	if caller.id != "" && !caller.removed {
		if err := caller.options.Runtime.Stop(cleanupCtx, caller.id); err != nil {
			return errors.Join(caller.allocationErr, fmt.Errorf("confirm replay caller removal: %w", err))
		}
		caller.removed = true
		caller.allocationErr = nil
	}
	if !caller.closed {
		if err := caller.options.Runtime.Close(); err != nil {
			return errors.Join(caller.allocationErr, err)
		}
		caller.closed = true
	}
	return caller.allocationErr
}

func (caller *Caller) upload(ctx context.Context, name string, data []byte) error {
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	if err := archive.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))}); err != nil {
		return err
	}
	if _, err := archive.Write(data); err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return caller.options.Runtime.UploadArchive(ctx, caller.id, workdir, &buffer)
}

func (caller *Caller) download(ctx context.Context, name string) ([]byte, error) {
	stream, info, err := caller.options.Runtime.DownloadArchive(ctx, caller.id, path.Join(workdir, name))
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	if !info.Mode.IsRegular() || info.Size < 0 || info.Size > maxStatusBytes || info.Mode.Perm()&0077 != 0 {
		return nil, errors.New("invalid private replay caller status")
	}
	reader := tar.NewReader(io.LimitReader(stream, maxStatusBytes+4096))
	header, err := reader.Next()
	if err != nil || header.Name != name || header.Typeflag != tar.TypeReg || header.Size != info.Size {
		return nil, errors.New("invalid replay caller status archive")
	}
	data, err := io.ReadAll(io.LimitReader(reader, info.Size+1))
	if err != nil || int64(len(data)) != info.Size {
		return nil, errors.New("invalid replay caller status length")
	}
	if _, err = reader.Next(); err != io.EOF {
		return nil, errors.New("replay caller status archive has extra files")
	}
	return data, nil
}

func poll(ctx context.Context) error {
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type childLaunch struct {
	Endpoint    string        `json:"endpoint"`
	CallTimeout time.Duration `json:"call_timeout"`
}

type childResult struct {
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}
