package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"google.golang.org/grpc"
)

type ServeOptions struct {
	Config    LaunchConfig
	Backend   target.Backend
	NewNative func(string, string) (NativeServer, error)
}

// Serve owns the run service's independent native sandbox access.
func Serve(ctx context.Context, options ServeOptions) error { return serve(ctx, options, nil) }
func serve(ctx context.Context, options ServeOptions, listener net.Listener) error {
	transport := transportOf(options.Config.BridgeType)
	if options.Config.RunID == "" || options.Backend == nil || options.NewNative == nil || transport == "" {
		if listener != nil {
			listener.Close()
		}
		return errors.New("bridge requires run identity, backend, native factory and transport")
	}
	service, err := control.NewServer(control.Config{NewSandbox: func(request *v1.RegisterSandboxRequest) control.Sandbox {
		return &nativeSandbox{options: options, request: request, transport: transport, root: filepath.Join(options.Config.OutputDir, request.SandboxId)}
	}})
	if err != nil {
		return err
	}
	if listener == nil {
		listener, err = net.Listen("tcp", options.Config.ControlAddress)
		if err != nil {
			return err
		}
	}
	defer listener.Close()
	server := grpc.NewServer()
	service.Register(server)
	defer server.Stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-done:
		serveErr = fmt.Errorf("bridge control serving stopped: %w", err)
	}
	cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return errors.Join(serveErr, service.Close(cleanup))
}

// This object belongs to one control entry; no second assignment registry exists.
type nativeSandbox struct {
	options   ServeOptions
	request   *v1.RegisterSandboxRequest
	transport string
	native    NativeServer
	borrowed  *target.Borrowed
	root      string
	ready     bool
}

func (s *nativeSandbox) Register(ctx context.Context) (*v1.Endpoint, error) {
	descriptor := control.TargetFromRegistration(s.request, s.options.Config.RunID, s.options.Config.Backend)
	var err error
	s.borrowed, err = target.New(ctx, descriptor, s.options.Backend)
	if err != nil {
		return nil, err
	}
	s.native, err = s.options.NewNative(s.request.SandboxId, s.root)
	if err != nil {
		return nil, err
	}
	endpoint, err := s.native.StartTarget(ctx, s.borrowed)
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return nil, err
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}
	s.ready = true
	return &v1.Endpoint{Port: uint32(number), User: endpoint.Username, Transport: endpoint.Protocol}, nil
}
func (s *nativeSandbox) Release(ctx context.Context) ([]*v1.Artifact, error) {
	if s.borrowed != nil {
		s.borrowed.Revoke()
	}
	if s.native != nil {
		if err := s.native.Stop(ctx); err != nil {
			return nil, err
		}
	}
	names := []string{"tool-calls.jsonl"}
	if s.options.Config.RetainRawLog {
		names = append(names, rawLogName(s.transport))
	}
	result := make([]*v1.Artifact, 0, len(names))
	for _, name := range names {
		entries, err := finalizeArtifacts(s.root, s.transport, []string{name})
		if errors.Is(err, os.ErrNotExist) && !s.ready {
			result = append(result, &v1.Artifact{Name: name, Status: "absent"})
			continue
		}
		if err != nil {
			return nil, err
		}
		a := entries[0]
		result = append(result, &v1.Artifact{Name: a.Name, Status: "complete", Size: a.Size, Sha256: a.SHA256})
	}
	// The control entry retains only the immutable request and terminal manifest.
	s.native = nil
	s.borrowed = nil
	return result, nil
}
