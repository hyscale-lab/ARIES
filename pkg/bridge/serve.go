package bridge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
)

type ServeOptions struct {
	Config    LaunchConfig
	Backend   target.Backend
	NewNative func(ssh.Signer, string, string) (NativeServer, error)
}

// Serve owns the run service's host signer and independent native sandbox access.
func Serve(ctx context.Context, options ServeOptions) error { return serve(ctx, options, nil) }
func serve(ctx context.Context, options ServeOptions, listener net.Listener) error {
	if options.Config.RunID == "" || options.Backend == nil || options.NewNative == nil {
		if listener != nil {
			listener.Close()
		}
		return errors.New("bridge requires run identity, backend and native factory")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return err
	}
	service, err := control.NewServer(control.Config{NewSandbox: func(sandboxID string) control.Sandbox {
		return &nativeSandbox{options: options, sandboxID: sandboxID, signer: signer, root: filepath.Join(options.Config.OutputDir, sandboxID)}
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
	sandboxID string
	signer    ssh.Signer
	native    NativeServer
	borrowed  *target.Borrowed
	root      string
	ready     bool
}

// Reserve opens the sandbox's native endpoint before its target exists.
func (s *nativeSandbox) Reserve(ctx context.Context) (*v1.Endpoint, error) {
	var err error
	s.native, err = s.options.NewNative(s.signer, s.sandboxID, s.root)
	if err != nil {
		return nil, err
	}
	endpoint, err := s.native.Listen(ctx)
	if err != nil {
		return nil, err
	}
	s.ready = true
	_, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return nil, err
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}
	return &v1.Endpoint{Port: uint32(number), User: endpoint.Username, Transport: endpoint.Protocol}, nil
}

// Bind validates the registered target and serves it on the reserved endpoint.
func (s *nativeSandbox) Bind(ctx context.Context, request *v1.RegisterSandboxRequest) error {
	descriptor := control.TargetFromRegistration(request, s.options.Config.RunID, s.options.Config.Backend)
	var err error
	s.borrowed, err = target.New(ctx, descriptor, s.options.Backend)
	if err != nil {
		return err
	}
	return s.native.Bind(ctx, s.borrowed)
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
		names = append(names, "ssh_raw.log")
	}
	result := make([]*v1.Artifact, 0, len(names))
	for _, name := range names {
		entries, err := finalizeArtifacts(s.root, []string{name})
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
	s.signer = nil
	return result, nil
}
