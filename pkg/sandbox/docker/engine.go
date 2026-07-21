package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/sandbox/docker/execproto"
)

const (
	defaultDockerSocket = "/var/run/docker.sock"
	dockerAPIVersion    = "v1.44"
	enginePollInterval  = 20 * time.Millisecond
	maxEngineResponse   = 1 << 20
	maxExecInput        = 16 << 20
	maxExecOutput       = 16 << 20
	maxRejectedPeers    = 32
	helperContainerPath = "/opt/aries/bin/aries-exec-helper"
	socketContainerDir  = "/run/aries"
)

type execEngine interface {
	Exec(context.Context, string, string, core.Command) (core.CommandResult, bool, error)
}

type engineClient struct {
	socket       string
	apiVersion   string
	pollInterval time.Duration
	httpClient   *http.Client
	newID        func() (string, error)
	socketOps    engineSocketOps
}

type engineSocketOps struct {
	listenUnix      func(string, *net.UnixAddr) (*net.UnixListener, error)
	chmod           func(string, os.FileMode) error
	remove          func(string) error
	lstat           func(string) (os.FileInfo, error)
	closeListener   func(*net.UnixListener) error
	closeConnection func(*net.UnixConn) error
}

type onceCloser struct {
	once     sync.Once
	close    func() error
	closeErr error
}

func (closer *onceCloser) Close() error {
	closer.once.Do(func() { closer.closeErr = closer.close() })
	return closer.closeErr
}

type execCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir"`
	User         string   `json:"User"`
}

type execCreateResponse struct {
	ID string `json:"Id"`
}

type execStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

type execInspection struct {
	ID      string `json:"ID"`
	Running bool   `json:"Running"`
	Pid     int    `json:"Pid"`
}

type topResponse struct {
	Titles    []string   `json:"Titles"`
	Processes [][]string `json:"Processes"`
}

func newEngineClient(socket string) *engineClient {
	if socket == "" {
		socket = defaultDockerSocket
	}
	client := &engineClient{
		socket:       socket,
		apiVersion:   dockerAPIVersion,
		pollInterval: enginePollInterval,
		newID:        randomID,
		socketOps: engineSocketOps{
			listenUnix:      net.ListenUnix,
			chmod:           os.Chmod,
			remove:          os.Remove,
			lstat:           os.Lstat,
			closeListener:   func(listener *net.UnixListener) error { return listener.Close() },
			closeConnection: func(connection *net.UnixConn) error { return connection.Close() },
		},
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", client.socket)
		},
	}
	client.httpClient = &http.Client{Transport: transport}
	return client
}

// Exec starts the trusted helper as a detached Docker exec. The helper's Unix
// peer PID must exactly match the daemon-issued exec PID before any bytes are
// trusted. ExecInspect is used only to establish that identity; completion is
// reported by the helper protocol and confirmed with container top.
func (c *engineClient) Exec(ctx context.Context, containerID, runtimeDir string, command core.Command) (result core.CommandResult, launched bool, returnErr error) {
	started := time.Now()
	failed := func() core.CommandResult {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}
	}
	if len(command.Stdin) > maxExecInput {
		return failed(), false, fmt.Errorf("Docker exec stdin exceeds %d bytes", maxExecInput)
	}
	socketID, err := c.newID()
	if err != nil {
		return failed(), false, fmt.Errorf("generate Docker exec socket ID: %w", err)
	}
	socketName := "exec-" + socketID + ".sock"
	hostSocket := filepath.Join(runtimeDir, socketName)
	if err := c.socketOps.remove(hostSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failed(), false, fmt.Errorf("remove stale Docker exec socket: %w", err)
	}
	listener, err := c.socketOps.listenUnix("unix", &net.UnixAddr{Name: hostSocket, Net: "unix"})
	if err != nil {
		return failed(), false, fmt.Errorf("listen for Docker exec helper: %w", err)
	}
	var connectionCloser *onceCloser
	var stopContextClose func()
	defer func() {
		cleanupErr := c.cleanupExecSocket(hostSocket, listener, connectionCloser, stopContextClose)
		if cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	if err := c.socketOps.chmod(hostSocket, 0o600); err != nil {
		return failed(), false, fmt.Errorf("protect Docker exec socket: %w", err)
	}

	request := execCreateRequest{
		Cmd: append([]string{
			helperContainerPath,
			filepath.Join(socketContainerDir, socketName),
			command.Path,
		}, command.Args...),
		WorkingDir: command.Dir,
		User:       "0",
	}
	for _, key := range sortedKeys(command.Env) {
		request.Env = append(request.Env, key+"="+command.Env[key])
	}
	var created execCreateResponse
	createPath := "/" + c.apiVersion + "/containers/" + url.PathEscape(containerID) + "/exec"
	if err := c.doJSON(ctx, http.MethodPost, createPath, request, &created); err != nil {
		return failed(), false, fmt.Errorf("create Docker exec: %w", err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return failed(), false, errors.New("create Docker exec: daemon returned an empty exec ID")
	}

	startPath := "/" + c.apiVersion + "/exec/" + url.PathEscape(created.ID) + "/start"
	launched = true
	if err := c.doJSON(ctx, http.MethodPost, startPath, execStartRequest{Detach: true}, nil); err != nil {
		return failed(), true, fmt.Errorf("start Docker exec %q: %w", created.ID, err)
	}
	inspection, err := c.waitExecPID(ctx, created.ID)
	if err != nil {
		return failed(), true, err
	}
	connection, err := c.acceptExpectedPeer(ctx, listener, inspection.Pid)
	if err != nil {
		return failed(), true, fmt.Errorf("authenticate Docker exec helper %q: %w", created.ID, err)
	}
	connectionCloser = &onceCloser{close: func() error { return c.socketOps.closeConnection(connection) }}
	stopContextClose = closeOnContext(ctx, connectionCloser)
	if err := execproto.ReadHello(connection); err != nil {
		return failed(), true, fmt.Errorf("read Docker exec helper greeting: %w", contextOrError(ctx, err))
	}
	if err := execproto.WriteInput(connection, command.Stdin); err != nil {
		return failed(), true, fmt.Errorf("send Docker exec stdin: %w", contextOrError(ctx, err))
	}
	helperResult, err := execproto.ReadResult(connection, maxExecOutput)
	if err != nil {
		return failed(), true, fmt.Errorf("read Docker exec result: %w", contextOrError(ctx, err))
	}
	stopContextClose()
	stopContextClose = nil
	if err := connectionCloser.Close(); err != nil {
		connectionCloser = nil
		return failed(), true, fmt.Errorf("close Docker exec helper connection: %w", err)
	}
	connectionCloser = nil
	if err := c.waitPIDAbsent(ctx, containerID, inspection.Pid); err != nil {
		return failed(), true, fmt.Errorf("confirm Docker exec helper %d exited: %w", inspection.Pid, err)
	}
	return core.CommandResult{
		ExitCode: helperResult.ExitCode,
		Stdout:   string(helperResult.Stdout),
		Stderr:   string(helperResult.Stderr),
		Duration: time.Since(started),
	}, true, nil
}

func (c *engineClient) cleanupExecSocket(hostSocket string, listener *net.UnixListener, connection *onceCloser, stopContextClose func()) error {
	if stopContextClose != nil {
		stopContextClose()
	}
	var errs []error
	if connection != nil {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("close Docker exec helper connection: %w", err))
		}
	}
	if listener != nil {
		if err := c.socketOps.closeListener(listener); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("close Docker exec helper listener: %w", err))
		}
	}
	if err := c.socketOps.remove(hostSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("unlink Docker exec helper socket: %w", err))
	}
	if _, err := c.socketOps.lstat(hostSocket); err == nil {
		errs = append(errs, errors.New("confirm Docker exec helper socket absence: path still exists"))
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("confirm Docker exec helper socket absence: %w", err))
	}
	return errors.Join(errs...)
}

func contextOrError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return err
}

func (c *engineClient) waitExecPID(ctx context.Context, execID string) (execInspection, error) {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		inspection, err := c.inspectExec(ctx, execID)
		if err != nil {
			return execInspection{}, fmt.Errorf("inspect Docker exec %q: %w", execID, err)
		}
		if inspection.ID != execID {
			return execInspection{}, fmt.Errorf("inspect Docker exec: daemon returned ID %q, want %q", inspection.ID, execID)
		}
		if inspection.Running && inspection.Pid > 0 {
			return inspection, nil
		}
		select {
		case <-ctx.Done():
			return execInspection{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *engineClient) inspectExec(ctx context.Context, execID string) (execInspection, error) {
	var inspection execInspection
	inspectPath := "/" + c.apiVersion + "/exec/" + url.PathEscape(execID) + "/json"
	if err := c.doJSON(ctx, http.MethodGet, inspectPath, nil, &inspection); err != nil {
		return execInspection{}, err
	}
	return inspection, nil
}

func (c *engineClient) acceptExpectedPeer(ctx context.Context, listener *net.UnixListener, expectedPID int) (*net.UnixConn, error) {
	for rejected := 0; rejected <= maxRejectedPeers; {
		if err := listener.SetDeadline(time.Now().Add(c.pollInterval)); err != nil {
			return nil, err
		}
		connection, err := listener.AcceptUnix()
		if err != nil {
			var netError net.Error
			if errors.As(err, &netError) && netError.Timeout() {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
					continue
				}
			}
			return nil, err
		}
		pid, peerErr := unixPeerPID(connection)
		if peerErr == nil && pid == expectedPID {
			return connection, nil
		}
		_ = connection.Close()
		rejected++
	}
	return nil, fmt.Errorf("more than %d peers failed PID authentication", maxRejectedPeers)
}

func closeOnContext(ctx context.Context, closer io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = closer.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func (c *engineClient) waitPIDAbsent(ctx context.Context, containerID string, pid int) error {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		present, err := c.containerHasPID(ctx, containerID, pid)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *engineClient) containerHasPID(ctx context.Context, containerID string, pid int) (bool, error) {
	var top topResponse
	topPath := "/" + c.apiVersion + "/containers/" + url.PathEscape(containerID) + "/top?ps_args=" + url.QueryEscape("-eo pid")
	if err := c.doJSON(ctx, http.MethodGet, topPath, nil, &top); err != nil {
		return false, err
	}
	pidColumn := -1
	for index, title := range top.Titles {
		if strings.EqualFold(strings.TrimSpace(title), "PID") {
			pidColumn = index
			break
		}
	}
	if pidColumn < 0 {
		return false, errors.New("Docker top response has no PID column")
	}
	want := strconv.Itoa(pid)
	for _, process := range top.Processes {
		if pidColumn >= len(process) {
			return false, errors.New("Docker top process row is shorter than its titles")
		}
		if strings.TrimSpace(process[pidColumn]) == want {
			return true, nil
		}
	}
	return false, nil
}

func (c *engineClient) doJSON(ctx context.Context, method, requestPath string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	response, err := c.do(ctx, method, requestPath, body, "application/json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(response)
	}
	if output == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxEngineResponse))
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxEngineResponse))
	return decoder.Decode(output)
}

func (c *engineClient) do(ctx context.Context, method, requestPath string, body io.Reader, contentType string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+requestPath, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return c.httpClient.Do(request)
}

func responseError(response *http.Response) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, maxEngineResponse))
	return fmt.Errorf("daemon returned %s: %s", response.Status, strings.TrimSpace(string(message)))
}

var _ execEngine = (*engineClient)(nil)
