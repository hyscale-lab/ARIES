package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	sshcredentials "github.com/hyscale-lab/aries/pkg/bridge/ssh/credentials"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
)

type Options struct {
	Runtime                                         deployment.Runtime
	Request                                         deployment.Request
	OutputDir, ClientPath, DockerSocket, BridgeType string
	RetainRawLog                                    bool
}
type targetExporter interface {
	ExportBridgeTarget() (core.BridgeTarget, error)
}
type Manager struct {
	runtimeClosed                                  bool
	mu                                             sync.Mutex
	options                                        Options
	started                                        bool
	runtimeID, instance, assignment, local, remote string
	descriptor                                     core.BridgeTarget
	connection                                     *grpc.ClientConn
	client                                         v1.BridgeControlClient
	exposed                                        bool
	assigned                                       bool
	revoked                                        bool
	collected                                      bool
	removed                                        bool
	manifest                                       []*v1.Artifact
	renewCancel                                    context.CancelFunc
	renewDone                                      chan struct{}
	isolationErr                                   error
	secretFiles                                    []string
}

var _ runner.ToolBridge = (*Manager)(nil)

func New(options Options) (*Manager, error) {
	if options.Runtime == nil || options.OutputDir == "" {
		return nil, errors.New("managed bridge requires runtime and output directory")
	}
	if options.BridgeType != "hermes-ssh" && options.BridgeType != "openclaw-ssh" {
		return nil, errors.New("unsupported native bridge type")
	}
	if options.BridgeType == "openclaw-ssh" && options.ClientPath == "" {
		return nil, errors.New("OpenClaw bridge requires local client helper")
	}
	root, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, err
	}
	options.OutputDir = root
	if options.ClientPath != "" {
		options.ClientPath, err = filepath.Abs(options.ClientPath)
		if err != nil {
			return nil, err
		}
	}
	return &Manager{options: options}, nil
}
func nonce() (string, error) {
	var b [24]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func (m *Manager) record() error {
	b, err := json.MarshalIndent(struct{ Backend, RuntimeID, InstanceID, AssignmentID, ResourceMetrics string }{"docker", m.runtimeID, m.instance, m.assignment, "docker-stats"}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.local, "runtime.json"), b, 0600)
}
func (m *Manager) Start(ctx context.Context, sandbox runner.Sandbox) (core.ToolEndpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var endpoint core.ToolEndpoint
	if m.started {
		return endpoint, errors.New("managed bridge does not admit another occurrence")
	}
	m.started = true
	exporter, ok := sandbox.(targetExporter)
	if !ok {
		return endpoint, errors.New("sandbox cannot export a bridge target")
	}
	d, err := exporter.ExportBridgeTarget()
	if err != nil {
		return endpoint, err
	}
	if err = target.Validate(d); err != nil {
		return endpoint, err
	}
	if d.Backend != "docker" {
		return endpoint, errors.New("unsupported bridge and sandbox deployment pairing")
	}
	m.descriptor = d
	m.instance, err = nonce()
	if err != nil {
		return endpoint, err
	}
	m.assignment, err = nonce()
	if err != nil {
		return endpoint, err
	}
	m.local = filepath.Join(m.options.OutputDir, d.TaskID, "bridge")
	if err = os.MkdirAll(m.local, 0700); err != nil {
		return endpoint, err
	}
	req := m.options.Request
	req.Name = "aries-bridge-" + m.instance[:24]
	req.Labels = map[string]string{"aries.managed": "true", "aries.component": "bridge", "aries.kind": "tool-bridge", "aries.run": d.RunID, "aries.task": d.TaskID, "aries.attempt": d.OccurrenceID}
	req.Placement = sandbox.Connectivity().Placement
	config := LaunchConfig{InstanceID: m.instance, BridgeType: m.options.BridgeType, Backend: d.Backend, DockerSocket: m.options.DockerSocket, RetainRawLog: m.options.RetainRawLog}
	destination := "/tmp/aries-bridge"
	req.Workdir = destination
	req.Entrypoint = []string{"/usr/local/bin/aries-bridge"}
	req.Args = []string{"config.json"}
	req.ServicePort = 8443
	req.HarnessPort = 2222
	config.ControlAddress = "0.0.0.0:8443"
	config.OutputDir = destination + "/evidence"
	config.Listen = core.BridgeListen{BindHost: "0.0.0.0", BindPort: 2222, AdvertisePort: 2222}
	req.TrustedDockerSocket = strings.TrimPrefix(m.options.DockerSocket, "unix://")
	if req.TrustedDockerSocket == "" {
		req.TrustedDockerSocket = "/var/run/docker.sock"
	}
	config.DockerSocket = "/var/run/docker.sock"
	m.remote = filepath.Join(config.OutputDir, d.TaskID, "bridge")
	m.runtimeID, err = m.options.Runtime.Create(ctx, req)
	if recordErr := m.record(); recordErr != nil {
		return endpoint, errors.Join(err, recordErr)
	}
	if err != nil {
		return endpoint, err
	}
	if m.runtimeID == "" {
		return endpoint, errors.New("bridge runtime returned empty identity")
	}
	clientKey, clientPublic, err := sshcredentials.GenerateIdentity()
	if err != nil {
		return endpoint, err
	}
	hostKey, hostPublic, err := sshcredentials.GenerateIdentity()
	if err != nil {
		return endpoint, err
	}
	identityPath := filepath.Join(m.local, "id_ed25519")
	m.secretFiles = append(m.secretFiles, identityPath)
	if err = os.WriteFile(identityPath, clientKey, 0600); err != nil {
		return endpoint, err
	}
	controlKeys, err := newControlCredentials()
	if err != nil {
		return endpoint, err
	}
	token, err := nonce()
	if err != nil {
		return endpoint, err
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return endpoint, err
	}
	archive, err := archiveFiles(map[string][]byte{"config.json": configJSON, "token": []byte(token), "ca.pem": controlKeys.CA, "server.pem": controlKeys.ServerCert, "server.key": controlKeys.ServerKey, "host.key": hostKey, "authorized.pub": ssh.MarshalAuthorizedKey(clientPublic)})
	if err != nil {
		return endpoint, err
	}
	if err = m.options.Runtime.UploadArchive(ctx, m.runtimeID, destination, bytes.NewReader(archive)); err != nil {
		return endpoint, err
	}
	if err = m.options.Runtime.Validate(ctx, m.runtimeID, req, [][]byte{clientKey, hostKey, controlKeys.ServerKey, controlKeys.ClientKey, []byte(token)}); err != nil {
		return endpoint, err
	}
	if err = m.options.Runtime.Start(ctx, m.runtimeID); err != nil {
		return endpoint, err
	}
	tlsConfig, err := controlTLS(controlKeys.CA, controlKeys.ClientCert, controlKeys.ClientKey, false)
	if err != nil {
		return endpoint, err
	}
	for {
		address, addressErr := m.options.Runtime.Address(ctx, m.runtimeID, req.ServicePort)
		if addressErr == nil {
			m.connection, m.client, err = control.NewClient(address, m.instance, token, credentials.NewTLS(tlsConfig))
			if err != nil {
				return endpoint, err
			}
			checkCtx, cancel := context.WithTimeout(ctx, time.Second)
			health, checkErr := healthpb.NewHealthClient(m.connection).Check(checkCtx, &healthpb.HealthCheckRequest{})
			cancel()
			if checkErr == nil && health.Status == healthpb.HealthCheckResponse_SERVING {
				break
			}
			m.connection.Close()
			m.connection = nil
			m.client = nil
		}
		running, runErr := m.options.Runtime.Running(ctx, m.runtimeID)
		if runErr != nil || !running {
			return endpoint, errors.Join(errors.New("bridge runtime exited before control readiness"), runErr)
		}
		if err = waitPoll(ctx); err != nil {
			return endpoint, err
		}
	}
	m.assigned = true // A lost admission response still owns this exact assignment.
	assignment, err := m.assignSandbox(ctx, &v1.AssignSandboxRequest{InstanceId: m.instance, AssignmentId: m.assignment, ProtocolVersion: 1, Target: control.TargetToProto(d), LeaseMillis: 60000, CredentialId: "ssh"})
	if err != nil {
		return endpoint, err
	}
	if err = m.validateAssignment(assignment); err != nil {
		return endpoint, err
	}
	if assignment.State != v1.State_READY || assignment.Endpoint == nil {
		failures := []error{errors.New("bridge assignment failed readiness")}
		for _, failure := range assignment.CleanupErrors {
			failures = append(failures, fmt.Errorf("%s: %s", failure.Stage, failure.Message))
		}
		return endpoint, errors.Join(failures...)
	}
	native := assignment.Endpoint
	if strings.TrimSpace(native.HostKey) != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPublic))) {
		return endpoint, errors.New("bridge host identity differs from staged key")
	}
	address := net.JoinHostPort(native.Host, strconv.Itoa(int(native.Port)))
	if native.Transport != "ssh" || native.User != "aries" || native.Port == 0 || native.Port > 65535 {
		return endpoint, errors.New("invalid native bridge endpoint")
	}
	services, ok := m.options.Runtime.(deployment.ServiceRuntime)
	if !ok {
		return endpoint, errors.New("runtime has no harness address")
	}
	expected, err := services.HarnessAddress(ctx, m.runtimeID, req.HarnessPort)
	if err != nil {
		return endpoint, err
	}
	if address != expected {
		return endpoint, errors.New("bridge advertised a different task attachment")
	}
	endpoint = core.ToolEndpoint{Protocol: "ssh", Address: address, Username: native.User, IdentityFile: "/run/aries/ssh/id_ed25519", IdentitySourceFile: identityPath, Workdir: d.Workdir, LogPaths: []string{filepath.Join(m.local, "tool-calls.jsonl")}}
	if m.options.RetainRawLog {
		endpoint.LogPaths = append(endpoint.LogPaths, filepath.Join(m.local, "ssh_raw.log"))
	}
	// Retain the public host identity as evidence for both native protocols.
	known := filepath.Join(m.local, "known_hosts")
	line := "[" + native.Host + "]:" + strconv.Itoa(int(native.Port)) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPublic))) + "\n"
	if err = os.WriteFile(known, []byte(line), 0600); err != nil {
		return endpoint, err
	}
	if m.options.BridgeType == "openclaw-ssh" {
		endpoint.KnownHostsFile = "/run/aries/ssh/known_hosts"
		endpoint.KnownHostsSourceFile = known
		endpoint.ClientCommand = "/opt/aries/bin/aries-ssh-client"
		endpoint.ClientSourceFile = filepath.Join(m.local, "aries-ssh-client")
		m.secretFiles = append(m.secretFiles, endpoint.ClientSourceFile)
		if err := stageClientHelper(m.options.ClientPath, endpoint.ClientSourceFile); err != nil {
			return core.ToolEndpoint{}, err
		}
	}
	renewCtx, cancel := context.WithCancel(context.Background())
	m.renewCancel = cancel
	m.renewDone = make(chan struct{})
	go m.renew(renewCtx)
	m.exposed = true
	return endpoint, nil
}

// stageClientHelper preserves the existing native bridge/harness contract:
// an immutable executable source is copied into task-local mode-0555 staging.
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
func (m *Manager) assignmentRequest() *v1.AssignmentRequest {
	return &v1.AssignmentRequest{InstanceId: m.instance, AssignmentId: m.assignment}
}
func (m *Manager) validateAssignment(a *v1.Assignment) error {
	if a == nil || a.InstanceId != m.instance || a.AssignmentId != m.assignment || !proto.Equal(a.Target, control.TargetToProto(m.descriptor)) {
		return errors.New("bridge assignment identity differs")
	}
	return nil
}
func (m *Manager) renew(ctx context.Context) {
	defer close(m.renewDone)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			call, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, _ = m.client.RenewLease(call, &v1.RenewLeaseRequest{InstanceId: m.instance, AssignmentId: m.assignment, LeaseMillis: 60000})
			cancel()
		}
	}
}
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.renewCancel != nil {
		m.renewCancel()
		<-m.renewDone
		m.renewCancel = nil
	}
	if m.assigned && !m.revoked {
		if m.isolationErr != nil {
			return m.disposeFailed(ctx)
		}
		if m.client == nil {
			return m.handleRevocationFailure(ctx, errors.New("bridge assignment revocation unconfirmed: control unavailable"))
		}
		a, err := m.client.RevokeAssignment(ctx, m.assignmentRequest())
		if err != nil {
			return m.handleRevocationFailure(ctx, fmt.Errorf("bridge revocation unconfirmed: %w", err))
		}
		if err = m.validateAssignment(a); err != nil {
			return err
		}
		if a.State != v1.State_REVOKED || len(a.CleanupErrors) > 0 {
			err := fmt.Errorf("bridge assignment drain or evidence finalization unconfirmed (state %s)", a.State)
			for _, failure := range a.CleanupErrors {
				err = errors.Join(err, fmt.Errorf("%s: %s", failure.GetStage(), failure.GetMessage()))
			}
			return err
		}
		m.revoked = true
		m.manifest = a.Artifacts
	}
	if m.revoked && !m.collected {
		expected := map[string]bool{"tool-calls.jsonl": false}
		if m.options.RetainRawLog {
			expected["ssh_raw.log"] = false
		}
		for _, entry := range m.manifest {
			seen, ok := expected[entry.Name]
			if !ok || seen {
				return errors.New("bridge evidence manifest is incomplete or unexpected")
			}
			if entry.Status == "absent" && !m.exposed && entry.Size == 0 && entry.Sha256 == "" {
				expected[entry.Name] = true
				continue
			}
			if entry.Status != "complete" {
				return errors.New("bridge evidence manifest is incomplete or unexpected")
			}
			if err := collectArtifact(ctx, m.options.Runtime, m.runtimeID, m.remote, m.local, Artifact{Name: entry.Name, Size: entry.Size, SHA256: entry.Sha256}); err != nil {
				return err
			}
			expected[entry.Name] = true
		}
		for _, seen := range expected {
			if !seen {
				return errors.New("bridge evidence missing from finalized manifest")
			}
		}
		m.collected = true
	}
	if m.runtimeID != "" && !m.removed {
		if err := m.options.Runtime.Stop(ctx, m.runtimeID); err != nil {
			return err
		}
		m.removed = true
	}
	for _, file := range m.secretFiles {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if m.connection != nil {
		if err := m.connection.Close(); err != nil {
			return err
		}
		m.connection = nil
	}
	if !m.runtimeClosed {
		if err := m.options.Runtime.Close(); err != nil {
			return err
		}
		m.runtimeClosed = true
	}
	return nil
}

// A positively observed child exit cannot prove sandbox execution drain. Dispose
// failed-run resources but preserve that isolation failure across every retry.
func (m *Manager) handleRevocationFailure(ctx context.Context, failure error) error {
	running, err := m.options.Runtime.Running(ctx, m.runtimeID)
	if err != nil || running {
		return errors.Join(failure, err)
	}
	m.isolationErr = failure
	return m.disposeFailed(ctx)
}
func (m *Manager) disposeFailed(ctx context.Context) error {
	var failures []error
	if !m.removed && m.runtimeID != "" {
		if err := m.options.Runtime.Stop(ctx, m.runtimeID); err != nil {
			failures = append(failures, err)
		} else {
			m.removed = true
		}
	}
	if m.removed {
		for _, file := range m.secretFiles {
			if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
		}
		if m.connection != nil {
			failures = append(failures, m.connection.Close())
			m.connection = nil
		}
	}
	if m.removed && len(failures) == 0 && !m.runtimeClosed {
		if err := m.options.Runtime.Close(); err != nil {
			failures = append(failures, err)
		} else {
			m.runtimeClosed = true
		}
	}
	return errors.Join(append([]error{m.isolationErr}, failures...)...)
}
