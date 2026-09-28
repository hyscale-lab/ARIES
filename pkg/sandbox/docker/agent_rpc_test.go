package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/internal/execsupervisor"
	"github.com/hyscale-lab/aries/pkg/core"
)

func TestAgentRPCPreservesCommandAndBinaryStreams(t *testing.T) {
	command := core.Command{Path: "/bin/tool", Args: []string{"", "two words", "$(literal)"}, Dir: "/work", User: "65532:65532", Env: map[string]string{"VALUE": "exact\nvalue"}, OutputLimitBytes: 1024}
	input := []byte("first\x00\xff\nlast")
	rpc, peer := agentRPCFixture(t)
	served := make(chan error, 1)
	go func() {
		request, err := execsupervisor.ReadMessage(peer)
		if err != nil {
			served <- err
			return
		}
		if request.Type != "exec" || request.Exec == nil || request.Exec.User != command.User || request.Exec.OutputLimitBytes != command.OutputLimitBytes || !reflect.DeepEqual(request.Exec.Args, command.Args) || !reflect.DeepEqual(request.Exec.Env, command.Env) {
			served <- errors.New("command metadata changed")
			return
		}
		var received []byte
		for {
			message, err := execsupervisor.ReadMessage(peer)
			if err != nil {
				served <- err
				return
			}
			if message.Type == "stdin_eof" {
				break
			}
			if message.Type != "stdin" {
				served <- errors.New("unexpected input message")
				return
			}
			received = append(received, message.Data...)
			if err := execsupervisor.WriteMessage(peer, execsupervisor.Message{Type: "stdin_ack", ID: request.ID}); err != nil {
				served <- err
				return
			}
		}
		if !bytes.Equal(received, input) {
			served <- errors.New("stdin changed")
			return
		}
		for _, stream := range []string{"stdout", "stderr"} {
			if err := execsupervisor.WriteMessage(peer, execsupervisor.Message{Type: stream, ID: request.ID, Data: input}); err != nil {
				served <- err
				return
			}
			ack, err := execsupervisor.ReadMessage(peer)
			if err != nil {
				served <- err
				return
			}
			if ack.Type != stream+"_ack" {
				served <- errors.New("missing output acknowledgement")
				return
			}
			if err := execsupervisor.WriteMessage(peer, execsupervisor.Message{Type: stream + "_eof", ID: request.ID}); err != nil {
				served <- err
				return
			}
		}
		code := 7
		if err := execsupervisor.WriteMessage(peer, execsupervisor.Message{Type: "exited", ID: request.ID, ExitCode: &code}); err != nil {
			served <- err
			return
		}
		served <- execsupervisor.WriteMessage(peer, execsupervisor.Message{Type: "retired", ID: request.ID})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out, diagnostic bytes.Buffer
	result, err := rpc.exec(ctx, command, bytes.NewReader(input), &out, &diagnostic, time.Second)
	if err != nil || result.ExitCode != 7 || !bytes.Equal(out.Bytes(), input) || !bytes.Equal(diagnostic.Bytes(), input) {
		t.Fatalf("result=%+v err=%v out=%q stderr=%q", result, err, out.Bytes(), diagnostic.Bytes())
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestAgentRPCCompletedCallRetainsBackgroundUntilStop(t *testing.T) {
	rpc, peer := agentRPCFixture(t)
	served := make(chan error, 1)
	go func() {
		execution, err := execsupervisor.ReadMessage(peer)
		if err != nil {
			served <- err
			return
		}
		input, err := execsupervisor.ReadMessage(peer)
		if err != nil || input.Type != "stdin_eof" {
			served <- errors.New("missing input EOF")
			return
		}
		code := 0
		for _, message := range []execsupervisor.Message{{Type: "exited", ID: execution.ID, ExitCode: &code}, {Type: "stdout_eof", ID: execution.ID}, {Type: "stderr_eof", ID: execution.ID}} {
			if err := execsupervisor.WriteMessage(peer, message); err != nil {
				served <- err
				return
			}
		}
		// A retained worker has not retired. A caller's deferred cancellation
		// after a successful result must not turn into a worker cancellation.
		message, err := execsupervisor.ReadMessage(peer)
		if err != nil {
			served <- err
			return
		}
		if message.Type != "stop" {
			served <- errors.New("completed command was canceled")
			return
		}
		served <- nil
	}()
	ctx, cancel := context.WithCancel(context.Background())
	result, err := rpc.exec(ctx, core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard, time.Second)
	cancel()
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := rpc.revoke(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestAgentRPCCancellationWaitsForWorkerRetirement(t *testing.T) {
	rpc, peer := agentRPCFixture(t)
	started, cancellation, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	go func() {
		execution, err := execsupervisor.ReadMessage(peer)
		if err != nil {
			return
		}
		close(started)
		for {
			message, err := execsupervisor.ReadMessage(peer)
			if err != nil {
				return
			}
			if message.Type == "cancel" {
				break
			}
		}
		close(cancellation)
		<-release
		code := 137
		for _, message := range []execsupervisor.Message{{Type: "exited", ID: execution.ID, ExitCode: &code}, {Type: "stdout_eof", ID: execution.ID}, {Type: "stderr_eof", ID: execution.ID}, {Type: "retired", ID: execution.ID}} {
			if execsupervisor.WriteMessage(peer, message) != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := rpc.exec(ctx, core.Command{Path: "/bin/sleep", Args: []string{"60"}}, nil, io.Discard, io.Discard, time.Second)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}
	cancel()
	select {
	case <-cancellation:
	case <-time.After(time.Second):
		t.Fatal("cancellation was not sent")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before retirement: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retired command did not finish")
	}
}

func TestAgentRPCBlockedStreamsDoNotBlockAnotherCallAndDrainHonestly(t *testing.T) {
	rpc, peer := agentRPCFixture(t)
	input, inputWriter := io.Pipe()
	defer input.Close()
	defer inputWriter.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	first := startAgentRPCCall(rpc, context.Background(), input, agentTestWriter(func(data []byte) (int, error) {
		close(entered)
		<-release
		return len(data), nil
	}))
	one := readAgentRPCMessage(t, peer)
	writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: "stdout", ID: one.ID, Data: []byte("blocked")})
	awaitAgentRPCSignal(t, entered, "first output was not written")
	second := startAgentRPCCall(rpc, context.Background(), nil, io.Discard)
	two := readAgentRPCMessage(t, peer)
	if two.Type != "exec" || two.ID == one.ID {
		t.Fatalf("second request = %+v", two)
	}
	if message := readAgentRPCMessage(t, peer); message.Type != "stdin_eof" || message.ID != two.ID {
		t.Fatalf("second stdin = %+v", message)
	}
	writeAgentRPCCompletion(t, peer, two.ID)
	if got := awaitAgentRPCResult(t, second); got.err != nil || got.result.ExitCode != 0 {
		t.Fatalf("unrelated call stalled or failed: %+v", got)
	}
	rpc.fail(errors.New("fixture revoked"))
	awaitAgentRPCSignal(t, rpc.done, "decoder did not stop")
	select {
	case <-rpc.drained:
		t.Fatal("drained claimed success while caller streams were blocked")
	default:
	}
	unblock()
	_ = input.Close()
	awaitAgentRPCSignal(t, rpc.drained, "released stream pumps did not drain")
	if got := awaitAgentRPCResult(t, first); got.err == nil {
		t.Fatal("aborted command reported success")
	}
}

func TestAgentRPCIgnoresLateInputErrorButPreservesLateOutputError(t *testing.T) {
	t.Run("input after foreground", func(t *testing.T) {
		rpc, peer := agentRPCFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		cause := errors.New("late input failure")
		finished := startAgentRPCCall(rpc, context.Background(), agentTestReader(func([]byte) (int, error) {
			close(entered)
			<-release
			return 0, cause
		}), io.Discard)
		command := readAgentRPCMessage(t, peer)
		awaitAgentRPCSignal(t, entered, "caller input did not block")
		writeAgentRPCCompletion(t, peer, command.ID)
		if got := awaitAgentRPCResult(t, finished); got.err != nil {
			t.Fatalf("normal completion = %+v", got)
		}
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := rpc.revoke(ctx); err != nil {
			t.Fatal(err)
		}
		if message := readAgentRPCMessage(t, peer); message.Type != "stop" {
			t.Fatalf("late input sent %+v", message)
		}
		_ = peer.Close()
		awaitAgentRPCSignal(t, rpc.drained, "late input pump survived Stop")
		rpc.mu.Lock()
		err := rpc.err
		rpc.mu.Unlock()
		if err != nil {
			t.Fatalf("late input poisoned supervision: %v", err)
		}
	})
	t.Run("output after foreground", func(t *testing.T) {
		rpc, peer := agentRPCFixture(t)
		cause := errors.New("output sink failed")
		finished := startAgentRPCCall(rpc, context.Background(), nil, agentTestWriter(func([]byte) (int, error) { return 0, cause }))
		command := readAgentRPCMessage(t, peer)
		_ = readAgentRPCMessage(t, peer)
		code := 0
		writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: "exited", ID: command.ID, ExitCode: &code})
		writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: "stdout", ID: command.ID, Data: []byte("lost output")})
		seen := map[string]bool{}
		for len(seen) != 2 {
			message := readAgentRPCMessage(t, peer)
			if message.ID != command.ID || message.Type != "cancel" && message.Type != "stdout_ack" {
				t.Fatalf("output failure sent %+v", message)
			}
			seen[message.Type] = true
		}
		for _, kind := range []string{"stdout_eof", "stderr_eof", "retired"} {
			writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: kind, ID: command.ID})
		}
		if got := awaitAgentRPCResult(t, finished); !errors.Is(got.err, cause) {
			t.Fatalf("late output failure was ignored: %+v", got)
		}
	})
}

func TestAgentRPCWorkerCleanupFailureStopsWholeSession(t *testing.T) {
	rpc, peer := agentRPCFixture(t)
	finished := startAgentRPCCall(rpc, context.Background(), nil, io.Discard)
	command := readAgentRPCMessage(t, peer)
	_ = readAgentRPCMessage(t, peer)
	code := 0
	writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: "exited", ID: command.ID, ExitCode: &code})
	writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: "retired", ID: command.ID, Error: "private diagnostic must be sanitized"})
	awaitAgentRPCSignal(t, rpc.failed, "worker cleanup failure did not stop admission and transport")
	if got := awaitAgentRPCResult(t, finished); got.err == nil {
		t.Fatal("failed worker cleanup reported success")
	}
	if _, err := rpc.exec(context.Background(), core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard, time.Second); err == nil {
		t.Fatal("session admitted a new command after failed cleanup")
	}
}

func TestAgentRPCCanceledOutputEOFAwaitsOutstandingWrite(t *testing.T) {
	for _, mode := range []string{"cancel", "stop"} {
		t.Run(mode, func(t *testing.T) {
			rpc, peer := agentRPCFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := startAgentRPCCall(rpc, ctx, nil, agentTestWriter(func(data []byte) (int, error) {
				close(entered)
				<-release
				return len(data), nil
			}))
			command := readAgentRPCMessage(t, peer)
			_ = readAgentRPCMessage(t, peer)
			writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: "stdout", ID: command.ID, Data: []byte("pending")})
			awaitAgentRPCSignal(t, entered, "output did not block")
			if mode == "cancel" {
				cancel()
			} else if err := rpc.revoke(context.Background()); err != nil {
				t.Fatal(err)
			}
			if message := readAgentRPCMessage(t, peer); message.Type != mode {
				t.Fatalf("shutdown request = %+v", message)
			}
			code := 137
			for _, message := range []execsupervisor.Message{{Type: "exited", ID: command.ID, ExitCode: &code}, {Type: "stdout_eof", ID: command.ID}, {Type: "stderr_eof", ID: command.ID}, {Type: "retired", ID: command.ID}} {
				writeAgentRPCMessage(t, peer, message)
			}
			select {
			case got := <-finished:
				t.Fatalf("call completed before outstanding output write: %+v", got)
			default:
			}
			unblock()
			if message := readAgentRPCMessage(t, peer); message.Type != "stdout_ack" || message.ID != command.ID {
				t.Fatalf("tail acknowledgement = %+v", message)
			}
			got := awaitAgentRPCResult(t, finished)
			if mode == "cancel" {
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("cancel = %+v", got)
				}
				if err := rpc.revoke(context.Background()); err != nil {
					t.Fatal(err)
				}
				if message := readAgentRPCMessage(t, peer); message.Type != "stop" {
					t.Fatalf("Stop = %+v", message)
				}
			} else if got.err != nil || got.result.ExitCode != 137 {
				t.Fatalf("stopped native result = %+v", got)
			}
			_ = peer.Close()
			awaitAgentRPCSignal(t, rpc.drained, "canceled output did not drain")
			rpc.mu.Lock()
			err := rpc.err
			rpc.mu.Unlock()
			if err != nil {
				t.Fatalf("expected cancellation poisoned session: %v", err)
			}
		})
	}
}

func TestAgentRPCCanceledAdmissionDoesNotPoisonSession(t *testing.T) {
	rpc, _ := agentRPCFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// One frame blocks the writer and the remaining frames fill its bounded
	// queue. A command canceled before enqueue has never reached the broker.
	for range cap(rpc.outgoing) + 1 {
		if err := rpc.send(ctx, execsupervisor.Message{Type: "stdin_eof", ID: 999}); err != nil {
			t.Fatal(err)
		}
	}
	admission, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	_, err := rpc.exec(admission, core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled admission = %v", err)
	}
	rpc.mu.Lock()
	poisoned := rpc.err != nil || rpc.revoked || len(rpc.calls) != 0
	rpc.mu.Unlock()
	if poisoned {
		t.Fatal("command canceled before enqueue poisoned the shared session")
	}
}

func TestAgentRPCNormalEOFCannotLeakBlockedControlWriter(t *testing.T) {
	output, peer := io.Pipe()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	closeWire := func() { once.Do(func() { close(release); _ = output.Close() }) }
	rpc := newAgentRPC(agentTestWriter(func([]byte) (int, error) {
		close(entered)
		<-release
		return 0, io.ErrClosedPipe
	}), output, closeWire)
	t.Cleanup(func() { rpc.fail(errors.New("test closed")); _ = peer.Close() })
	writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: "ready", Version: execsupervisor.ProtocolVersion})
	awaitAgentRPCSignal(t, rpc.ready, "broker did not become ready")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rpc.revoke(ctx); err != nil {
		t.Fatal(err)
	}
	awaitAgentRPCSignal(t, entered, "control writer did not block")
	_ = peer.Close()
	awaitAgentRPCSignal(t, rpc.done, "EOF did not finish decoder")
	awaitAgentRPCSignal(t, rpc.drained, "EOF leaked the control writer")
}

func TestAgentRPCMissingWorkerRetirementFailsClosedAtEOF(t *testing.T) {
	rpc, peer := agentRPCFixture(t)
	finished := startAgentRPCCall(rpc, context.Background(), nil, io.Discard)
	command := readAgentRPCMessage(t, peer)
	_ = readAgentRPCMessage(t, peer)
	code := 0
	for _, message := range []execsupervisor.Message{{Type: "exited", ID: command.ID, ExitCode: &code}, {Type: "stdout_eof", ID: command.ID}, {Type: "stderr_eof", ID: command.ID}} {
		writeAgentRPCMessage(t, peer, message)
	}
	if got := awaitAgentRPCResult(t, finished); got.err != nil {
		t.Fatal(got.err)
	}
	if err := rpc.revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if message := readAgentRPCMessage(t, peer); message.Type != "stop" {
		t.Fatalf("Stop sent %+v", message)
	}
	_ = peer.Close()
	awaitAgentRPCSignal(t, rpc.drained, "incomplete broker did not drain")
	rpc.mu.Lock()
	err := rpc.err
	rpc.mu.Unlock()
	if err == nil {
		t.Fatal("EOF without worker retirement allowed successful session cleanup")
	}
}

func TestAgentRPCRevokeNeverSendsExecAfterStop(t *testing.T) {
	for range 20 {
		rpc, peer := agentRPCFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		start, first, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
		serverDone := make(chan error, 1)
		go func() {
			seenStop := false
			var violation error
			for {
				message, err := execsupervisor.ReadMessage(peer)
				if err != nil {
					serverDone <- violation
					return
				}
				switch message.Type {
				case "exec":
					select {
					case <-first:
					default:
						close(first)
					}
					if seenStop {
						violation = errors.New("exec was sent after stop")
					}
					code := 0
					for _, reply := range []execsupervisor.Message{{Type: "exited", ID: message.ID, ExitCode: &code}, {Type: "stdout_eof", ID: message.ID}, {Type: "stderr_eof", ID: message.ID}, {Type: "retired", ID: message.ID}} {
						if err := execsupervisor.WriteMessage(peer, reply); err != nil {
							serverDone <- err
							return
						}
					}
				case "stop":
					seenStop = true
					close(stopped)
				}
			}
		}()
		var callers sync.WaitGroup
		for range 32 {
			callers.Add(1)
			go func() {
				defer callers.Done()
				<-start
				_, _ = rpc.exec(ctx, core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard, time.Second)
			}()
		}
		close(start)
		awaitAgentRPCSignal(t, first, "no command reached the broker before revocation")
		if err := rpc.revoke(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		callers.Wait()
		awaitAgentRPCSignal(t, stopped, "Stop was not sent")
		_ = peer.Close()
		if err := <-serverDone; err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		awaitAgentRPCSignal(t, rpc.drained, "racing admissions did not drain")
	}
}

func TestAgentRPCCancellationCannotHideMissingForegroundResult(t *testing.T) {
	rpc, peer := agentRPCFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan agentRPCResult, 1)
	go func() {
		result, err := rpc.exec(ctx, core.Command{Path: "/bin/tool"}, nil, io.Discard, io.Discard, 20*time.Millisecond)
		finished <- agentRPCResult{result, err}
	}()
	command := readAgentRPCMessage(t, peer)
	cancel()
	for {
		if message := readAgentRPCMessage(t, peer); message.Type == "cancel" {
			break
		}
	}
	for _, kind := range []string{"stdout_eof", "stderr_eof", "retired"} {
		writeAgentRPCMessage(t, peer, execsupervisor.Message{Type: kind, ID: command.ID})
	}
	got := awaitAgentRPCResult(t, finished)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancellation cause was lost: %v", got.err)
	}
	rpc.mu.Lock()
	latched := rpc.err
	rpc.mu.Unlock()
	if latched == nil {
		t.Fatalf("missing foreground result was treated as ordinary cancellation: %v", got.err)
	}
}

type agentRPCResult struct {
	result core.CommandResult
	err    error
}
type agentTestWriter func([]byte) (int, error)

func (write agentTestWriter) Write(data []byte) (int, error) { return write(data) }

type agentTestReader func([]byte) (int, error)

func (read agentTestReader) Read(data []byte) (int, error) { return read(data) }

func startAgentRPCCall(rpc *agentRPC, ctx context.Context, stdin io.Reader, stdout io.Writer) <-chan agentRPCResult {
	done := make(chan agentRPCResult, 1)
	go func() {
		result, err := rpc.exec(ctx, core.Command{Path: "/bin/tool"}, stdin, stdout, io.Discard, time.Second)
		done <- agentRPCResult{result, err}
	}()
	return done
}
func awaitAgentRPCResult(t *testing.T, done <-chan agentRPCResult) agentRPCResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("agent call did not return")
	}
	return agentRPCResult{}
}
func awaitAgentRPCSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}
func readAgentRPCMessage(t *testing.T, reader io.Reader) execsupervisor.Message {
	t.Helper()
	message, err := execsupervisor.ReadMessage(reader)
	if err != nil {
		t.Fatal(err)
	}
	return message
}
func writeAgentRPCMessage(t *testing.T, writer io.Writer, message execsupervisor.Message) {
	t.Helper()
	if err := execsupervisor.WriteMessage(writer, message); err != nil {
		t.Fatal(err)
	}
}
func writeAgentRPCCompletion(t *testing.T, peer io.Writer, id uint64) {
	t.Helper()
	code := 0
	for _, message := range []execsupervisor.Message{{Type: "exited", ID: id, ExitCode: &code}, {Type: "stdout_eof", ID: id}, {Type: "stderr_eof", ID: id}, {Type: "retired", ID: id}} {
		writeAgentRPCMessage(t, peer, message)
	}
}

func agentRPCFixture(t *testing.T) (*agentRPC, net.Conn) {
	t.Helper()
	host, peer := net.Pipe()
	rpc := newAgentRPC(host, host, func() { _ = host.Close() })
	t.Cleanup(func() {
		rpc.fail(errors.New("test closed"))
		_ = peer.Close()
		select {
		case <-rpc.done:
		case <-time.After(time.Second):
			t.Error("RPC decoder leaked")
		}
		select {
		case <-rpc.drained:
		case <-time.After(time.Second):
			t.Error("RPC stream pumps leaked")
		}
	})
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	go func() {
		_ = execsupervisor.WriteMessage(peer, execsupervisor.Message{Type: "ready", Version: execsupervisor.ProtocolVersion})
	}()
	select {
	case <-rpc.ready:
	case <-time.After(time.Second):
		t.Fatal("RPC was not ready")
	}
	return rpc, peer
}
