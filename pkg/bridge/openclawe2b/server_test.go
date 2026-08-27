package openclawe2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	arsandbox "github.com/hyscale-lab/aries/pkg/sandbox"
)

func TestUnauthenticatedRequestIsRejectedWithoutProtocolDispatch(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://bridge.invalid/process", nil)
	response := httptest.NewRecorder()
	newServer(nil).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

type fixedAddr string

func (address fixedAddr) Network() string { return "tcp" }
func (address fixedAddr) String() string  { return string(address) }

type blockingListener struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingListener() *blockingListener {
	return &blockingListener{closed: make(chan struct{})}
}

func (listener *blockingListener) Accept() (net.Conn, error) {
	<-listener.closed
	return nil, net.ErrClosed
}
func (listener *blockingListener) Close() error {
	listener.once.Do(func() { close(listener.closed) })
	return nil
}
func (*blockingListener) Addr() net.Addr { return fixedAddr("0.0.0.0:43123") }

func TestServerOwnsOneWildcardIPv4ListenerAndStopsIdempotently(t *testing.T) {
	listener := newBlockingListener()
	var network, address string
	server := newServer(func(_ context.Context, gotNetwork, gotAddress string) (net.Listener, error) {
		network, address = gotNetwork, gotAddress
		return listener, nil
	})
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if network != "tcp4" || address != "0.0.0.0:0" {
		t.Fatalf("listen network/address = %q %q", network, address)
	}
	if got, err := server.Address(); err != nil || got != "0.0.0.0:43123" {
		t.Fatalf("Address() = %q, %v", got, err)
	}
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("second Start succeeded")
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("idempotent Stop() = %v", err)
	}
	if _, err := server.Address(); err == nil {
		t.Fatal("Address succeeded after positive shutdown")
	}
}

func TestServerStartReturnsListenFailure(t *testing.T) {
	want := errors.New("listen failed")
	server := newServer(func(context.Context, string, string) (net.Listener, error) { return nil, want })
	if err := server.Start(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Start() error = %v", err)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() after failed Start = %v", err)
	}
}

type testSandbox struct {
	taskID    string
	network   string
	gateway   string
	gatewayFn func(context.Context) (string, error)
	process   func(context.Context, core.Command, io.Writer, io.Writer, func(arsandbox.ProcessRef) error) (core.CommandResult, error)
	signal    func(context.Context, arsandbox.ProcessRef, string) error
	terminate func(context.Context, arsandbox.ProcessRef) error
	readFile  func(context.Context, string) ([]byte, error)
	writeFile func(context.Context, string, []byte) error
	statPath  func(context.Context, string) (arsandbox.FileInfo, error)
	listDir   func(context.Context, string) ([]arsandbox.FileInfo, error)
	makeDir   func(context.Context, string) error
	remove    func(context.Context, string) error
	move      func(context.Context, string, string) error
}

func (*testSandbox) Exec(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, nil
}
func (*testSandbox) Upload(context.Context, string, string) error   { return nil }
func (*testSandbox) Download(context.Context, string, string) error { return nil }
func (sandbox *testSandbox) NetworkName() string                    { return sandbox.network }
func (sandbox *testSandbox) NetworkGateway(ctx context.Context) (string, error) {
	if sandbox.gatewayFn != nil {
		return sandbox.gatewayFn(ctx)
	}
	return sandbox.gateway, nil
}
func (sandbox *testSandbox) TaskID() string { return sandbox.taskID }
func (*testSandbox) Workdir() string        { return "/workspace" }
func (sandbox *testSandbox) ExecProcessStream(ctx context.Context, command core.Command, stdout, stderr io.Writer, onStart func(arsandbox.ProcessRef) error) (core.CommandResult, error) {
	if sandbox.process == nil {
		return core.CommandResult{ExitCode: -1}, errors.New("process fixture is not configured")
	}
	return sandbox.process(ctx, command, stdout, stderr, onStart)
}
func (sandbox *testSandbox) SendProcessSignal(ctx context.Context, ref arsandbox.ProcessRef, signal string) error {
	if sandbox.signal == nil {
		return errors.New("signal fixture is not configured")
	}
	return sandbox.signal(ctx, ref, signal)
}
func (sandbox *testSandbox) TerminateProcess(ctx context.Context, ref arsandbox.ProcessRef) error {
	if sandbox.terminate == nil {
		return errors.New("termination fixture is not configured")
	}
	return sandbox.terminate(ctx, ref)
}
func (sandbox *testSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if sandbox.readFile == nil {
		return nil, errors.New("read fixture is not configured")
	}
	return sandbox.readFile(ctx, path)
}
func (sandbox *testSandbox) WriteFile(ctx context.Context, path string, content []byte) error {
	if sandbox.writeFile == nil {
		return errors.New("write fixture is not configured")
	}
	return sandbox.writeFile(ctx, path, content)
}
func (sandbox *testSandbox) StatPath(ctx context.Context, path string) (arsandbox.FileInfo, error) {
	if sandbox.statPath == nil {
		return arsandbox.FileInfo{}, errors.New("stat fixture is not configured")
	}
	return sandbox.statPath(ctx, path)
}
func (sandbox *testSandbox) ListDir(ctx context.Context, path string) ([]arsandbox.FileInfo, error) {
	if sandbox.listDir == nil {
		return nil, errors.New("list fixture is not configured")
	}
	return sandbox.listDir(ctx, path)
}
func (sandbox *testSandbox) MakeDir(ctx context.Context, path string) error {
	if sandbox.makeDir == nil {
		return errors.New("mkdir fixture is not configured")
	}
	return sandbox.makeDir(ctx, path)
}
func (sandbox *testSandbox) RemovePath(ctx context.Context, path string) error {
	if sandbox.remove == nil {
		return errors.New("remove fixture is not configured")
	}
	return sandbox.remove(ctx, path)
}
func (sandbox *testSandbox) MovePath(ctx context.Context, source, destination string) error {
	if sandbox.move == nil {
		return errors.New("move fixture is not configured")
	}
	return sandbox.move(ctx, source, destination)
}

func startTestServer(t *testing.T) *Server {
	t.Helper()
	server := newServer(func(context.Context, string, string) (net.Listener, error) {
		return newBlockingListener(), nil
	})
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return server
}

func startGrant(t *testing.T, server *Server, sandbox *testSandbox) (*Grant, core.ToolEndpoint, string) {
	t.Helper()
	grant := server.NewGrant(t.TempDir())
	endpoint, err := grant.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := grant.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	token, err := os.ReadFile(endpoint.AccessTokenSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	return grant, endpoint, string(token)
}

func authorizedRequest(endpoint core.ToolEndpoint, token, destination string, body io.Reader) *http.Request {
	request := httptest.NewRequest(http.MethodPost, endpoint.Address+"/process", body)
	request.Header.Set(sandboxIDHeader, endpoint.SandboxID)
	request.Header.Set(accessTokenHeader, token)
	return request.WithContext(context.WithValue(request.Context(), localDestinationKey{}, fixedAddr(net.JoinHostPort(destination, "43123"))))
}

func requestStatus(server *Server, request *http.Request) int {
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response.Code
}

func TestTaskGrantsShareListenerButHaveDistinctBoundCredentials(t *testing.T) {
	server := startTestServer(t)
	sandboxA := &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"}
	grantA, endpointA, tokenA := startGrant(t, server, sandboxA)
	_, endpointB, tokenB := startGrant(t, server, &testSandbox{taskID: "b", network: "net-b", gateway: "172.31.0.1"})
	if endpointA.Address != "http://172.30.0.1:43123" || endpointB.Address != "http://172.31.0.1:43123" {
		t.Fatalf("addresses = %q, %q", endpointA.Address, endpointB.Address)
	}
	if endpointA.Workdir != "/workspace" || endpointB.Workdir != "/workspace" {
		t.Fatalf("task workdirs = %q, %q", endpointA.Workdir, endpointB.Workdir)
	}
	if endpointA.SandboxID == endpointB.SandboxID || tokenA == tokenB || endpointA.AccessTokenFile != accessTokenContainerPath || endpointA.Protocol != "http" {
		t.Fatalf("grants are not distinct: A=%#v B=%#v", endpointA, endpointB)
	}
	encodedEndpoint, err := json.Marshal(endpointA)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedEndpoint), tokenA) {
		t.Fatalf("endpoint serialized token bytes: %s", encodedEndpoint)
	}
	if info, err := os.Stat(endpointA.AccessTokenSourceFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, %v", info, err)
	}
	server.mu.Lock()
	registeredSandbox := server.registrations[endpointA.SandboxID].sandbox
	server.mu.Unlock()
	if registeredSandbox != sandboxA {
		t.Fatal("registration did not retain exact sandbox capability")
	}
	if requestStatus(server, authorizedRequest(endpointA, tokenA, "172.30.0.1", nil)) != http.StatusNotImplemented || requestStatus(server, authorizedRequest(endpointB, tokenB, "172.31.0.1", nil)) != http.StatusNotImplemented {
		t.Fatal("valid grants were not admitted")
	}
	for name, request := range map[string]*http.Request{
		"cross sandbox": authorizedRequest(endpointB, tokenA, "172.31.0.1", nil),
		"wrong token":   authorizedRequest(endpointA, tokenB, "172.30.0.1", nil),
		"wrong gateway": authorizedRequest(endpointA, tokenA, "172.31.0.1", nil),
	} {
		if status := requestStatus(server, request); status != http.StatusUnauthorized {
			t.Fatalf("%s status = %d", name, status)
		}
	}
	missing := authorizedRequest(endpointA, tokenA, "172.30.0.1", nil)
	missing.Header.Del(accessTokenHeader)
	if status := requestStatus(server, missing); status != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", status)
	}
	unknown := authorizedRequest(endpointA, tokenA, "172.30.0.1", nil)
	unknown.Header.Set(sandboxIDHeader, strings.Repeat("0", len(endpointA.SandboxID)))
	if status := requestStatus(server, unknown); status != http.StatusUnauthorized {
		t.Fatalf("unknown sandbox status = %d", status)
	}
	if err := grantA.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpointA.AccessTokenSourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked token file still exists: %v", err)
	}
	if status := requestStatus(server, authorizedRequest(endpointA, tokenA, "172.30.0.1", nil)); status != http.StatusUnauthorized {
		t.Fatalf("revoked A status = %d", status)
	}
	if status := requestStatus(server, authorizedRequest(endpointB, tokenB, "172.31.0.1", nil)); status != http.StatusNotImplemented {
		t.Fatalf("B after A revocation status = %d", status)
	}
	if err := grantA.Stop(context.Background()); err != nil {
		t.Fatalf("repeated Stop = %v", err)
	}
}

type panicBody struct{}

func (panicBody) Read([]byte) (int, error) { panic("request body read before authorization") }
func (panicBody) Close() error             { return nil }

func TestAuthorizationRejectsBeforeReadingRequestBody(t *testing.T) {
	server := startTestServer(t)
	_, endpoint, _ := startGrant(t, server, &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"})
	request := authorizedRequest(endpoint, "wrong", "172.30.0.1", panicBody{})
	if status := requestStatus(server, request); status != http.StatusUnauthorized {
		t.Fatalf("status = %d", status)
	}
}

func processRequest(endpoint core.ToolEndpoint, token, destination, payload string) *http.Request {
	request := authorizedRequest(endpoint, token, destination, strings.NewReader(payload))
	request.URL.Path = "/v1/process/start"
	return request
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (recorder *flushRecorder) Flush() {
	recorder.flushes++
	recorder.ResponseRecorder.Flush()
}

func TestProcessStartStreamsOrderedBinaryEventsAndNonzeroExit(t *testing.T) {
	server := startTestServer(t)
	sandbox := &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"}
	sandbox.process = func(_ context.Context, command core.Command, stdout, stderr io.Writer, onStart func(arsandbox.ProcessRef) error) (core.CommandResult, error) {
		if command.Path != "/bin/bash" || !reflect.DeepEqual(command.Args, []string{"-lc", "git status"}) || command.Dir != "/workspace" || !reflect.DeepEqual(command.Env, map[string]string{"A": "1"}) {
			t.Fatalf("command = %#v", command)
		}
		if err := onStart(arsandbox.ProcessRef{PID: 123}); err != nil {
			return core.CommandResult{ExitCode: -1}, err
		}
		for _, chunk := range [][]byte{{0x00, 0xff}, []byte("out-2")} {
			if _, err := stdout.Write(chunk); err != nil {
				return core.CommandResult{ExitCode: -1}, err
			}
		}
		if _, err := stderr.Write([]byte{0xfe, 0x00}); err != nil {
			return core.CommandResult{ExitCode: -1}, err
		}
		return core.CommandResult{ExitCode: 7}, nil
	}
	_, endpoint, token := startGrant(t, server, sandbox)
	request := processRequest(endpoint, token, sandbox.gateway, `{"process":{"cmd":"/bin/bash","args":["-lc","git status"],"cwd":"/workspace","envs":{"A":"1"}}}`)
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n")
	if len(lines) != 5 || recorder.flushes != len(lines) {
		t.Fatalf("lines=%d flushes=%d body=%s", len(lines), recorder.flushes, recorder.Body.String())
	}
	var events []processEvent
	for _, line := range lines {
		var event processEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if events[0].Event.Start == nil || events[0].Event.Start.PID != 123 {
		t.Fatalf("start event = %#v", events[0])
	}
	if !bytes.Equal(events[1].Event.Data.Stdout, []byte{0x00, 0xff}) || !bytes.Equal(events[2].Event.Data.Stdout, []byte("out-2")) || !bytes.Equal(events[3].Event.Data.Stderr, []byte{0xfe, 0x00}) {
		t.Fatalf("data events = %#v", events[1:4])
	}
	if events[4].Event.End == nil || events[4].Event.End.ExitCode != 7 || events[4].Event.End.Error != nil {
		t.Fatalf("end event = %#v", events[4])
	}
}

func TestProcessStartMalformedAndPreStartFailuresAreStructured(t *testing.T) {
	server := startTestServer(t)
	_, endpoint, token := startGrant(t, server, &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"})
	for name, payload := range map[string]string{
		"malformed":       `{`,
		"missing process": `{}`,
		"missing command": `{"process":{"args":[]}}`,
		"unknown field":   `{"process":{"cmd":"/bin/true","unknown":true}}`,
		"trailing value":  `{"process":{"cmd":"/bin/true"}} {}`,
		"cannot start":    `{"process":{"cmd":"/bin/true","args":[],"cwd":"/workspace","envs":{}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.ServeHTTP(response, processRequest(endpoint, token, "172.30.0.1", payload))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Header().Get("Content-Type"), "application/json") || !strings.Contains(response.Body.String(), `"error"`) {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
		})
	}
	request := processRequest(endpoint, "wrong", "172.30.0.1", "")
	request.Body = panicBody{}
	if status := requestStatus(server, request); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized malformed status = %d", status)
	}
}

func TestProcessStartExecutionFailureAfterStartUsesEndEvent(t *testing.T) {
	server := startTestServer(t)
	want := errors.New("stream execution failed")
	sandbox := &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"}
	sandbox.process = func(_ context.Context, _ core.Command, _ io.Writer, _ io.Writer, onStart func(arsandbox.ProcessRef) error) (core.CommandResult, error) {
		if err := onStart(arsandbox.ProcessRef{PID: 456}); err != nil {
			return core.CommandResult{ExitCode: -1}, err
		}
		return core.CommandResult{ExitCode: -1}, want
	}
	_, endpoint, token := startGrant(t, server, sandbox)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, processRequest(endpoint, token, sandbox.gateway, `{"process":{"cmd":"/bin/false","args":[],"cwd":"/workspace","envs":{}}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(response.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("body=%s", response.Body.String())
	}
	var end processEvent
	if err := json.Unmarshal([]byte(lines[1]), &end); err != nil || end.Event.End == nil || end.Event.End.Error == nil || *end.Event.End.Error != want.Error() {
		t.Fatalf("end=%#v err=%v", end, err)
	}
}

func TestProcessStartRequestCancellationEndsAttachedExecution(t *testing.T) {
	server := startTestServer(t)
	started := make(chan struct{})
	sandbox := &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"}
	sandbox.process = func(ctx context.Context, _ core.Command, _ io.Writer, _ io.Writer, onStart func(arsandbox.ProcessRef) error) (core.CommandResult, error) {
		if err := onStart(arsandbox.ProcessRef{PID: 789}); err != nil {
			return core.CommandResult{ExitCode: -1}, err
		}
		close(started)
		<-ctx.Done()
		return core.CommandResult{ExitCode: -1}, ctx.Err()
	}
	_, endpoint, token := startGrant(t, server, sandbox)
	ctx, cancel := context.WithCancel(context.Background())
	request := processRequest(endpoint, token, sandbox.gateway, `{"process":{"cmd":"/bin/sleep","args":["60"],"cwd":"/workspace","envs":{}}}`)
	ctx = context.WithValue(ctx, localDestinationKey{}, fixedAddr(net.JoinHostPort(sandbox.gateway, "43123")))
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(response, request)
		close(done)
	}()
	<-started
	cancel()
	<-done
	lines := strings.Split(strings.TrimSpace(response.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("body=%s", response.Body.String())
	}
	var end processEvent
	if err := json.Unmarshal([]byte(lines[1]), &end); err != nil || end.Event.End == nil || end.Event.End.Error == nil || !strings.Contains(*end.Event.End.Error, context.Canceled.Error()) {
		t.Fatalf("end=%#v err=%v", end, err)
	}
}

func TestConcurrentProcessStartRequestsWithinAndAcrossSandboxes(t *testing.T) {
	server := startTestServer(t)
	newProcessSandbox := func(taskID, network, gateway string, pid int) *testSandbox {
		sandbox := &testSandbox{taskID: taskID, network: network, gateway: gateway}
		var sequence atomic.Int64
		sandbox.process = func(_ context.Context, _ core.Command, stdout, stderr io.Writer, onStart func(arsandbox.ProcessRef) error) (core.CommandResult, error) {
			processPID := pid + int(sequence.Add(1))
			if err := onStart(arsandbox.ProcessRef{PID: processPID}); err != nil {
				return core.CommandResult{ExitCode: -1}, err
			}
			_, _ = stdout.Write([]byte(taskID))
			_, _ = stderr.Write([]byte{byte(processPID)})
			return core.CommandResult{ExitCode: 0}, nil
		}
		return sandbox
	}
	sandboxA := newProcessSandbox("a", "net-a", "172.30.0.1", 101)
	sandboxB := newProcessSandbox("b", "net-b", "172.31.0.1", 202)
	_, endpointA, tokenA := startGrant(t, server, sandboxA)
	_, endpointB, tokenB := startGrant(t, server, sandboxB)
	type target struct {
		endpoint core.ToolEndpoint
		token    string
		gateway  string
	}
	targets := []target{{endpointA, tokenA, sandboxA.gateway}, {endpointB, tokenB, sandboxB.gateway}}
	const requests = 24
	var wait sync.WaitGroup
	wait.Add(requests)
	for index := 0; index < requests; index++ {
		go func(index int) {
			defer wait.Done()
			target := targets[index%len(targets)]
			response := httptest.NewRecorder()
			server.ServeHTTP(response, processRequest(target.endpoint, target.token, target.gateway, `{"process":{"cmd":"/bin/true","args":[],"cwd":"/workspace","envs":{}}}`))
			if response.Code != http.StatusOK {
				t.Errorf("request %d status=%d body=%s", index, response.Code, response.Body.String())
			}
		}(index)
	}
	wait.Wait()
}

func signalRequest(endpoint core.ToolEndpoint, token, destination string, pid int, signal string, body io.Reader) *http.Request {
	if body == nil {
		body = strings.NewReader(fmt.Sprintf(`{"process":{"pid":%d},"signal":%q}`, pid, signal))
	}
	request := authorizedRequest(endpoint, token, destination, body)
	request.URL.Path = "/v1/process/send-signal"
	return request
}

type runningProcessFixture struct {
	pid        int
	started    chan struct{}
	finish     chan struct{}
	finishOnce sync.Once
	signals    chan string
	terminated chan struct{}
}

func newRunningProcessSandbox(taskID, network, gateway string, pid int) (*testSandbox, *runningProcessFixture) {
	fixture := &runningProcessFixture{
		pid: pid, started: make(chan struct{}), finish: make(chan struct{}), signals: make(chan string, 4), terminated: make(chan struct{}, 1),
	}
	sandbox := &testSandbox{taskID: taskID, network: network, gateway: gateway}
	sandbox.process = func(ctx context.Context, _ core.Command, _ io.Writer, _ io.Writer, onStart func(arsandbox.ProcessRef) error) (core.CommandResult, error) {
		if err := onStart(arsandbox.ProcessRef{PID: fixture.pid}); err != nil {
			return core.CommandResult{ExitCode: -1}, err
		}
		close(fixture.started)
		select {
		case <-fixture.finish:
			return core.CommandResult{ExitCode: 0}, nil
		case <-ctx.Done():
			return core.CommandResult{ExitCode: -1}, ctx.Err()
		}
	}
	sandbox.signal = func(_ context.Context, ref arsandbox.ProcessRef, signal string) error {
		if ref.PID != fixture.pid {
			return fmt.Errorf("wrong process ref PID %d", ref.PID)
		}
		fixture.signals <- signal
		return nil
	}
	sandbox.terminate = func(_ context.Context, ref arsandbox.ProcessRef) error {
		if ref.PID != fixture.pid {
			return fmt.Errorf("wrong termination ref PID %d", ref.PID)
		}
		fixture.terminated <- struct{}{}
		fixture.finishOnce.Do(func() { close(fixture.finish) })
		return nil
	}
	return sandbox, fixture
}

func startProcessRequest(t *testing.T, server *Server, endpoint core.ToolEndpoint, token, gateway string) chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, processRequest(endpoint, token, gateway, `{"process":{"cmd":"/bin/sleep","args":["60"],"cwd":"/workspace","envs":{}}}`))
		done <- response
	}()
	return done
}

func TestProcessSendSignalAuthenticatesBeforeBodyAndSupportsOnlyTermAndKill(t *testing.T) {
	server := startTestServer(t)
	sandbox, fixture := newRunningProcessSandbox("a", "net-a", "172.30.0.1", 123)
	_, endpoint, token := startGrant(t, server, sandbox)
	processDone := startProcessRequest(t, server, endpoint, token, sandbox.gateway)
	<-fixture.started

	unauthorized := signalRequest(endpoint, "wrong", sandbox.gateway, fixture.pid, "SIGNAL_SIGTERM", panicBody{})
	if status := requestStatus(server, unauthorized); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", status)
	}
	for _, signal := range []string{"SIGNAL_SIGTERM", "SIGNAL_SIGKILL"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, signalRequest(endpoint, token, sandbox.gateway, fixture.pid, signal, nil))
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"ok":true}` {
			t.Fatalf("%s status=%d body=%s", signal, response.Code, response.Body.String())
		}
		if got := <-fixture.signals; got != signal {
			t.Fatalf("signal = %q", got)
		}
	}
	for name, request := range map[string]*http.Request{
		"unsupported": signalRequest(endpoint, token, sandbox.gateway, fixture.pid, "SIGNAL_SIGINT", nil),
		"unknown":     signalRequest(endpoint, token, sandbox.gateway, 999, "SIGNAL_SIGTERM", nil),
	} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if name == "unsupported" && response.Code != http.StatusBadRequest || name == "unknown" && response.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	fixture.finishOnce.Do(func() { close(fixture.finish) })
	<-processDone
	response := httptest.NewRecorder()
	server.ServeHTTP(response, signalRequest(endpoint, token, sandbox.gateway, fixture.pid, "SIGNAL_SIGTERM", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("exited PID status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestProcessIdentityIsSandboxScopedAndRemovalIsGenerationSafe(t *testing.T) {
	server := startTestServer(t)
	sandboxA, fixtureA := newRunningProcessSandbox("a", "net-a", "172.30.0.1", 321)
	sandboxB, fixtureB := newRunningProcessSandbox("b", "net-b", "172.31.0.1", 321)
	_, endpointA, tokenA := startGrant(t, server, sandboxA)
	_, endpointB, tokenB := startGrant(t, server, sandboxB)
	doneA := startProcessRequest(t, server, endpointA, tokenA, sandboxA.gateway)
	doneB := startProcessRequest(t, server, endpointB, tokenB, sandboxB.gateway)
	<-fixtureA.started
	<-fixtureB.started

	response := httptest.NewRecorder()
	server.ServeHTTP(response, signalRequest(endpointB, tokenB, sandboxB.gateway, 321, "SIGNAL_SIGTERM", nil))
	if response.Code != http.StatusOK || <-fixtureB.signals != "SIGNAL_SIGTERM" {
		t.Fatalf("sandbox B signal status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case signal := <-fixtureA.signals:
		t.Fatalf("sandbox A received %q", signal)
	default:
	}

	key := processKey{sandboxID: endpointA.SandboxID, pid: 321}
	server.mu.Lock()
	old := server.processes[key]
	newer := &activeProcess{key: key, generation: old.generation + 1, registration: old.registration, sandbox: old.sandbox, ref: old.ref}
	server.processes[key] = newer
	server.mu.Unlock()
	server.removeProcess(old)
	server.mu.Lock()
	remaining := server.processes[key]
	server.mu.Unlock()
	if remaining != newer {
		t.Fatal("old completion removed reused PID generation")
	}
	server.removeProcess(newer)
	fixtureA.finishOnce.Do(func() { close(fixtureA.finish) })
	fixtureB.finishOnce.Do(func() { close(fixtureB.finish) })
	<-doneA
	<-doneB
}

func TestRevocationTerminatesOnlyItsProcessesBeforeDrain(t *testing.T) {
	server := startTestServer(t)
	sandboxA, fixtureA := newRunningProcessSandbox("a", "net-a", "172.30.0.1", 111)
	sandboxB, fixtureB := newRunningProcessSandbox("b", "net-b", "172.31.0.1", 222)
	grantA, endpointA, tokenA := startGrant(t, server, sandboxA)
	_, endpointB, tokenB := startGrant(t, server, sandboxB)
	doneA := startProcessRequest(t, server, endpointA, tokenA, sandboxA.gateway)
	doneB := startProcessRequest(t, server, endpointB, tokenB, sandboxB.gateway)
	<-fixtureA.started
	<-fixtureB.started
	if err := grantA.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixtureA.terminated:
	default:
		t.Fatal("revocation completed before terminating A")
	}
	select {
	case <-fixtureB.terminated:
		t.Fatal("revoking A terminated B")
	default:
	}
	if status := requestStatus(server, signalRequest(endpointA, tokenA, sandboxA.gateway, 111, "SIGNAL_SIGTERM", nil)); status != http.StatusUnauthorized {
		t.Fatalf("revoked A status = %d", status)
	}
	if status := requestStatus(server, signalRequest(endpointB, tokenB, sandboxB.gateway, 222, "SIGNAL_SIGTERM", nil)); status != http.StatusOK {
		t.Fatalf("live B status = %d", status)
	}
	<-doneA
	fixtureB.finishOnce.Do(func() { close(fixtureB.finish) })
	<-doneB
}

func TestProcessIsRegisteredBeforeStartCallbackReturns(t *testing.T) {
	server := startTestServer(t)
	sandbox := &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"}
	_, endpoint, token := startGrant(t, server, sandbox)
	sandbox.process = func(_ context.Context, _ core.Command, _ io.Writer, _ io.Writer, onStart func(arsandbox.ProcessRef) error) (core.CommandResult, error) {
		if err := onStart(arsandbox.ProcessRef{PID: 818}); err != nil {
			return core.CommandResult{ExitCode: -1}, err
		}
		server.mu.Lock()
		tracked := server.processes[processKey{sandboxID: endpoint.SandboxID, pid: 818}]
		server.mu.Unlock()
		if tracked == nil {
			return core.CommandResult{ExitCode: -1}, errors.New("child released before process registration")
		}
		return core.CommandResult{ExitCode: 0}, nil
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, processRequest(endpoint, token, sandbox.gateway, `{"process":{"cmd":"/bin/true"}}`))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "child released") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServerShutdownTerminatesAllActiveProcesses(t *testing.T) {
	server := startTestServer(t)
	sandboxA, fixtureA := newRunningProcessSandbox("a", "net-a", "172.30.0.1", 401)
	sandboxB, fixtureB := newRunningProcessSandbox("b", "net-b", "172.31.0.1", 402)
	_, endpointA, tokenA := startGrant(t, server, sandboxA)
	_, endpointB, tokenB := startGrant(t, server, sandboxB)
	doneA := startProcessRequest(t, server, endpointA, tokenA, sandboxA.gateway)
	doneB := startProcessRequest(t, server, endpointB, tokenB, sandboxB.gateway)
	<-fixtureA.started
	<-fixtureB.started
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixtureA.terminated:
	default:
		t.Fatal("server shutdown did not terminate A")
	}
	select {
	case <-fixtureB.terminated:
	default:
		t.Fatal("server shutdown did not terminate B")
	}
	<-doneA
	<-doneB
	server.mu.Lock()
	registrations, processes := len(server.registrations), len(server.processes)
	server.mu.Unlock()
	if registrations != 0 || processes != 0 {
		t.Fatalf("shutdown left registrations=%d processes=%d", registrations, processes)
	}
}

func TestConcurrentSignalCompletionAndRevocationFailsClosed(t *testing.T) {
	server := startTestServer(t)
	sandbox, fixture := newRunningProcessSandbox("a", "net-a", "172.30.0.1", 515)
	sandbox.signal = func(context.Context, arsandbox.ProcessRef, string) error { return nil }
	grant, endpoint, token := startGrant(t, server, sandbox)
	processDone := startProcessRequest(t, server, endpoint, token, sandbox.gateway)
	<-fixture.started

	const signals = 32
	var wait sync.WaitGroup
	wait.Add(signals + 1)
	for index := 0; index < signals; index++ {
		go func() {
			defer wait.Done()
			response := httptest.NewRecorder()
			server.ServeHTTP(response, signalRequest(endpoint, token, sandbox.gateway, fixture.pid, "SIGNAL_SIGTERM", nil))
			switch response.Code {
			case http.StatusOK, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict:
			default:
				t.Errorf("concurrent signal status=%d body=%s", response.Code, response.Body.String())
			}
		}()
	}
	go func() {
		defer wait.Done()
		if err := grant.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	wait.Wait()
	<-processDone
	server.mu.Lock()
	registrations, processes := len(server.registrations), len(server.processes)
	server.mu.Unlock()
	if registrations != 0 || processes != 0 {
		t.Fatalf("concurrent revocation left registrations=%d processes=%d", registrations, processes)
	}
}

func TestFailedProcessRevocationRemainsUnauthorizedAndRetryable(t *testing.T) {
	server := startTestServer(t)
	sandbox, fixture := newRunningProcessSandbox("a", "net-a", "172.30.0.1", 616)
	want := errors.New("termination confirmation failed")
	terminations := 0
	sandbox.terminate = func(context.Context, arsandbox.ProcessRef) error {
		terminations++
		fixture.finishOnce.Do(func() { close(fixture.finish) })
		if terminations == 1 {
			return want
		}
		return nil
	}
	grant, endpoint, token := startGrant(t, server, sandbox)
	processDone := startProcessRequest(t, server, endpoint, token, sandbox.gateway)
	<-fixture.started
	if err := grant.Stop(context.Background()); !errors.Is(err, want) {
		t.Fatalf("first Stop error=%v", err)
	}
	<-processDone
	if status := requestStatus(server, authorizedRequest(endpoint, token, sandbox.gateway, nil)); status != http.StatusUnauthorized {
		t.Fatalf("failed revocation restored authorization: status=%d", status)
	}
	server.mu.Lock()
	_, retained := server.registrations[endpoint.SandboxID]
	server.mu.Unlock()
	if !retained {
		t.Fatal("failed positive revocation discarded retry state")
	}
	if err := grant.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop=%v", err)
	}
	if terminations != 1 {
		t.Fatalf("completed process was terminated %d times", terminations)
	}
}

func TestFailedServerShutdownRetainsCleanupStateForRetry(t *testing.T) {
	listener := newBlockingListener()
	server := newServer(func(context.Context, string, string) (net.Listener, error) { return listener, nil })
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sandbox, fixture := newRunningProcessSandbox("a", "net-a", "172.30.0.1", 717)
	want := errors.New("termination confirmation failed")
	terminations := 0
	sandbox.terminate = func(context.Context, arsandbox.ProcessRef) error {
		terminations++
		fixture.finishOnce.Do(func() { close(fixture.finish) })
		if terminations == 1 {
			return want
		}
		return nil
	}
	grant, endpoint, token := startGrant(t, server, sandbox)
	processDone := startProcessRequest(t, server, endpoint, token, sandbox.gateway)
	<-fixture.started
	if err := server.Stop(context.Background()); !errors.Is(err, want) {
		t.Fatalf("first server Stop=%v", err)
	}
	<-processDone
	if status := requestStatus(server, authorizedRequest(endpoint, token, sandbox.gateway, nil)); status != http.StatusUnauthorized {
		t.Fatalf("failed shutdown restored authorization: status=%d", status)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("retry server Stop=%v", err)
	}
	server.mu.Lock()
	registrations, processes := len(server.registrations), len(server.processes)
	server.mu.Unlock()
	if registrations != 0 || processes != 0 {
		t.Fatalf("retry left registrations=%d processes=%d", registrations, processes)
	}
	if err := grant.Stop(context.Background()); err != nil {
		t.Fatalf("grant cleanup after server retry=%v", err)
	}
}

type memoryFilesystem struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
}

func newMemoryFilesystemSandbox(taskID, network, gateway string) (*testSandbox, *memoryFilesystem) {
	filesystem := &memoryFilesystem{files: make(map[string][]byte), dirs: map[string]bool{"/": true}}
	sandbox := &testSandbox{taskID: taskID, network: network, gateway: gateway}
	sandbox.readFile = func(_ context.Context, target string) ([]byte, error) {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		content, ok := filesystem.files[target]
		if !ok {
			return nil, os.ErrNotExist
		}
		return append([]byte(nil), content...), nil
	}
	sandbox.writeFile = func(_ context.Context, target string, content []byte) error {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		for parent := path.Dir(target); ; parent = path.Dir(parent) {
			filesystem.dirs[parent] = true
			if parent == "/" {
				break
			}
		}
		filesystem.files[target] = append([]byte(nil), content...)
		return nil
	}
	sandbox.statPath = func(_ context.Context, target string) (arsandbox.FileInfo, error) {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		if content, ok := filesystem.files[target]; ok {
			return arsandbox.FileInfo{Name: path.Base(target), Path: target, Type: "file", Size: int64(len(content)), Mode: 0o640, ModTime: time.Unix(123, 0)}, nil
		}
		if filesystem.dirs[target] {
			return arsandbox.FileInfo{Name: path.Base(target), Path: target, Type: "directory", Mode: os.ModeDir | 0o750, ModTime: time.Unix(124, 0)}, nil
		}
		return arsandbox.FileInfo{}, os.ErrNotExist
	}
	sandbox.listDir = func(_ context.Context, target string) ([]arsandbox.FileInfo, error) {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		if !filesystem.dirs[target] {
			return nil, os.ErrNotExist
		}
		entries := make([]arsandbox.FileInfo, 0)
		for directory := range filesystem.dirs {
			if directory != target && path.Dir(directory) == target {
				entries = append(entries, arsandbox.FileInfo{Name: path.Base(directory), Path: directory, Type: "directory", Mode: os.ModeDir | 0o755})
			}
		}
		for file, content := range filesystem.files {
			if path.Dir(file) == target {
				entries = append(entries, arsandbox.FileInfo{Name: path.Base(file), Path: file, Type: "file", Size: int64(len(content)), Mode: 0o644})
			}
		}
		slices.SortFunc(entries, func(left, right arsandbox.FileInfo) int { return strings.Compare(left.Name, right.Name) })
		return entries, nil
	}
	sandbox.makeDir = func(_ context.Context, target string) error {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		for current := target; ; current = path.Dir(current) {
			filesystem.dirs[current] = true
			if current == "/" {
				break
			}
		}
		return nil
	}
	sandbox.remove = func(_ context.Context, target string) error {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		delete(filesystem.files, target)
		for file := range filesystem.files {
			if strings.HasPrefix(file, target+"/") {
				delete(filesystem.files, file)
			}
		}
		for directory := range filesystem.dirs {
			if directory == target || strings.HasPrefix(directory, target+"/") {
				delete(filesystem.dirs, directory)
			}
		}
		return nil
	}
	sandbox.move = func(_ context.Context, source, destination string) error {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		if content, ok := filesystem.files[source]; ok {
			delete(filesystem.files, source)
			filesystem.files[destination] = content
			return nil
		}
		if !filesystem.dirs[source] {
			return os.ErrNotExist
		}
		for directory := range filesystem.dirs {
			if directory == source || strings.HasPrefix(directory, source+"/") {
				delete(filesystem.dirs, directory)
				filesystem.dirs[destination+strings.TrimPrefix(directory, source)] = true
			}
		}
		for file, content := range filesystem.files {
			if strings.HasPrefix(file, source+"/") {
				delete(filesystem.files, file)
				filesystem.files[destination+strings.TrimPrefix(file, source)] = content
			}
		}
		return nil
	}
	return sandbox, filesystem
}

func filesystemRequest(endpoint core.ToolEndpoint, token, destination, route, payload string) *http.Request {
	request := authorizedRequest(endpoint, token, destination, strings.NewReader(payload))
	request.URL.Path = route
	request.Header.Set("Content-Type", "application/json")
	return request
}

func rawFileRequest(endpoint core.ToolEndpoint, token, destination, method, target string, body io.Reader) *http.Request {
	request := authorizedRequest(endpoint, token, destination, body)
	request.Method = method
	request.URL.Path = "/v1/files"
	request.URL.RawQuery = "path=" + url.QueryEscape(target)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	return request
}

func TestRawFilesPreserveBinaryCreateParentsOverwriteAndZeroBytes(t *testing.T) {
	server := startTestServer(t)
	sandbox, filesystem := newMemoryFilesystemSandbox("a", "net-a", "172.30.0.1")
	_, endpoint, token := startGrant(t, server, sandbox)
	target := "/workspace/missing/bytes.bin"
	for _, content := range [][]byte{{0x00, 0xff, 'a'}, {}} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, rawFileRequest(endpoint, token, sandbox.gateway, http.MethodPost, target, bytes.NewReader(content)))
		if response.Code != http.StatusOK {
			t.Fatalf("write status=%d body=%s", response.Code, response.Body.String())
		}
		response = httptest.NewRecorder()
		server.ServeHTTP(response, rawFileRequest(endpoint, token, sandbox.gateway, http.MethodGet, target, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/octet-stream" || !bytes.Equal(response.Body.Bytes(), content) {
			t.Fatalf("read status=%d type=%q body=%v", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes())
		}
	}
	filesystem.mu.Lock()
	parents := filesystem.dirs["/workspace"] && filesystem.dirs["/workspace/missing"]
	filesystem.mu.Unlock()
	if !parents {
		t.Fatal("missing parents were not created")
	}
	request := rawFileRequest(endpoint, "wrong", sandbox.gateway, http.MethodPost, target, panicBody{})
	if status := requestStatus(server, request); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized raw write status=%d", status)
	}
}

func TestSemanticFilesystemStatListMkdirRemoveMoveAndValidation(t *testing.T) {
	server := startTestServer(t)
	sandbox, _ := newMemoryFilesystemSandbox("a", "net-a", "172.30.0.1")
	_, endpoint, token := startGrant(t, server, sandbox)
	call := func(route, payload string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, filesystemRequest(endpoint, token, sandbox.gateway, route, payload))
		return response
	}
	if response := call("/v1/filesystem/make-dir", `{"path":"/workspace/a/b"}`); response.Code != http.StatusOK {
		t.Fatalf("mkdir status=%d body=%s", response.Code, response.Body.String())
	}
	write := httptest.NewRecorder()
	server.ServeHTTP(write, rawFileRequest(endpoint, token, sandbox.gateway, http.MethodPost, "/workspace/a/file.bin", bytes.NewReader([]byte("abc"))))
	for target, wantType := range map[string]string{"/workspace/a/file.bin": "file", "/workspace/a": "directory"} {
		response := call("/v1/filesystem/stat", fmt.Sprintf(`{"path":%q}`, target))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"`+wantType+`"`) || !strings.Contains(response.Body.String(), `"mode":"`) || !strings.Contains(response.Body.String(), `"modifiedAt":`) {
			t.Fatalf("stat %s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	if response := call("/v1/filesystem/stat", `{"path":"/missing"}`); response.Code != http.StatusNotFound {
		t.Fatalf("missing stat status=%d body=%s", response.Code, response.Body.String())
	}
	response := call("/v1/filesystem/list-dir", `{"path":"/workspace/a"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"b"`) || !strings.Contains(response.Body.String(), `"name":"file.bin"`) || strings.Contains(response.Body.String(), "/workspace/a/b/") {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("/v1/filesystem/move", `{"source":"/workspace/a/file.bin","destination":"/workspace/a/renamed.bin"}`); response.Code != http.StatusOK {
		t.Fatalf("move status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("/v1/filesystem/stat", `{"path":"/workspace/a/renamed.bin"}`); response.Code != http.StatusOK {
		t.Fatalf("moved stat status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("/v1/filesystem/remove", `{"path":"/workspace/a"}`); response.Code != http.StatusOK {
		t.Fatalf("remove status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call("/v1/filesystem/stat", `{"path":"/workspace/a/renamed.bin"}`); response.Code != http.StatusNotFound {
		t.Fatalf("recursive remove left file: %s", response.Body.String())
	}
	for _, invalid := range []string{"/", "/.", "/workspace/..", "relative", ""} {
		response := call("/v1/filesystem/remove", fmt.Sprintf(`{"path":%q}`, invalid))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("remove %q status=%d body=%s", invalid, response.Code, response.Body.String())
		}
	}
	for _, payload := range []string{`{`, `{}`, `{"path":"/ok","extra":true}`, `{"path":"/ok"} {}`} {
		if response := call("/v1/filesystem/stat", payload); response.Code != http.StatusBadRequest {
			t.Fatalf("malformed %q status=%d body=%s", payload, response.Code, response.Body.String())
		}
	}
	unauthorized := filesystemRequest(endpoint, "wrong", sandbox.gateway, "/v1/filesystem/stat", "")
	unauthorized.Body = panicBody{}
	if status := requestStatus(server, unauthorized); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized semantic status=%d", status)
	}
}

func TestFilesystemOperationsAreSandboxScopedRevocableAndRaceClean(t *testing.T) {
	server := startTestServer(t)
	sandboxA, _ := newMemoryFilesystemSandbox("a", "net-a", "172.30.0.1")
	sandboxB, _ := newMemoryFilesystemSandbox("b", "net-b", "172.31.0.1")
	grantA, endpointA, tokenA := startGrant(t, server, sandboxA)
	_, endpointB, tokenB := startGrant(t, server, sandboxB)
	const operations = 32
	var wait sync.WaitGroup
	wait.Add(operations)
	for index := 0; index < operations; index++ {
		go func(index int) {
			defer wait.Done()
			endpoint, token, gateway := endpointA, tokenA, sandboxA.gateway
			if index%2 != 0 {
				endpoint, token, gateway = endpointB, tokenB, sandboxB.gateway
			}
			target := fmt.Sprintf("/workspace/file-%d", index)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, rawFileRequest(endpoint, token, gateway, http.MethodPost, target, bytes.NewReader([]byte{byte(index)})))
			if response.Code != http.StatusOK {
				t.Errorf("write %d status=%d", index, response.Code)
			}
		}(index)
	}
	wait.Wait()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, rawFileRequest(endpointB, tokenB, sandboxB.gateway, http.MethodGet, "/workspace/file-0", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("sandbox B read sandbox A file status=%d", response.Code)
	}
	if err := grantA.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := requestStatus(server, rawFileRequest(endpointA, tokenA, sandboxA.gateway, http.MethodGet, "/workspace/file-0", nil)); status != http.StatusUnauthorized {
		t.Fatalf("revoked sandbox status=%d", status)
	}
	if status := requestStatus(server, rawFileRequest(endpointB, tokenB, sandboxB.gateway, http.MethodGet, "/workspace/file-1", nil)); status != http.StatusOK {
		t.Fatalf("live sandbox status=%d", status)
	}
}

func TestRevocationInvalidatesImmediatelyAndDrainsAdmittedRequest(t *testing.T) {
	server := startTestServer(t)
	grant, endpoint, token := startGrant(t, server, &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"})
	release, ok := server.authorize(authorizedRequest(endpoint, token, "172.30.0.1", nil))
	if !ok {
		t.Fatal("request was not admitted")
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- grant.Stop(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for requestStatus(server, authorizedRequest(endpoint, token, "172.30.0.1", nil)) != http.StatusUnauthorized {
		if time.Now().After(deadline) {
			t.Fatal("token was not invalidated while admitted request drained")
		}
	}
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before admitted request released: %v", err)
	default:
	}
	release()
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
}

func TestFailedGrantStartRemovesPartialTokenArtifact(t *testing.T) {
	root := t.TempDir()
	grant := newServer(nil).NewGrant(root)
	if _, err := grant.Start(context.Background(), &testSandbox{taskID: "a", network: "net-a", gateway: "172.30.0.1"}); err == nil {
		t.Fatal("Start succeeded without a running server")
	}
	if matches, err := filepath.Glob(filepath.Join(root, "a", "bridge", "*.token")); err != nil || len(matches) != 0 {
		t.Fatalf("partial token artifacts = %v, %v", matches, err)
	}
	if err := grant.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after partial Start = %v", err)
	}
}

func TestGrantStopWaitsForConcurrentStartAndThenRevokes(t *testing.T) {
	server := startTestServer(t)
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	sandbox := &testSandbox{taskID: "a", network: "net-a", gatewayFn: func(context.Context) (string, error) {
		close(startEntered)
		<-releaseStart
		return "172.30.0.1", nil
	}}
	grant := server.NewGrant(t.TempDir())
	type startResult struct {
		endpoint core.ToolEndpoint
		err      error
	}
	started := make(chan startResult, 1)
	go func() {
		endpoint, err := grant.Start(context.Background(), sandbox)
		started <- startResult{endpoint: endpoint, err: err}
	}()
	<-startEntered
	stopped := make(chan error, 1)
	go func() { stopped <- grant.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned while Start was still publishing the grant: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseStart)
	result := <-started
	if result.err != nil {
		t.Fatal(result.err)
	}
	token, err := os.ReadFile(result.endpoint.AccessTokenSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if status := requestStatus(server, authorizedRequest(result.endpoint, string(token), "172.30.0.1", nil)); status != http.StatusUnauthorized {
		t.Fatalf("grant remains authorized after concurrent Stop: status=%d", status)
	}
	if _, err := os.Stat(result.endpoint.AccessTokenSourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("grant token remains after concurrent Stop: %v", err)
	}
}

func TestConcurrentGrantRegistrationAndRevocation(t *testing.T) {
	server := startTestServer(t)
	const count = 32
	root := t.TempDir()
	var wait sync.WaitGroup
	wait.Add(count)
	for index := 0; index < count; index++ {
		go func(index int) {
			defer wait.Done()
			grant := server.NewGrant(root)
			endpoint, err := grant.Start(context.Background(), &testSandbox{taskID: fmt.Sprintf("task-%d", index), network: "net", gateway: "172.30.0.1"})
			if err != nil {
				t.Error(err)
				return
			}
			token, err := os.ReadFile(endpoint.AccessTokenSourceFile)
			if err != nil {
				t.Error(err)
				return
			}
			if status := requestStatus(server, authorizedRequest(endpoint, string(token), "172.30.0.1", nil)); status != http.StatusNotImplemented {
				t.Errorf("status = %d", status)
			}
			if err := grant.Stop(context.Background()); err != nil {
				t.Error(err)
			}
		}(index)
	}
	wait.Wait()
	server.mu.Lock()
	remaining := len(server.registrations)
	server.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("remaining registrations = %d", remaining)
	}
}
