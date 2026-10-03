package remote

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	defaultRequestTimeout = 2 * time.Minute
	defaultDaemonCleanup  = 30 * time.Second
)

// BridgeFactory builds the SSH bridge for one grant. It is a function rather
// than a switch here so this package depends on no concrete bridge; the
// switch lives in cmd/aries-bridge.
type BridgeFactory func(request GrantRequest, outputDir string, host ssh.Signer, authorized ssh.PublicKey) (runner.ToolBridge, error)

// AttachFunc returns a verified handle on the sandbox a grant names.
type AttachFunc func(context.Context, GrantRequest) (runner.Sandbox, error)

// DaemonOptions configure the in-pod bridge daemon.
type DaemonOptions struct {
	// OutputDir holds one private directory per live grant. It should be
	// container-local scratch space: collect copies what matters to the runner.
	OutputDir string
	// Backend is the only sandbox backend grants may name: "kubernetes" or
	// "docker".
	Backend string
	// Namespace is, for Kubernetes, the only namespace grants may name. The
	// daemon's RBAC is namespaced anyway; this makes a mismatch an explicit
	// refusal.
	Namespace      string
	NewBridge      BridgeFactory
	Attach         AttachFunc
	RequestTimeout time.Duration
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// Daemon serves many grants, one SSH bridge each, and keeps them only in
// memory.
type Daemon struct {
	outputDir      string
	backend        string
	namespace      string
	newBridge      BridgeFactory
	attach         AttachFunc
	requestTimeout time.Duration
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	instance       string

	mu     sync.Mutex
	grants map[string]*daemonGrant
	closed bool
}

type daemonGrant struct {
	// mu serialises every operation on one grant, so a revoke that arrives
	// while the grant is still starting waits for the start to settle.
	mu      sync.Mutex
	dir     string
	bridge  runner.ToolBridge
	state   string
	logs    []string
	removed bool
}

// NewDaemon validates options and picks this process's instance ID.
func NewDaemon(options DaemonOptions) (*Daemon, error) {
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("bridge daemon output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("create bridge daemon output directory: %w", err)
	}
	switch options.Backend {
	case BackendKubernetes:
		if options.Namespace == "" {
			return nil, errors.New("bridge daemon namespace is required for the Kubernetes backend")
		}
	case BackendDocker:
	default:
		return nil, fmt.Errorf("unknown bridge daemon backend %q", options.Backend)
	}
	if options.NewBridge == nil || options.Attach == nil {
		return nil, errors.New("bridge daemon needs a bridge factory and a sandbox attach function")
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultDaemonCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	instance, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		outputDir: outputDir, backend: options.Backend, namespace: options.Namespace,
		newBridge: options.NewBridge, attach: options.Attach,
		requestTimeout: options.RequestTimeout, cleanupTimeout: options.CleanupTimeout,
		logger: options.Logger, instance: instance, grants: make(map[string]*daemonGrant),
	}, nil
}

// Instance is this process's random ID.
func (d *Daemon) Instance() string { return d.instance }

// Handle answers one request. Errors are reported in the response, never as
// a Go error, so the runner always learns the instance and the grant state.
func (d *Daemon) Handle(ctx context.Context, request Request) Response {
	response := Response{Instance: d.instance}
	if request.Op != OpStatus {
		if err := validateGrantID(request.GrantID); err != nil {
			response.Error = err.Error()
			return response
		}
	}
	var err error
	switch request.Op {
	case OpGrant:
		response.Grant, err = d.grant(ctx, request.GrantID, request.Grant)
		response.State = StateActive
	case OpRevoke:
		response.State, err = d.revoke(ctx, request.GrantID)
	case OpCollect:
		response.Files, err = d.collect(request.GrantID)
		response.State = StateRevoked
	case OpRelease:
		response.State, err = d.release(request.GrantID)
	case OpStatus:
		d.mu.Lock()
		response.Grants = len(d.grants)
		d.mu.Unlock()
	default:
		err = fmt.Errorf("unknown operation %q", request.Op)
	}
	if err != nil {
		response.Error = err.Error()
		response.Grant, response.Files = nil, nil
		if request.Op == OpGrant || request.Op == OpCollect {
			response.State = d.stateOf(request.GrantID)
		}
	}
	d.logger.WithFields(logrus.Fields{"op": request.Op, "grant_id": request.GrantID, "state": response.State, "error": response.Error}).Info("bridge control request")
	return response
}

func (d *Daemon) grant(ctx context.Context, id string, request *GrantRequest) (*GrantResponse, error) {
	if err := validateGrantRequest(request); err != nil {
		return nil, err
	}
	if request.Sandbox.Backend != d.backend {
		return nil, fmt.Errorf("grant names a %s sandbox; this bridge serves %s", request.Sandbox.Backend, d.backend)
	}
	if d.backend == BackendKubernetes && request.Sandbox.Namespace != d.namespace {
		return nil, fmt.Errorf("grant names namespace %q; this bridge serves %q", request.Sandbox.Namespace, d.namespace)
	}
	authorized, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(request.AuthorizedKey))
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("authorized_key is not one authorized_keys line")
	}
	entry := &daemonGrant{dir: filepath.Join(d.outputDir, id), state: "starting"}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, errors.New("bridge daemon is shutting down")
	}
	if _, exists := d.grants[id]; exists {
		d.mu.Unlock()
		return nil, fmt.Errorf("grant %s already exists", id)
	}
	d.grants[id] = entry
	d.mu.Unlock()

	// discard forgets a grant that never served anything.
	discard := func(primary error) (*GrantResponse, error) {
		d.forget(id, entry)
		return nil, errors.Join(primary, os.RemoveAll(entry.dir))
	}
	sandbox, err := d.attach(ctx, *request)
	if err != nil {
		return discard(err)
	}
	host, err := newHostSigner()
	if err != nil {
		return discard(err)
	}
	bridge, err := d.newBridge(*request, entry.dir, host, authorized)
	if err != nil {
		return discard(err)
	}
	endpoint, err := bridge.Start(ctx, sandbox)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cleanupTimeout)
		defer cancel()
		if stopErr := bridge.Stop(cleanupCtx); stopErr != nil {
			// Revocation of the partial start is unconfirmed, so the grant
			// stays registered and a later revoke retries it.
			entry.bridge, entry.state = bridge, StateActive
			return nil, errors.Join(err, stopErr)
		}
		return discard(err)
	}
	entry.bridge, entry.state = bridge, StateActive
	names, err := d.adoptLogs(entry, endpoint)
	if err != nil {
		return nil, err
	}
	return &GrantResponse{
		Address: endpoint.Address, HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host.PublicKey()))),
		Network: endpoint.Network, LogFiles: names,
	}, nil
}

// adoptLogs keeps the endpoint's log paths, refusing any outside the grant's
// directory or with an unexpected name.
func (d *Daemon) adoptLogs(entry *daemonGrant, endpoint core.ToolEndpoint) ([]string, error) {
	names := make([]string, 0, len(endpoint.LogPaths))
	for _, path := range endpoint.LogPaths {
		relative, err := filepath.Rel(entry.dir, path)
		if err != nil || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
			return nil, fmt.Errorf("bridge log %q is outside the grant directory", path)
		}
		name := filepath.Base(path)
		if err := validateLogFile(name); err != nil {
			return nil, err
		}
		entry.logs = append(entry.logs, path)
		names = append(names, name)
	}
	return names, nil
}

func (d *Daemon) revoke(ctx context.Context, id string) (string, error) {
	entry := d.lookup(id)
	if entry == nil {
		return StateAbsent, nil
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.removed {
		return StateAbsent, nil
	}
	if entry.state == StateRevoked {
		return StateRevoked, nil
	}
	if err := entry.bridge.Stop(ctx); err != nil {
		return StateActive, fmt.Errorf("revocation not confirmed: %w", err)
	}
	entry.state = StateRevoked
	return StateRevoked, nil
}

// collect returns the grant's log files. Evidence is only released after
// revocation, so it is complete and no session can still be appending to it.
func (d *Daemon) collect(id string) ([]string, error) {
	entry := d.lookup(id)
	if entry == nil {
		return nil, fmt.Errorf("grant %s is absent", id)
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.removed {
		return nil, fmt.Errorf("grant %s is absent", id)
	}
	if entry.state != StateRevoked {
		return nil, fmt.Errorf("grant %s is not revoked", id)
	}
	return append([]string(nil), entry.logs...), nil
}

func (d *Daemon) release(id string) (string, error) {
	entry := d.lookup(id)
	if entry == nil {
		return StateAbsent, nil
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.removed {
		return StateAbsent, nil
	}
	if entry.state != StateRevoked {
		return entry.state, fmt.Errorf("grant %s is not revoked", id)
	}
	if err := os.RemoveAll(entry.dir); err != nil {
		return StateRevoked, err
	}
	d.forget(id, entry)
	return StateAbsent, nil
}

func (d *Daemon) lookup(id string) *daemonGrant {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.grants[id]
}

// forget removes the grant from the registry. The caller holds entry.mu.
func (d *Daemon) forget(id string, entry *daemonGrant) {
	entry.removed = true
	d.mu.Lock()
	if d.grants[id] == entry {
		delete(d.grants, id)
	}
	d.mu.Unlock()
}

func (d *Daemon) stateOf(id string) string {
	entry := d.lookup(id)
	if entry == nil {
		return StateAbsent
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.removed {
		return StateAbsent
	}
	return entry.state
}

// Close refuses new grants and revokes every live one.
func (d *Daemon) Close(ctx context.Context) error {
	d.mu.Lock()
	d.closed = true
	entries := make([]*daemonGrant, 0, len(d.grants))
	for _, entry := range d.grants {
		entries = append(entries, entry)
	}
	d.mu.Unlock()
	var errs []error
	for _, entry := range entries {
		entry.mu.Lock()
		if entry.bridge != nil && entry.state == StateActive {
			if err := entry.bridge.Stop(ctx); err != nil {
				errs = append(errs, err)
			} else {
				entry.state = StateRevoked
			}
		}
		entry.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Serve answers requests on a Unix socket until ctx ends. The socket is 0600,
// so only the container's own user, which is who `kubectl exec` runs as, can
// reach it.
func (d *Daemon) Serve(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale control socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen on control socket: %w", err)
	}
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("protect control socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	var wait sync.WaitGroup
	defer wait.Wait()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept control connection: %w", err)
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			d.serveConnection(ctx, connection)
		}()
	}
}

func (d *Daemon) serveConnection(ctx context.Context, connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(d.requestTimeout + 10*time.Second))
	var request Request
	reader := bufio.NewReader(&limitedReader{reader: connection, remaining: maxMessageBytes})
	line, err := reader.ReadBytes('\n')
	var response Response
	if err != nil {
		response = Response{Instance: d.instance, Error: fmt.Sprintf("read request: %v", err)}
	} else if err := json.Unmarshal(line, &request); err != nil {
		response = Response{Instance: d.instance, Error: fmt.Sprintf("parse request: %v", err)}
	} else {
		requestCtx, cancel := context.WithTimeout(ctx, d.requestTimeout)
		response = d.Handle(requestCtx, request)
		cancel()
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return
	}
	_, _ = connection.Write(append(encoded, '\n'))
}

type limitedReader struct {
	reader    interface{ Read([]byte) (int, error) }
	remaining int
}

func (r *limitedReader) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("control message too large")
	}
	if len(buffer) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	n, err := r.reader.Read(buffer)
	r.remaining -= n
	return n, err
}

func newHostSigner() (ssh.Signer, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate SSH host key: %w", err)
	}
	return ssh.NewSignerFromKey(private)
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
