package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

type Options struct {
	Runtime                      deployment.Runtime
	Launch                       LaunchSpec
	Client                       ClientConfig
	OutputDir, BridgeType, RunID string
	Placement                    core.RuntimePlacement
	RetainRawLog                 bool
}

// ClientConfig is the optional native client helper supplied by composition.
type ClientConfig struct{ Command, SourcePath string }

// Service owns one deployed bridge and its connection for the entire run.
// Sessions own sandbox access and evidence; they never remove this runtime.
type Service struct {
	mu                         sync.Mutex
	options                    Options
	state                      string
	runtimeID, runtimeName     string
	connection                 *grpc.ClientConn
	client                     v1.BridgeControlClient
	sessions                   []*Session
	removed, runtimeClosed     bool
	allocationErr, shutdownErr error
}

func NewService(options Options) (*Service, error) {
	if options.Runtime == nil || options.OutputDir == "" || options.RunID == "" {
		return nil, errors.New("bridge service requires runtime, run identity and output directory")
	}
	if _, ok := options.Runtime.(deployment.ServiceRuntime); !ok {
		return nil, errors.New("bridge runtime requires harness addressing")
	}
	launch := options.Launch
	if launch.RuntimeBackend == "" || launch.ResourceMetrics == "" || launch.Config.Backend == "" {
		return nil, errors.New("bridge requires explicit runtime metadata and execution backend")
	}
	if launch.Request.Workdir == "" || launch.Config.OutputDir == "" || launch.Config.ControlAddress == "" || launch.Request.ServicePort <= 0 || launch.Request.ServicePort > 65535 {
		return nil, errors.New("bridge requires explicit staging, evidence and control addresses")
	}
	if options.BridgeType == "" {
		return nil, errors.New("bridge native type is required")
	}
	if (options.Client.Command == "") != (options.Client.SourcePath == "") {
		return nil, errors.New("bridge client helper requires source and destination")
	}
	var err error
	options.OutputDir, err = filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, err
	}
	if options.Client.SourcePath != "" {
		options.Client.SourcePath, err = filepath.Abs(options.Client.SourcePath)
		if err != nil {
			return nil, err
		}
	}
	return &Service{options: options, state: "new"}, nil
}
func nonce() (string, error) {
	var b [12]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func (s *Service) RuntimeID() string   { s.mu.Lock(); defer s.mu.Unlock(); return s.runtimeID }
func (s *Service) RuntimeName() string { s.mu.Lock(); defer s.mu.Unlock(); return s.runtimeName }
func (s *Service) record() error {
	b, err := json.MarshalIndent(struct{ Backend, RuntimeID, RuntimeName, RunID, ResourceMetrics string }{s.options.Launch.RuntimeBackend, s.runtimeID, s.runtimeName, s.options.RunID, s.options.Launch.ResourceMetrics}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.options.OutputDir, "runtime.json"), b, 0600)
}
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != "new" {
		return errors.New("bridge service cannot be restarted")
	}
	s.state = "starting"
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := os.MkdirAll(s.options.OutputDir, 0700); err != nil {
		return err
	}
	id, err := nonce()
	if err != nil {
		return err
	}
	req := s.options.Launch.Request
	req.Name = "aries-bridge-" + id
	s.runtimeName = req.Name
	req.Placement = s.options.Placement
	req.Labels = maps.Clone(req.Labels)
	if req.Labels == nil {
		req.Labels = make(map[string]string)
	}
	delete(req.Labels, "aries.task")
	delete(req.Labels, "aries.attempt")
	maps.Copy(req.Labels, map[string]string{"aries.managed": "true", "aries.component": "bridge", "aries.kind": "tool-bridge", "aries.run": s.options.RunID})
	s.runtimeID, err = s.options.Runtime.Create(ctx, req)
	if errors.Is(err, deployment.ErrAllocationUnconfirmed) {
		s.allocationErr = err
	}
	if s.runtimeID == "" && err == nil {
		s.allocationErr = fmt.Errorf("bridge runtime returned empty identity: %w", deployment.ErrAllocationUnconfirmed)
		err = s.allocationErr
	}
	if recordErr := s.record(); recordErr != nil {
		return errors.Join(err, recordErr)
	}
	if err != nil {
		return err
	}
	config := s.options.Launch.Config
	config.RunID = s.options.RunID
	config.BridgeType = s.options.BridgeType
	config.RetainRawLog = s.options.RetainRawLog
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	archive, err := archiveFiles(map[string][]byte{"config.json": data})
	if err != nil {
		return err
	}
	if err = s.options.Runtime.UploadArchive(ctx, s.runtimeID, req.Workdir, bytes.NewReader(archive)); err != nil {
		return err
	}
	if err = s.options.Runtime.Validate(ctx, s.runtimeID, req, nil); err != nil {
		return err
	}
	if err = s.options.Runtime.Start(ctx, s.runtimeID); err != nil {
		return err
	}
	for {
		address, addressErr := s.options.Runtime.Address(ctx, s.runtimeID, req.ServicePort)
		if addressErr == nil {
			s.connection, s.client, err = control.NewClient(address)
			if err != nil {
				return err
			}
			checkCtx, done := context.WithTimeout(ctx, time.Second)
			h, checkErr := healthpb.NewHealthClient(s.connection).Check(checkCtx, &healthpb.HealthCheckRequest{})
			done()
			if checkErr == nil && h.Status == healthpb.HealthCheckResponse_SERVING {
				s.state = "running"
				return nil
			}
			s.connection.Close()
			s.connection = nil
			s.client = nil
		}
		running, runErr := s.options.Runtime.Running(ctx, s.runtimeID)
		if runErr != nil || !running {
			return errors.Join(errors.New("bridge runtime exited before control readiness"), runErr)
		}
		if err = waitPoll(ctx); err != nil {
			return err
		}
	}
}
func (s *Service) NewSession(outputRoot string) (runner.ToolBridge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != "running" {
		return nil, errors.New("bridge service is not accepting sessions")
	}
	root, err := filepath.Abs(outputRoot)
	if err != nil {
		return nil, err
	}
	session := &Session{service: s, outputRoot: root}
	s.sessions = append(s.sessions, session)
	return session, nil
}

// Stop stops admission, retries every session's cleanup, then removes shared
// infrastructure. Historical failures remain reportable even after removal.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.state == "closed" {
		err := errors.Join(s.allocationErr, s.shutdownErr)
		s.mu.Unlock()
		return err
	}
	s.state = "closing"
	sessions := append([]*Session(nil), s.sessions...)
	s.mu.Unlock()
	var wg sync.WaitGroup
	failures := make(chan error, len(sessions))
	for _, session := range sessions {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- session.Stop(ctx) }()
	}
	wg.Wait()
	close(failures)
	var errs []error
	for err := range failures {
		errs = append(errs, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdownErr = errors.Join(s.shutdownErr, errors.Join(errs...))
	if s.runtimeID != "" && !s.removed {
		if err := s.options.Runtime.Stop(ctx, s.runtimeID); err != nil {
			return errors.Join(s.allocationErr, s.shutdownErr, err)
		}
		s.removed = true
	}
	if s.connection != nil {
		if err := s.connection.Close(); err != nil {
			return errors.Join(s.allocationErr, s.shutdownErr, err)
		}
		s.connection = nil
	}
	if !s.runtimeClosed {
		if err := s.options.Runtime.Close(); err != nil {
			return errors.Join(s.allocationErr, s.shutdownErr, err)
		}
		s.runtimeClosed = true
	}
	s.state = "closed"
	return errors.Join(s.allocationErr, s.shutdownErr)
}
func waitPoll(ctx context.Context) error {
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
