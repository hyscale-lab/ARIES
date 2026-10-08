package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh/credentials"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
	gossh "golang.org/x/crypto/ssh"
)

const (
	defaultBridgeCleanup  = 20 * time.Second
	maxRecordedInputBytes = 16 << 20
	maxToolLogBytes       = 256 << 20

	lockedUsername       = "aries"
	lockedConnectTimeout = 5 * time.Second
)

// Options are the host-local inputs to one SSH bridge.
type Options struct {
	Dialect Dialect
	// Credentials selects serving with controller-staged keys. No client private key enters the server.
	Credentials *credentials.Credentials
	// ResolveListen supplies task-local listener and harness destination settings.
	ResolveListen  func(context.Context) (core.BridgeListen, error)
	OutputDir      string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
	// OmitRawLog drops ssh_raw.log, the byte-level record of every channel
	// request. That log is the only artifact holding the raw wire command,
	// the request payload, and binary stdin that the structured log omits,
	// so it is the forensic record rather than a duplicate of
	// tool-calls.jsonl. The zero value retains it; setting this trades that
	// evidence for disk.
	OmitRawLog bool
}

// Manager owns one occurrence and proxies exec requests to its borrowed target.
type Manager struct {
	dialect        Dialect
	used           bool
	credentials    *credentials.Credentials
	resolveListen  func(context.Context) (core.BridgeListen, error)
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	openAudit      func(string) (*auditFile, error)
	afterStart     func(*bridgeSession) error
	omitRawLog     bool

	mu       sync.Mutex
	active   *bridgeSession
	stopping bool
	stopDone chan struct{}
	stopErr  error
}

type bridgeSession struct {
	dialect       Dialect
	sandbox       target.Executor
	listener      net.Listener
	configuration *gossh.ServerConfig
	cancel        context.CancelFunc
	artifactDir   string
	knownSource   string
	toolLogPath   string
	rawLogPath    string
	audit         *auditWriter
	partialStart  bool
	replyRequest  func(*gossh.Request, bool) error

	mu            sync.Mutex
	connections   map[net.Conn]struct{}
	revoked       bool
	done          chan struct{}
	revocationMu  sync.Mutex
	revocationErr error
	wait          sync.WaitGroup
	revokeOnce    sync.Once
}

func New(options Options) (*Manager, error) {
	if options.Dialect == nil {
		return nil, errors.New("SSH dialect is required")
	}
	if options.Credentials == nil || options.Credentials.HostSigner == nil || options.Credentials.AuthorizedKey == nil {
		return nil, errors.New("staged SSH credentials are required")
	}
	if options.ResolveListen == nil {
		return nil, errors.New("SSH bridge listen resolver is required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("SSH output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve SSH output directory: %w", err)
	}
	if err := ensurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare SSH output directory: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{dialect: options.Dialect,
		resolveListen: options.ResolveListen, credentials: options.Credentials,
		outputDir: outputDir, cleanupTimeout: options.CleanupTimeout,
		logger: options.Logger, openAudit: openAuditFile, omitRawLog: options.OmitRawLog,
	}, nil
}

// StartTarget serves a borrowed execution capability without sandbox lifecycle authority.
func (manager *Manager) StartTarget(ctx context.Context, sandbox target.Executor) (core.ToolEndpoint, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.used || manager.active != nil || manager.stopping {
		return core.ToolEndpoint{}, errors.New("SSH bridge is already active")
	}
	if sandbox == nil {
		return core.ToolEndpoint{}, errors.New("SSH target is required")
	}
	listen, err := manager.resolveListen(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve bridge listener: %w", err)
	}
	bindIP := net.ParseIP(listen.BindHost)
	if bindIP == nil || bindIP.To4() == nil || !core.ValidEndpointHost(listen.AdvertiseHost) || listen.BindPort < 0 || listen.BindPort > 65535 || listen.AdvertisePort < 0 || listen.AdvertisePort > 65535 {
		return core.ToolEndpoint{}, errors.New("SSH bridge requires an IPv4 bind host, valid advertised host and valid ports")
	}
	manager.used = true
	session := &bridgeSession{dialect: manager.dialect,
		sandbox: sandbox, connections: make(map[net.Conn]struct{}),
		replyRequest: func(request *gossh.Request, accepted bool) error { return request.Reply(accepted, nil) },
	}
	session.artifactDir = filepath.Join(manager.outputDir, sandbox.TaskID(), "bridge")
	fail := func(primary error) (core.ToolEndpoint, error) {
		session.partialStart = true
		session.revoke()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		waitErr := session.waitFor(cleanupCtx)
		if waitErr != nil {
			manager.active = session
			return core.ToolEndpoint{}, errors.Join(primary, waitErr)
		}
		cleanupErr := session.finalize(cleanupCtx)
		if cleanupErr != nil {
			manager.active = session
		}
		return core.ToolEndpoint{}, errors.Join(primary, cleanupErr)
	}
	if err := ensurePrivateDirectory(session.artifactDir); err != nil {
		return fail(fmt.Errorf("create private SSH artifact directory: %w", err))
	}
	hostSigner, authorized := manager.credentials.HostSigner, manager.credentials.AuthorizedKey
	session.knownSource = filepath.Join(session.artifactDir, "known_hosts")
	session.toolLogPath = filepath.Join(session.artifactDir, "tool-calls.jsonl")
	if !manager.omitRawLog {
		session.rawLogPath = filepath.Join(session.artifactDir, "ssh_raw.log")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(listen.BindHost, strconv.Itoa(listen.BindPort)))
	if err != nil {
		return fail(fmt.Errorf("listen on configured bridge host: %w", err))
	}
	session.listener = listener
	_, port, err := net.SplitHostPort(listener.Addr().String())
	host := listen.AdvertiseHost
	if listen.AdvertisePort != 0 {
		port = strconv.Itoa(listen.AdvertisePort)
	}
	if err != nil {
		return fail(fmt.Errorf("parse SSH listener address: %w", err))
	}
	// Retain the public host key as native evidence. Harness credential locations
	// and whether to preload this key are supplied to the controller by wiring.
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, gossh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := writeExclusivePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return fail(fmt.Errorf("write SSH known-hosts file: %w", err))
	}
	structured, err := manager.openAudit(session.toolLogPath)
	if err != nil {
		return fail(fmt.Errorf("create SSH tool log: %w", err))
	}
	var raw *auditFile
	if session.rawLogPath != "" {
		raw, err = manager.openAudit(session.rawLogPath)
		if err != nil {
			return fail(errors.Join(fmt.Errorf("create SSH raw log: %w", err), structured.close()))
		}
	}
	session.audit = newAuditWriter(structured, raw)
	serveCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	session.configuration = newServerConfig(hostSigner, authorized)
	session.wait.Add(1)
	go session.serve(serveCtx, manager.logger)
	if manager.afterStart != nil {
		if err := manager.afterStart(session); err != nil {
			return fail(err)
		}
	}
	manager.active = session
	manager.stopErr = nil
	address := net.JoinHostPort(host, port)
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "container": sandbox.ContainerName()}).Info("SSH bridge started")
	endpoint := core.ToolEndpoint{Protocol: "ssh", Address: address, Username: lockedUsername, Workdir: sandbox.Workdir(), LogPaths: session.logPaths(), KnownHostsSourceFile: session.knownSource}
	return endpoint, nil
}

// logPaths omits ssh_raw.log when it was not retained, so the endpoint never
// advertises an artifact that does not exist.
func (session *bridgeSession) logPaths() []string {
	if session.rawLogPath == "" {
		return []string{session.toolLogPath}
	}
	return []string{session.toolLogPath, session.rawLogPath}
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	if manager.active == nil && !manager.stopping {
		manager.used = true
		manager.credentials = nil
		err := manager.stopErr
		manager.mu.Unlock()
		return err
	}
	if manager.stopping {
		done := manager.stopDone
		manager.mu.Unlock()
		select {
		case <-done:
			manager.mu.Lock()
			err := manager.stopErr
			manager.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	session := manager.active
	manager.stopping = true
	manager.stopDone = make(chan struct{})
	done := manager.stopDone
	manager.mu.Unlock()

	session.revoke()
	err := session.waitFor(ctx)
	if err == nil {
		err = session.finalize(ctx)
	}
	manager.mu.Lock()
	manager.stopErr = err
	manager.stopping = false
	if err == nil {
		manager.active = nil
		manager.credentials = nil
	}
	close(done)
	manager.mu.Unlock()
	return err
}

func (session *bridgeSession) finalize(ctx context.Context) error {
	auditErr := session.closeAudit(ctx)
	if session.audit != nil && !session.audit.finished() {
		return auditErr
	}
	// The controller owns private client material. Retain the public host key
	// as evidence; closing admission and joining execution happens before here.
	session.configuration = nil
	cleanupErr := errors.Join(
		session.revocationError(), auditErr,
	)
	if session.partialStart && cleanupErr == nil {
		cleanupErr = os.RemoveAll(session.artifactDir)
	}
	return cleanupErr
}

func (session *bridgeSession) recordRevocationError(err error) {
	if !hasCancellationCause(err) {
		return
	}
	session.revocationMu.Lock()
	session.revocationErr = errors.Join(session.revocationErr, err)
	session.revocationMu.Unlock()
}

func (session *bridgeSession) revocationError() error {
	session.revocationMu.Lock()
	defer session.revocationMu.Unlock()
	if isPureCancellation(session.revocationErr) {
		return nil
	}
	return session.revocationErr
}

func hasCancellationCause(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isPureCancellation(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isPureCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isPureCancellation(wrapped.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

func (session *bridgeSession) revoke() {
	session.revokeOnce.Do(func() {
		session.mu.Lock()
		session.revoked = true
		session.done = make(chan struct{})
		connections := make([]net.Conn, 0, len(session.connections))
		for connection := range session.connections {
			connections = append(connections, connection)
		}
		done := session.done
		session.mu.Unlock()
		if session.cancel != nil {
			session.cancel()
		}
		if session.listener != nil {
			_ = session.listener.Close()
		}
		for _, connection := range connections {
			_ = connection.Close()
		}
		// Admission is closed under the same lock as every Add. All cleanup
		// attempts share one waiter, including retries after a deadline.
		go func() { session.wait.Wait(); close(done) }()
	})
}

func (session *bridgeSession) waitFor(ctx context.Context) error {
	session.mu.Lock()
	done := session.done
	session.mu.Unlock()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
