package openclawe2b

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	accessTokenContainerPath = "/run/aries/e2b/access.token"
	grantRollbackTimeout     = 5 * time.Second
)

type grantSandbox interface {
	runner.Sandbox
	NetworkName() string
	NetworkGateway(context.Context) (string, error)
	TaskID() string
	Workdir() string
}

// Grant is one occurrence-scoped ToolBridge backed by the shared Server.
type Grant struct {
	server    *Server
	outputDir string

	mu        sync.Mutex
	sandboxID string
	tokenPath string
	started   bool
	stopped   bool
	stopping  bool
	stopDone  chan struct{}
}

func (server *Server) NewGrant(outputDir string) *Grant {
	return &Grant{server: server, outputDir: outputDir}
}

func (grant *Grant) Start(ctx context.Context, capability runner.Sandbox) (core.ToolEndpoint, error) {
	grant.mu.Lock()
	if grant.started || grant.stopping || grant.stopped {
		grant.mu.Unlock()
		return core.ToolEndpoint{}, errors.New("OpenClaw E2B task grant already started or stopped")
	}
	grant.started = true
	grant.mu.Unlock()

	sandbox, ok := capability.(grantSandbox)
	if !ok {
		return core.ToolEndpoint{}, errors.New("OpenClaw E2B bridge requires the local Docker sandbox capability")
	}
	gatewayText, err := sandbox.NetworkGateway(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve task network gateway: %w", err)
	}
	gateway, err := netip.ParseAddr(gatewayText)
	if err != nil || !gateway.Is4() {
		return core.ToolEndpoint{}, errors.New("task network gateway must be IPv4")
	}
	sandboxID, err := randomHex(16)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("generate sandbox ID: %w", err)
	}
	token, err := randomHex(32)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("generate access token: %w", err)
	}
	tokenPath, err := grant.writeToken(sandbox.TaskID(), token)
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	registration := &registration{
		sandbox: capability, sandboxID: sandboxID, tokenHash: sha256.Sum256([]byte(token)), gateway: gateway.Unmap(),
	}
	if err := grant.server.register(registration); err != nil {
		return core.ToolEndpoint{}, errors.Join(err, removeToken(tokenPath))
	}

	grant.mu.Lock()
	grant.sandboxID = sandboxID
	grant.tokenPath = tokenPath
	grant.mu.Unlock()
	endpoint, err := grant.server.endpoint(sandboxID, gatewayText, sandbox.NetworkName(), sandbox.Workdir(), tokenPath)
	if err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grantRollbackTimeout)
		defer cancel()
		return core.ToolEndpoint{}, errors.Join(err, grant.Stop(rollbackCtx))
	}
	return endpoint, nil
}

func (server *Server) endpoint(sandboxID, gateway, network, workdir, tokenPath string) (core.ToolEndpoint, error) {
	address, err := server.Address()
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("parse shared bridge address: %w", err)
	}
	return core.ToolEndpoint{
		Protocol: "http", Address: "http://" + net.JoinHostPort(gateway, port), Network: network,
		SandboxID: sandboxID, Workdir: workdir,
		AccessTokenFile: accessTokenContainerPath, AccessTokenSourceFile: tokenPath,
	}, nil
}

func (grant *Grant) Stop(ctx context.Context) error {
	grant.mu.Lock()
	if grant.stopped {
		grant.mu.Unlock()
		return nil
	}
	if grant.stopping {
		done := grant.stopDone
		grant.mu.Unlock()
		select {
		case <-done:
			return grant.Stop(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	grant.stopping = true
	grant.stopDone = make(chan struct{})
	sandboxID, tokenPath := grant.sandboxID, grant.tokenPath
	done := grant.stopDone
	grant.mu.Unlock()

	var stopErr error
	if sandboxID != "" {
		stopErr = grant.server.revoke(ctx, sandboxID)
	}
	stopErr = errors.Join(stopErr, removeToken(tokenPath))

	grant.mu.Lock()
	grant.stopping = false
	if stopErr == nil {
		grant.stopped = true
		grant.sandboxID = ""
		grant.tokenPath = ""
	}
	close(done)
	grant.mu.Unlock()
	return stopErr
}

func (grant *Grant) writeToken(taskID, token string) (string, error) {
	directory := filepath.Join(grant.outputDir, taskID, "bridge")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create task bridge artifact directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", fmt.Errorf("secure task bridge artifact directory: %w", err)
	}
	path := filepath.Join(directory, "e2b-access.token")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create task access token: %w", err)
	}
	if _, err := file.WriteString(token); err != nil {
		return "", errors.Join(fmt.Errorf("write task access token: %w", err), file.Close(), removeToken(path))
	}
	if err := file.Sync(); err != nil {
		return "", errors.Join(fmt.Errorf("sync task access token: %w", err), file.Close(), removeToken(path))
	}
	if err := file.Close(); err != nil {
		return "", errors.Join(fmt.Errorf("close task access token: %w", err), removeToken(path))
	}
	return path, nil
}

func removeToken(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
