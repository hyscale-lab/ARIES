package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/sandbox/docker/execproto"
)

type fakeEngineServer struct {
	t          *testing.T
	socket     string
	runtimeDir string
	server     *http.Server
	listener   net.Listener

	createStatus int
	startStatus  int
	helper       func(string)
	top          func(int) topResponse

	mu            sync.Mutex
	createRequest execCreateRequest
	startRequest  execStartRequest
	inspectCalls  int
	topCalls      int
	input         []byte
}

func newFakeEngineServer(t *testing.T) *fakeEngineServer {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("", "aries-engine-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	server := &fakeEngineServer{
		t:            t,
		socket:       filepath.Join(t.TempDir(), "docker.sock"),
		runtimeDir:   runtimeDir,
		createStatus: http.StatusCreated,
		startStatus:  http.StatusOK,
	}
	server.helper = func(socket string) {
		connection, err := net.Dial("unix", socket)
		if err != nil {
			t.Errorf("fake helper dial: %v", err)
			return
		}
		defer connection.Close()
		if err := execproto.WriteHello(connection); err != nil {
			t.Errorf("fake helper hello: %v", err)
			return
		}
		input, err := execproto.ReadInput(connection, maxExecInput)
		if err != nil {
			t.Errorf("fake helper input: %v", err)
			return
		}
		server.mu.Lock()
		server.input = input
		server.mu.Unlock()
		if err := execproto.WriteResult(connection, execproto.Result{ExitCode: 7, Stdout: []byte("stdout"), Stderr: []byte("stderr")}); err != nil {
			t.Errorf("fake helper result: %v", err)
		}
	}
	server.top = func(call int) topResponse {
		processes := [][]string{}
		if call == 0 {
			processes = [][]string{{"other", "1"}, {"helper", strconv.Itoa(os.Getpid())}}
		}
		return topResponse{Titles: []string{"CMD", "PID"}, Processes: processes}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+dockerAPIVersion+"/containers/container-id/exec", server.handleCreate)
	mux.HandleFunc("/"+dockerAPIVersion+"/exec/daemon-exec-id/start", server.handleStart)
	mux.HandleFunc("/"+dockerAPIVersion+"/exec/daemon-exec-id/json", server.handleInspect)
	mux.HandleFunc("/"+dockerAPIVersion+"/containers/container-id/top", server.handleTop)
	server.server = &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	listener, err := net.Listen("unix", server.socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("test sandbox forbids local Unix listeners: %v", err)
		}
		t.Fatal(err)
	}
	server.listener = listener
	go func() {
		if err := server.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("fake Docker Engine server: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = server.server.Close()
		_ = server.listener.Close()
		_ = os.Remove(server.socket)
	})
	return server
}

func (s *fakeEngineServer) handleCreate(writer http.ResponseWriter, request *http.Request) {
	var create execCreateRequest
	if err := json.NewDecoder(request.Body).Decode(&create); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.createRequest = create
	s.mu.Unlock()
	if s.createStatus < 200 || s.createStatus >= 300 {
		http.Error(writer, "create rejected", s.createStatus)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(s.createStatus)
	_ = json.NewEncoder(writer).Encode(execCreateResponse{ID: "daemon-exec-id"})
}

func (s *fakeEngineServer) handleStart(writer http.ResponseWriter, request *http.Request) {
	var start execStartRequest
	if err := json.NewDecoder(request.Body).Decode(&start); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.startRequest = start
	create := s.createRequest
	s.mu.Unlock()
	if s.startStatus < 200 || s.startStatus >= 300 {
		http.Error(writer, "start rejected", s.startStatus)
		return
	}
	writer.WriteHeader(s.startStatus)
	if s.helper != nil {
		hostSocket := filepath.Join(s.runtimeDir, filepath.Base(create.Cmd[1]))
		go s.helper(hostSocket)
	}
}

func (s *fakeEngineServer) handleInspect(writer http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.inspectCalls++
	s.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(execInspection{ID: "daemon-exec-id", Running: true, Pid: os.Getpid()})
}

func (s *fakeEngineServer) handleTop(writer http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	call := s.topCalls
	s.topCalls++
	top := s.top
	s.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(top(call))
}

func (s *fakeEngineServer) snapshot() (execCreateRequest, execStartRequest, []byte, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createRequest, s.startRequest, append([]byte(nil), s.input...), s.inspectCalls, s.topCalls
}

func fixedSocketPath(server *fakeEngineServer) string {
	return filepath.Join(server.runtimeDir, "exec-fixedid.sock")
}

func assertSocketAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exec socket %q still exists: %v", path, err)
	}
}

func TestEngineExecAuthenticatesHelperAndReturnsFramedResult(t *testing.T) {
	server := newFakeEngineServer(t)
	client := newEngineClient(server.socket)
	client.newID = func() (string, error) { return "fixedid", nil }
	result, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{
		Path:  "/bin/tool",
		Args:  []string{"one", "$(touch /bad)", "three"},
		Dir:   "/work",
		Env:   map[string]string{"ZED": "last", "ALPHA": "first"},
		Stdin: []byte("input"),
	})
	if err != nil || !launched {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
	if result.ExitCode != 7 || result.Stdout != "stdout" || result.Stderr != "stderr" || result.Duration <= 0 {
		t.Fatalf("Exec() = %#v", result)
	}
	create, start, input, inspectCalls, topCalls := server.snapshot()
	wantCommand := []string{helperContainerPath, socketContainerDir + "/exec-fixedid.sock", "/bin/tool", "one", "$(touch /bad)", "three"}
	if !reflect.DeepEqual(create.Cmd, wantCommand) || !reflect.DeepEqual(create.Env, []string{"ALPHA=first", "ZED=last"}) {
		t.Fatalf("create command/env = %#v / %#v", create.Cmd, create.Env)
	}
	if create.AttachStdin || create.AttachStdout || create.AttachStderr || create.Tty || create.WorkingDir != "/work" || create.User != "0" {
		t.Fatalf("create request = %#v", create)
	}
	if !start.Detach || start.Tty || string(input) != "input" || inspectCalls < 1 || topCalls < 2 {
		t.Fatalf("start=%#v input=%q inspect=%d top=%d", start, input, inspectCalls, topCalls)
	}
	assertSocketAbsent(t, fixedSocketPath(server))
}

func TestEngineExecRejectsWrongPIDPeer(t *testing.T) {
	server := newFakeEngineServer(t)
	validHelper := server.helper
	server.helper = func(socket string) {
		child := exec.Command(os.Args[0], "-test.run=TestEnginePeerProcess")
		child.Env = append(os.Environ(), "ARIES_TEST_PEER_SOCKET="+socket)
		if output, err := child.CombinedOutput(); err != nil {
			t.Errorf("wrong-PID peer: %v: %s", err, output)
		}
		validHelper(socket)
	}
	client := newEngineClient(server.socket)
	client.newID = func() (string, error) { return "fixedid", nil }
	result, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
	if err != nil || !launched || result.ExitCode != 7 {
		t.Fatalf("Exec() = %#v, launched=%v, error=%v", result, launched, err)
	}
}

func TestEnginePeerProcess(t *testing.T) {
	socket := os.Getenv("ARIES_TEST_PEER_SOCKET")
	if socket == "" {
		return
	}
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := execproto.WriteHello(connection); err != nil {
		t.Fatal(err)
	}
}

func TestEngineExecCancellationAfterLaunch(t *testing.T) {
	server := newFakeEngineServer(t)
	server.helper = nil
	client := newEngineClient(server.socket)
	client.newID = func() (string, error) { return "fixedid", nil }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, launched, err := client.Exec(ctx, "container-id", server.runtimeDir, core.Command{Path: "/bin/sleep", Args: []string{"10"}})
	if !launched || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
	assertSocketAbsent(t, fixedSocketPath(server))
}

func TestEngineExecCancellationWhileReadingResult(t *testing.T) {
	server := newFakeEngineServer(t)
	server.helper = func(socket string) {
		connection, err := net.Dial("unix", socket)
		if err != nil {
			t.Errorf("fake helper dial: %v", err)
			return
		}
		defer connection.Close()
		if err := execproto.WriteHello(connection); err != nil {
			t.Errorf("fake helper hello: %v", err)
			return
		}
		if _, err := execproto.ReadInput(connection, maxExecInput); err != nil {
			t.Errorf("fake helper input: %v", err)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	client := newEngineClient(server.socket)
	client.newID = func() (string, error) { return "fixedid", nil }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, launched, err := client.Exec(ctx, "container-id", server.runtimeDir, core.Command{Path: "/bin/sleep", Args: []string{"10"}})
	if !launched || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
	assertSocketAbsent(t, fixedSocketPath(server))
}

func TestEngineExecDistinguishesPreAndPostLaunchFailures(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		server := newFakeEngineServer(t)
		server.createStatus = http.StatusInternalServerError
		client := newEngineClient(server.socket)
		_, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
		if err == nil || launched {
			t.Fatalf("Exec() error = %v, launched=%v", err, launched)
		}
	})
	t.Run("start", func(t *testing.T) {
		server := newFakeEngineServer(t)
		server.startStatus = http.StatusInternalServerError
		client := newEngineClient(server.socket)
		_, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
		if err == nil || !launched || !strings.Contains(err.Error(), "start Docker exec") {
			t.Fatalf("Exec() error = %v, launched=%v", err, launched)
		}
	})
	t.Run("protocol", func(t *testing.T) {
		server := newFakeEngineServer(t)
		server.helper = func(socket string) {
			connection, err := net.Dial("unix", socket)
			if err == nil {
				_, _ = connection.Write([]byte("not-the-protocol"))
				_ = connection.Close()
			}
		}
		client := newEngineClient(server.socket)
		client.newID = func() (string, error) { return "fixedid", nil }
		_, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
		if err == nil || !launched || !strings.Contains(err.Error(), "greeting") {
			t.Fatalf("Exec() error = %v, launched=%v", err, launched)
		}
		assertSocketAbsent(t, fixedSocketPath(server))
	})
}

func TestEngineExecCleansSocketAfterHelperDeath(t *testing.T) {
	server := newFakeEngineServer(t)
	server.helper = func(socket string) {
		connection, err := net.Dial("unix", socket)
		if err != nil {
			t.Errorf("fake helper dial: %v", err)
			return
		}
		defer connection.Close()
		if err := execproto.WriteHello(connection); err != nil {
			t.Errorf("fake helper hello: %v", err)
			return
		}
		if _, err := execproto.ReadInput(connection, maxExecInput); err != nil {
			t.Errorf("fake helper input: %v", err)
		}
	}
	client := newEngineClient(server.socket)
	client.newID = func() (string, error) { return "fixedid", nil }
	_, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
	if err == nil || !launched || !strings.Contains(err.Error(), "read Docker exec result") {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
	assertSocketAbsent(t, fixedSocketPath(server))
}

func TestEngineExecSocketCleanupOrder(t *testing.T) {
	server := newFakeEngineServer(t)
	client := newEngineClient(server.socket)
	client.newID = func() (string, error) { return "fixedid", nil }
	defaultOps := client.socketOps
	var events []string
	removeCalls := 0
	client.socketOps.closeConnection = func(connection *net.UnixConn) error {
		events = append(events, "connection")
		return defaultOps.closeConnection(connection)
	}
	client.socketOps.closeListener = func(listener *net.UnixListener) error {
		events = append(events, "listener")
		return defaultOps.closeListener(listener)
	}
	client.socketOps.remove = func(path string) error {
		removeCalls++
		if removeCalls > 1 {
			events = append(events, "unlink")
		}
		return defaultOps.remove(path)
	}
	client.socketOps.lstat = func(path string) (os.FileInfo, error) {
		events = append(events, "lstat")
		return defaultOps.lstat(path)
	}
	_, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
	if err != nil || !launched {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
	if want := []string{"connection", "listener", "unlink", "lstat"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("cleanup events = %#v, want %#v", events, want)
	}
	assertSocketAbsent(t, fixedSocketPath(server))
}

func TestEngineExecCleanupFailuresRemainPostLaunchFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*engineClient, error)
	}{
		{
			name: "connection close",
			mutate: func(client *engineClient, injected error) {
				closeConnection := client.socketOps.closeConnection
				client.socketOps.closeConnection = func(connection *net.UnixConn) error {
					return errors.Join(closeConnection(connection), injected)
				}
			},
		},
		{
			name: "listener close",
			mutate: func(client *engineClient, injected error) {
				closeListener := client.socketOps.closeListener
				client.socketOps.closeListener = func(listener *net.UnixListener) error {
					return errors.Join(closeListener(listener), injected)
				}
			},
		},
		{
			name: "unlink",
			mutate: func(client *engineClient, injected error) {
				remove := client.socketOps.remove
				calls := 0
				client.socketOps.remove = func(path string) error {
					calls++
					_ = remove(path)
					if calls > 1 {
						return injected
					}
					return os.ErrNotExist
				}
			},
		},
		{
			name: "absence confirmation",
			mutate: func(client *engineClient, injected error) {
				client.socketOps.lstat = func(string) (os.FileInfo, error) { return nil, injected }
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newFakeEngineServer(t)
			client := newEngineClient(server.socket)
			client.newID = func() (string, error) { return "fixedid", nil }
			injected := errors.New("injected socket cleanup failure")
			test.mutate(client, injected)
			_, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
			if !launched || !errors.Is(err, injected) {
				t.Fatalf("Exec() error = %v, launched=%v", err, launched)
			}
			assertSocketAbsent(t, fixedSocketPath(server))
		})
	}
}

func TestEngineExecJoinsProtocolAndCleanupFailure(t *testing.T) {
	server := newFakeEngineServer(t)
	server.helper = func(socket string) {
		connection, err := net.Dial("unix", socket)
		if err == nil {
			_, _ = connection.Write([]byte("not-the-protocol"))
			_ = connection.Close()
		}
	}
	client := newEngineClient(server.socket)
	client.newID = func() (string, error) { return "fixedid", nil }
	injected := errors.New("injected listener cleanup failure")
	closeListener := client.socketOps.closeListener
	client.socketOps.closeListener = func(listener *net.UnixListener) error {
		return errors.Join(closeListener(listener), injected)
	}
	_, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
	if !launched || !errors.Is(err, injected) || !strings.Contains(err.Error(), "greeting") {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
	assertSocketAbsent(t, fixedSocketPath(server))
}

func TestEngineExecCleanupRejectsSocketReportedPresent(t *testing.T) {
	server := newFakeEngineServer(t)
	client := newEngineClient(server.socket)
	client.newID = func() (string, error) { return "fixedid", nil }
	info, err := os.Stat(server.runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	client.socketOps.lstat = func(string) (os.FileInfo, error) { return info, nil }
	_, launched, err := client.Exec(context.Background(), "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
	if !launched || err == nil || !strings.Contains(err.Error(), "path still exists") {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
	assertSocketAbsent(t, fixedSocketPath(server))
}

func TestEngineExecRequiresConfirmedPIDAbsence(t *testing.T) {
	server := newFakeEngineServer(t)
	server.top = func(int) topResponse {
		return topResponse{Titles: []string{"PID"}, Processes: [][]string{{strconv.Itoa(os.Getpid())}}}
	}
	client := newEngineClient(server.socket)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, launched, err := client.Exec(ctx, "container-id", server.runtimeDir, core.Command{Path: "/bin/true"})
	if !launched || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
}

func TestEngineExecRejectsOversizedInputBeforeDocker(t *testing.T) {
	client := newEngineClient(filepath.Join(t.TempDir(), "absent.sock"))
	_, launched, err := client.Exec(context.Background(), "container-id", t.TempDir(), core.Command{Path: "/bin/true", Stdin: make([]byte, maxExecInput+1)})
	if err == nil || launched || !strings.Contains(err.Error(), "stdin exceeds") {
		t.Fatalf("Exec() error = %v, launched=%v", err, launched)
	}
}
