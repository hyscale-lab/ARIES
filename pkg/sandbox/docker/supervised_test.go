package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

type supervisedTestClient struct {
	*fakeClient
	closed         chan struct{}
	attached       chan struct{}
	staleRunning   bool
	wrongContainer bool
}

type supervisedTestConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *supervisedTestConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (f *supervisedTestClient) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	local, daemon := net.Pipe()
	connection := &supervisedTestConn{Conn: local, closed: f.closed}
	f.mu.Lock()
	handler := f.attach
	f.mu.Unlock()
	go func() {
		defer daemon.Close()
		if handler != nil {
			handler(daemon)
		}
		f.mu.Lock()
		f.execRunning = false
		f.mu.Unlock()
	}()
	close(f.attached)
	return client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(connection, "application/vnd.docker.multiplexed-stream")}, nil
}

func (f *supervisedTestClient) ExecInspect(ctx context.Context, id string, options client.ExecInspectOptions) (client.ExecInspectResult, error) {
	result, err := f.fakeClient.ExecInspect(ctx, id, options)
	if f.wrongContainer {
		result.ContainerID = "another-container"
	}
	if f.staleRunning {
		select {
		case <-f.closed:
		default:
			result.Running = true
		}
	}
	return result, err
}

func (f *supervisedTestClient) ContainerTop(ctx context.Context, id string, options client.ContainerTopOptions) (client.ContainerTopResult, error) {
	if f.staleRunning {
		return client.ContainerTopResult{Titles: []string{"PID"}}, nil
	}
	return f.fakeClient.ContainerTop(ctx, id, options)
}

func supervisedTestSandbox(t *testing.T) (*Sandbox, *supervisedTestClient) {
	t.Helper()
	base := &fakeClient{execRunning: true}
	sandbox := startSandbox(t, base)
	fake := &supervisedTestClient{fakeClient: base, closed: make(chan struct{}), attached: make(chan struct{})}
	sandbox.client = fake
	t.Cleanup(func() { _ = sandbox.stop(context.Background()) })
	return sandbox, fake
}

func TestExecSupervisedStreamUsesExactArgvAndNativeStreams(t *testing.T) {
	sandbox, fake := supervisedTestSandbox(t)
	fake.execExit = 7
	payload := "late\x00input\n"
	fake.attach = func(connection net.Conn) {
		input := make([]byte, len(payload))
		if _, err := io.ReadFull(connection, input); err != nil || string(input) != payload {
			return
		}
		writeFrame(connection, stdcopy.Stdout, []byte("native out"))
		writeFrame(connection, stdcopy.Stderr, []byte("native proof\x1f"))
	}
	command := core.Command{Path: "/.aries-codex-id/supervisor", Args: []string{"--codex", "/path with spaces/codex", "$(not-shell)", ""}, User: "0:0", Env: map[string]string{"CODEX_HOME": "/.aries-codex-id/home"}}
	var stdout, stderr bytes.Buffer
	result, err := sandbox.ExecSupervisedStream(context.Background(), command, strings.NewReader(payload), &stdout, &stderr)
	if err != nil || result.ExitCode != 7 || result.Duration <= 0 || stdout.String() != "native out" || stderr.String() != "native proof\x1f" {
		t.Fatalf("ExecSupervisedStream = %+v, %v; stdout %q stderr %q", result, err, stdout.String(), stderr.String())
	}
	fake.mu.Lock()
	options, creates := fake.execOptions, fake.execCreates
	fake.mu.Unlock()
	if creates != 1 || !reflect.DeepEqual(options.Cmd, append([]string{command.Path}, command.Args...)) || options.WorkingDir != "/work" || options.User != "0:0" || !options.AttachStdin || !options.AttachStdout || !options.AttachStderr || !reflect.DeepEqual(options.Env, []string{"CODEX_HOME=/.aries-codex-id/home"}) {
		t.Fatalf("direct exec options = %+v; creates %d", options, creates)
	}
}

func TestExecSupervisedStreamCancellationNeverStartsCleanupCommand(t *testing.T) {
	sandbox, fake := supervisedTestSandbox(t)
	fake.attach = func(net.Conn) { <-fake.closed }
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := sandbox.ExecSupervisedStream(ctx, core.Command{Path: "/supervisor"}, input, io.Discard, io.Discard)
		if result.ExitCode != -1 {
			err = errors.Join(err, errors.New("canceled supervisor returned a successful exit code"))
		}
		done <- err
	}()
	<-fake.attached
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for caller-owned stdin")
	}
	fake.mu.Lock()
	creates, control := fake.execCreates, fake.controlOptions.Cmd
	fake.mu.Unlock()
	if creates != 1 || len(control) != 0 {
		t.Fatalf("cancellation executed task commands: creates=%d control=%q", creates, control)
	}
	select {
	case <-fake.closed:
	default:
		t.Fatal("cancellation left Docker attach open")
	}
}

func TestExecSupervisedStreamClosesAttachBeforeDefinitiveInspection(t *testing.T) {
	sandbox, fake := supervisedTestSandbox(t)
	fake.staleRunning = true
	fake.execExit = 23
	fake.attach = func(connection net.Conn) { writeFrame(connection, stdcopy.Stdout, []byte("finished")) }
	var output bytes.Buffer
	result, err := sandbox.ExecSupervisedStream(context.Background(), core.Command{Path: "/supervisor"}, nil, &output, nil)
	if err != nil || result.ExitCode != 23 || output.String() != "finished" {
		t.Fatalf("stale-running result = %+v, %v; output %q", result, err, output.String())
	}
}

func TestExecSupervisedStreamRejectsForeignExecInspection(t *testing.T) {
	sandbox, fake := supervisedTestSandbox(t)
	fake.wrongContainer = true
	result, err := sandbox.ExecSupervisedStream(context.Background(), core.Command{Path: "/supervisor"}, nil, nil, nil)
	if err == nil || result.ExitCode != -1 {
		t.Fatalf("foreign exec result = %+v, %v", result, err)
	}
}

func TestExecSupervisedStreamHonorsOutputLimit(t *testing.T) {
	sandbox, fake := supervisedTestSandbox(t)
	fake.attach = func(connection net.Conn) { writeFrame(connection, stdcopy.Stdout, []byte("too much")) }
	result, err := sandbox.ExecSupervisedStream(context.Background(), core.Command{Path: "/supervisor", OutputLimitBytes: 3}, nil, io.Discard, nil)
	if err == nil || result.ExitCode != -1 {
		t.Fatalf("output limit result = %+v, %v", result, err)
	}
	fake.mu.Lock()
	creates := fake.execCreates
	fake.mu.Unlock()
	if creates != 1 {
		t.Fatalf("output failure started %d commands", creates)
	}
}

type supervisedZeroReader struct{}

func (supervisedZeroReader) Read(content []byte) (int, error) {
	clear(content)
	return len(content), nil
}

func TestExecSupervisedStreamReservesBoundedPrivateInputPrefix(t *testing.T) {
	for _, test := range []struct {
		name string
		size int64
		fail bool
	}{
		{"RPC plus nonce", maxExecInput + 65, false},
		{"over combined limit", maxExecInput + 1025, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sandbox, fake := supervisedTestSandbox(t)
			fake.attach = func(connection net.Conn) {
				if _, err := io.CopyN(io.Discard, connection, test.size); err == nil {
					writeFrame(connection, stdcopy.Stderr, []byte("finished"))
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := sandbox.ExecSupervisedStream(ctx, core.Command{Path: "/supervisor"}, io.LimitReader(supervisedZeroReader{}, test.size), nil, nil)
			if (err != nil) != test.fail || test.fail && result.ExitCode != -1 {
				t.Fatalf("input budget result = %+v, %v", result, err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("input budget failed only by timeout")
			}
		})
	}
}

func TestExecSupervisedStreamHonorsCommandTimeout(t *testing.T) {
	sandbox, fake := supervisedTestSandbox(t)
	fake.attach = func(net.Conn) { <-fake.closed }
	result, err := sandbox.ExecSupervisedStream(context.Background(), core.Command{Path: "/supervisor", Timeout: 20 * time.Millisecond}, nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != -1 {
		t.Fatalf("timeout result = %+v, %v", result, err)
	}
	fake.mu.Lock()
	creates := fake.execCreates
	fake.mu.Unlock()
	if creates != 1 {
		t.Fatalf("timeout started %d commands", creates)
	}
}

func TestSupervisedCapabilitiesRequireOwnedLiveContainer(t *testing.T) {
	for _, invalid := range []string{"unowned", "stopped", "wrong labels", "not running", "invalid command"} {
		t.Run(invalid, func(t *testing.T) {
			sandbox, fake := supervisedTestSandbox(t)
			command := core.Command{Path: "/supervisor"}
			switch invalid {
			case "unowned":
				sandbox.containerOwned = false
			case "stopped":
				sandbox.stopped = true
			case "wrong labels":
				fake.containerOpts.Config.Labels["aries.task"] = "another-task"
			case "not running":
				fake.containerLive = false
			case "invalid command":
				command.Args = []string{"has\x00nul"}
			}
			if _, err := sandbox.ExecSupervisedStream(context.Background(), command, nil, nil, nil); err == nil {
				t.Fatal("invalid supervised execution accepted")
			}
			if invalid != "invalid command" {
				if _, err := sandbox.TaskUser(context.Background()); err == nil {
					t.Fatal("invalid task-user ownership accepted")
				}
			}
			if fake.execCreates != 0 {
				t.Fatal("invalid execution reached Docker ExecCreate")
			}
		})
	}
}

func TestTaskUserResolvesDeclaredAndImageUsers(t *testing.T) {
	for _, test := range []struct{ name, declared, image, want string }{
		{"default root", "", "", "0:0"},
		{"image numeric", "", "1000:1001", "1000:1001"},
		{"image named", "", "app:appgroup", "app:appgroup"},
		{"declared overrides image", "65532:65532", "app", "65532:65532"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sandbox, fake := supervisedTestSandbox(t)
			sandbox.execUser = test.declared
			fake.containerOpts.Config.User = test.image
			fake.containerOpts.HostConfig.SecurityOpt = []string{"no-new-privileges=true"}
			got, err := sandbox.TaskUser(context.Background())
			if err != nil || got != test.want {
				t.Fatalf("TaskUser = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}
