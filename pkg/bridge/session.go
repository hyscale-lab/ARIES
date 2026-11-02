package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

type targetExporter interface {
	ExportBridgeTarget() (core.BridgeTarget, error)
}

// Session is one sandbox's immutable bridge access and evidence owner.
type Session struct {
	mu                                       sync.Mutex
	service                                  *Service
	outputRoot                               string
	started, stopped                         bool
	descriptor                               core.BridgeTarget
	local, remote, helper                    string
	registered, exposed, released, collected bool
	// reserved marks a sandbox reserved before it started, whose endpoint
	// Start keeps.
	reserved     bool
	reservedPort uint32
	endpoint     core.ToolEndpoint
	manifest     []*v1.Artifact
	evidenceErr  error
}

var _ runner.ToolBridge = (*Session)(nil)

func (s *Session) record() error {
	b, err := json.MarshalIndent(struct{ Backend, RuntimeID, RuntimeName, SandboxID, TargetRuntimeID string }{s.service.options.Launch.RuntimeBackend, s.service.runtimeID, s.service.runtimeName, s.descriptor.SandboxID, s.descriptor.RuntimeID}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.local, "runtime.json"), b, 0600)
}

// Reserve opens the sandbox's access before the sandbox has started, so the
// harness can be given its endpoint while the sandbox starts. Start then binds
// the started sandbox to the same endpoint.
func (s *Session) Reserve(ctx context.Context, sandbox runner.Sandbox) (core.ToolEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped || s.reserved {
		return core.ToolEndpoint{}, errors.New("bridge session cannot be reused")
	}
	s.reserved = true
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	d, err := s.claim(sandbox, target.ValidateReservation)
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	s.registered = true // Retain cleanup ownership even if the response is lost.
	access, err := s.admit(ctx, func(ctx context.Context) (*v1.SandboxAccess, error) {
		return s.service.client.ReserveSandbox(ctx, &v1.ReserveSandboxRequest{SandboxId: d.SandboxID, Metadata: &v1.Metadata{TaskId: d.TaskID}})
	})
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	if access.State != v1.State_RESERVED || access.Endpoint == nil {
		return core.ToolEndpoint{}, fmt.Errorf("bridge sandbox reservation failed: state %s: %s", access.State, access.Error)
	}
	s.endpoint, err = s.expose(ctx, access.Endpoint, d.Workdir)
	return s.endpoint, err
}

// Start binds a reserved sandbox now that it has started, or registers it in
// one step when it was not reserved.
func (s *Session) Start(ctx context.Context, sandbox runner.Sandbox) (core.ToolEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return core.ToolEndpoint{}, errors.New("bridge session cannot be reused")
	}
	s.started = true
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	reserved := s.descriptor
	d, err := s.claim(sandbox, target.Validate)
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	if s.reserved && (d.SandboxID != reserved.SandboxID || d.TaskID != reserved.TaskID || d.Workdir != reserved.Workdir || d.ExecUser != reserved.ExecUser) {
		return core.ToolEndpoint{}, errors.New("started sandbox differs from its reservation")
	}
	if err = s.record(); err != nil {
		return core.ToolEndpoint{}, err
	}
	s.registered = true // Retain cleanup ownership even if the response is lost.
	access, err := s.registerSandbox(ctx, control.Registration(d))
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	if access.State != v1.State_READY || access.Endpoint == nil {
		return core.ToolEndpoint{}, fmt.Errorf("bridge sandbox registration failed: state %s: %s", access.State, access.Error)
	}
	if s.reserved {
		if access.Endpoint.Port != s.reservedPort {
			return core.ToolEndpoint{}, errors.New("bridge bound the sandbox to a different endpoint than it reserved")
		}
		return s.endpoint, nil
	}
	return s.expose(ctx, access.Endpoint, d.Workdir)
}

// claim exports the sandbox's bridge target, checks it against this run's
// service and records where its evidence goes.
func (s *Session) claim(sandbox runner.Sandbox, validate func(core.BridgeTarget) error) (core.BridgeTarget, error) {
	s.service.mu.Lock()
	running := s.service.state == "running"
	s.service.mu.Unlock()
	if !running {
		return core.BridgeTarget{}, errors.New("bridge service is not running")
	}
	exporter, ok := sandbox.(targetExporter)
	if !ok {
		return core.BridgeTarget{}, errors.New("sandbox cannot export a bridge target")
	}
	d, err := exporter.ExportBridgeTarget()
	if err != nil {
		return core.BridgeTarget{}, err
	}
	if err = validate(d); err != nil {
		return core.BridgeTarget{}, err
	}
	if d.Backend != s.service.options.Launch.Config.Backend || d.RunID != s.service.options.RunID {
		return core.BridgeTarget{}, errors.New("sandbox backend or run does not match bridge service")
	}
	s.descriptor = d
	s.local = filepath.Join(s.outputRoot, d.TaskID, "bridge")
	s.remote = filepath.Join(s.service.options.Launch.Config.OutputDir, d.SandboxID)
	if err = os.MkdirAll(s.local, 0700); err != nil {
		return core.BridgeTarget{}, err
	}
	return d, nil
}

// expose turns the bridge's native endpoint into the harness's: the bridge
// runtime's address at that port, and the staged client helper.
func (s *Session) expose(ctx context.Context, native *v1.Endpoint, workdir string) (core.ToolEndpoint, error) {
	if native.Transport != "ssh" || native.User != "aries" || native.Port == 0 || native.Port > 65535 {
		return core.ToolEndpoint{}, errors.New("invalid native bridge endpoint")
	}
	// Binding is reported by the child; deployment resolves the consumer address.
	address, err := s.service.options.Runtime.(deployment.ServiceRuntime).TaskAddress(ctx, s.service.runtimeID, int(native.Port))
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return core.ToolEndpoint{}, errors.New("invalid resolved native endpoint")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return core.ToolEndpoint{}, errors.New("invalid resolved native endpoint port")
	}
	s.reservedPort = native.Port
	endpoint := core.ToolEndpoint{Protocol: "ssh", Address: address, Username: native.User, Workdir: workdir, LogPaths: []string{filepath.Join(s.local, "tool-calls.jsonl")}}
	if s.service.options.RetainRawLog {
		endpoint.LogPaths = append(endpoint.LogPaths, filepath.Join(s.local, "ssh_raw.log"))
	}
	client := s.service.options.Client
	if client.SourcePath != "" {
		endpoint.ClientCommand = client.Command
		endpoint.ClientSourceFile = filepath.Join(s.local, filepath.Base(client.Command))
		s.helper = endpoint.ClientSourceFile
		if err = stageClientHelper(client.SourcePath, s.helper); err != nil {
			return core.ToolEndpoint{}, err
		}
	}
	s.exposed = true
	return endpoint, nil
}
func (s *Session) request() *v1.SandboxRequest {
	return &v1.SandboxRequest{SandboxId: s.descriptor.SandboxID}
}
func (s *Session) validateAccess(a *v1.SandboxAccess) error {
	if a == nil || a.SandboxId != s.descriptor.SandboxID {
		return errors.New("bridge sandbox identity differs")
	}
	return nil
}
func (s *Session) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.evidenceErr != nil {
		return errors.Join(s.evidenceErr, s.removeHelper())
	}
	if s.registered && !s.released {
		a, err := s.service.client.ReleaseSandbox(ctx, s.request())
		if err != nil {
			running, runErr := s.service.options.Runtime.Running(ctx, s.service.runtimeID)
			if runErr != nil || running {
				return errors.Join(fmt.Errorf("bridge release unconfirmed: %w", err), runErr)
			}
			s.released = true
			if s.exposed {
				s.evidenceErr = errors.New("bridge exited without finalized evidence")
				return errors.Join(s.evidenceErr, s.removeHelper())
			}
			s.collected = true
		} else {
			if err = s.validateAccess(a); err != nil {
				return err
			}
			if a.State != v1.State_RELEASED || a.CleanupError != "" {
				return fmt.Errorf("bridge access closure or evidence finalization unconfirmed (state %s): %s", a.State, a.CleanupError)
			}
			s.released = true
			s.manifest = a.Artifacts
			if !s.exposed && len(a.Artifacts) == 0 {
				s.collected = true
			}
		}
	}
	if s.released && !s.collected {
		expected := map[string]bool{"tool-calls.jsonl": false}
		if s.service.options.RetainRawLog {
			expected["ssh_raw.log"] = false
		}
		for _, entry := range s.manifest {
			seen, ok := expected[entry.Name]
			if !ok || seen {
				return errors.New("bridge evidence manifest is incomplete or unexpected")
			}
			if entry.Status == "absent" && !s.exposed && entry.Size == 0 && entry.Sha256 == "" {
				expected[entry.Name] = true
				continue
			}
			if entry.Status != "complete" {
				return errors.New("bridge evidence manifest is incomplete or unexpected")
			}
			if err := collectArtifact(ctx, s.service.options.Runtime, s.service.runtimeID, s.remote, s.local, Artifact{Name: entry.Name, Size: entry.Size, SHA256: entry.Sha256}); err != nil {
				return err
			}
			expected[entry.Name] = true
		}
		for _, seen := range expected {
			if !seen {
				return errors.New("bridge evidence missing from finalized manifest")
			}
		}
		s.collected = true
	}
	return s.removeHelper()
}
func (s *Session) removeHelper() error {
	if s.helper != "" {
		if err := os.Remove(s.helper); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
func stageClientHelper(source, destination string) error {
	const maxHelperBytes int64 = 64 << 20
	before, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0111 == 0 || before.Size() <= 0 || before.Size() > maxHelperBytes {
		return errors.New("bridge helper must be a bounded regular executable")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	content, readErr := io.ReadAll(io.LimitReader(input, maxHelperBytes+1))
	closeErr := input.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		return err
	}
	after, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != int64(len(content)) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("bridge helper source changed while staging")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := output.Write(content)
	modeErr := output.Chmod(0555)
	return errors.Join(writeErr, modeErr, output.Close())
}
