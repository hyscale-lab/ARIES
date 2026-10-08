package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	sshcredentials "github.com/hyscale-lab/aries/pkg/bridge/ssh/credentials"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type ServeOptions struct {
	Config            LaunchConfig
	Backend           target.Backend
	NewNative         func(*sshcredentials.Credentials) (NativeServer, error)
	CollectionTimeout time.Duration
}

// Serve runs the control service in the bridge child. It never obtains a Runner
// callback or ownership of the target sandbox.
func Serve(ctx context.Context, options ServeOptions) error {
	return serve(ctx, options, nil)
}

// serve owns listener when supplied; otherwise it opens the configured address.
func serve(ctx context.Context, options ServeOptions, listener net.Listener) (returnErr error) {
	defer func() {
		if listener != nil {
			listener.Close()
		}
	}()
	defer func() {
		returnErr = errors.Join(returnErr, eraseStagedCredentials("host.key", "authorized.pub", "server.key", "token"))
	}()
	config := options.Config
	read := func(name string) ([]byte, error) {
		info, err := os.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
			return nil, errors.New("invalid private bridge bootstrap file")
		}
		return os.ReadFile(name)
	}
	ca, err := read("ca.pem")
	if err != nil {
		return err
	}
	cert, err := read("server.pem")
	if err != nil {
		return err
	}
	key, err := read("server.key")
	if err != nil {
		return err
	}
	token, err := read("token")
	if err != nil {
		return err
	}
	tlsConfig, err := controlTLS(ca, cert, key, true)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var native NativeServer
	var borrowedTarget *target.Borrowed
	var artifactRoot string
	var publicKey string
	ready := false
	names := []string{"tool-calls.jsonl"}
	if config.RetainRawLog {
		names = append(names, "ssh_raw.log")
	}
	service, err := control.NewServer(control.Config{
		InstanceID: config.InstanceID, Token: string(token), MaxLease: 2 * time.Minute, OperationTimeout: time.Minute,
		Assign: func(call context.Context, request *v1.AssignSandboxRequest) (*v1.Endpoint, error) {
			mu.Lock()
			defer mu.Unlock()
			if request.CredentialId != "ssh" {
				return nil, errors.New("unknown staged SSH credentials")
			}
			descriptor := control.TargetFromProto(request.Target)
			borrowed, e := target.New(call, descriptor, options.Backend)
			if e != nil {
				return nil, e
			}
			borrowedTarget = borrowed
			hostKey, e := read("host.key")
			if e != nil {
				return nil, e
			}
			authorized, e := read("authorized.pub")
			if e != nil {
				return nil, e
			}
			keys, e := sshcredentials.ParseCredentials(hostKey, authorized)
			if e != nil {
				return nil, e
			}
			publicKey = string(ssh.MarshalAuthorizedKey(keys.HostSigner.PublicKey()))
			native, e = options.NewNative(keys)
			if e != nil {
				return nil, e
			}
			artifactRoot = filepath.Join(config.OutputDir, descriptor.TaskID, "bridge")
			endpoint, e := native.StartTarget(call, borrowed)
			if e != nil {
				return nil, e
			}
			ready = true
			host, port, e := net.SplitHostPort(endpoint.Address)
			if e != nil {
				return nil, e
			}
			number, e := strconv.Atoi(port)
			if e != nil {
				return nil, e
			}
			return &v1.Endpoint{Host: host, Port: uint32(number), User: endpoint.Username, Transport: endpoint.Protocol, HostKey: publicKey}, nil
		},
		Revoke: func(call context.Context) (artifacts []*v1.Artifact, returnErr error) {
			mu.Lock()
			defer mu.Unlock()
			defer func() {
				returnErr = errors.Join(returnErr, eraseStagedCredentials("host.key", "authorized.pub"))
			}()
			if borrowedTarget != nil {
				borrowedTarget.Revoke()
			}
			if native != nil {
				if e := native.Stop(call); e != nil {
					return nil, e
				}
			}
			if !ready {
				result := make([]*v1.Artifact, 0, len(names))
				for _, name := range names {
					if artifactRoot == "" {
						result = append(result, &v1.Artifact{Name: name, Status: "absent"})
						continue
					}
					entries, e := finalizeArtifacts(artifactRoot, []string{name})
					if errors.Is(e, os.ErrNotExist) {
						result = append(result, &v1.Artifact{Name: name, Status: "absent"})
						continue
					}
					if e != nil {
						return nil, e
					}
					a := entries[0]
					result = append(result, &v1.Artifact{Name: a.Name, Status: "complete", Size: a.Size, Sha256: a.SHA256})
				}
				return result, nil
			}
			finalized, e := finalizeArtifacts(artifactRoot, names)
			if e != nil {
				return nil, e
			}
			result := make([]*v1.Artifact, 0, len(finalized))
			for _, a := range finalized {
				result = append(result, &v1.Artifact{Name: a.Name, Status: "complete", Size: a.Size, Sha256: a.SHA256})
			}
			return result, nil
		},
	})
	if err != nil {
		return err
	}
	if listener == nil {
		listener, err = net.Listen("tcp", config.ControlAddress)
		if err != nil {
			return err
		}
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.UnaryInterceptor(service.UnaryInterceptor), grpc.StreamInterceptor(service.StreamInterceptor))
	service.Register(server)
	defer server.Stop()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	// Admission itself is bounded even when a controller dies before assignment.
	bootstrap := time.NewTimer(2 * time.Minute)
	defer bootstrap.Stop()
	select {
	case <-ctx.Done():
		service.Revoke()
	case <-service.Revoking():
	case <-bootstrap.C:
		active := service.HasAssignment()
		if active {
			select {
			case <-ctx.Done():
				service.Revoke()
			case <-service.Revoking():
			case e := <-serveDone:
				return e
			}
		} else {
			service.Revoke()
		}
	case e := <-serveDone:
		return fmt.Errorf("bridge control serving stopped: %w", e)
	}
	// Failed native cleanup remains visible through the bounded collection period.
	timeout := options.CollectionTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return awaitCollectionExit(service, timeout, serveDone)
}

func eraseStagedCredentials(names ...string) error {
	var failures []error
	for _, name := range names {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("erase bridge credential %s: %w", name, err))
		}
	}
	return errors.Join(failures...)
}

func awaitCollectionExit(service *control.Server, timeout time.Duration, serveDone <-chan error) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		select {
		case <-service.Done():
			return nil
		default:
			if service.HasAssignment() {
				return errors.New("bridge exit without confirmed native revocation")
			}
			return nil
		}
	case e := <-serveDone:
		return e
	}
}
