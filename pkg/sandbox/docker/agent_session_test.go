package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/internal/execsupervisor"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

func TestAgentProofRequiresExactPrivateTerminalMarker(t *testing.T) {
	nonce := strings.Repeat("a", 64)
	marker := []byte(execsupervisor.AgentProofPrefix + nonce + execsupervisor.AgentProofSuffix)
	for _, test := range []struct {
		name  string
		body  []byte
		valid bool
	}{
		{"valid", append([]byte("private diagnostic\n"), marker...), true},
		{"missing", []byte("ordinary output"), false},
		{"different nonce", []byte(execsupervisor.AgentProofPrefix + strings.Repeat("b", 64) + execsupervisor.AgentProofSuffix), false},
		{"trailing bytes", append(append([]byte(nil), marker...), []byte("unconfirmed")...), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			proof := &agentProof{marker: marker}
			for _, value := range test.body {
				if _, err := proof.Write([]byte{value}); err != nil {
					t.Fatal(err)
				}
			}
			if err := proof.finish(); (err == nil) != test.valid {
				t.Fatalf("proof finish=%v valid=%v", err, test.valid)
			}
			if len(proof.tail) > len(marker) {
				t.Fatal("unbounded proof retention")
			}
		})
	}
	proof := &agentProof{marker: marker}
	_, _ = proof.Write(bytes.Repeat([]byte("x"), 1<<20))
	if len(proof.tail) != len(marker) {
		t.Fatalf("tail=%d", len(proof.tail))
	}
}

func TestAgentExecRequiresStartedSession(t *testing.T) {
	sandbox := &Sandbox{}
	result, err := sandbox.ExecAgentStream(context.Background(), core.Command{Path: "/bin/true"}, nil, nil, nil)
	if err == nil || result.ExitCode != -1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := sandbox.StopAgentSession(context.Background()); err != nil {
		t.Fatalf("stop unused session=%v", err)
	}
}

func TestAgentStopClosesStaleAttachWithoutInventingRPCFailure(t *testing.T) {
	fake := &fakeClient{containerID: "container-id", execRunning: true}
	sandbox, session, peer, marker := agentSessionFixture(t, fake, time.Second)
	served := make(chan error, 1)
	go func() {
		var ready bytes.Buffer
		_ = execsupervisor.WriteMessage(&ready, execsupervisor.Message{Type: "ready", Version: execsupervisor.ProtocolVersion})
		writeFrame(peer, stdcopy.Stdout, ready.Bytes())
		message, err := execsupervisor.ReadMessage(peer)
		if err != nil || message.Type != "stop" {
			served <- errors.Join(err, errors.New("broker did not receive stop"))
			return
		}
		writeFrame(peer, stdcopy.Stderr, marker)
		fake.mu.Lock()
		fake.execRunning = false
		fake.mu.Unlock()
		// Docker may leave attach open after process exit. Only the host's
		// close releases this stream after the valid terminal proof.
		_, err = io.Copy(io.Discard, peer)
		served <- err
	}()
	awaitAgentSessionSignal(t, session.rpc.ready, "broker readiness")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sandbox.StopAgentSession(ctx); err != nil {
		t.Fatalf("confirmed supervisor exit failed after closing stale attach: %v", err)
	}
	if session.stageOwned {
		t.Fatal("successful proof did not release stage ownership")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("host did not release Docker attach")
	}
}

func TestAgentCancellationWaitsForInspectAndSandboxStopKeepsFailedGate(t *testing.T) {
	fake := &agentHeldInspection{
		fakeClient: &fakeClient{containerID: "container-id", execRunning: true},
		inspecting: make(chan struct{}), canceled: make(chan struct{}), released: make(chan struct{}),
	}
	sandbox, session, _, _ := agentSessionFixture(t, fake, 20*time.Millisecond)
	t.Cleanup(fake.release)
	awaitAgentSessionSignal(t, fake.inspecting, "Docker exec inspection")
	stopCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sandbox.StopAgentSession(stopCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled StopAgentSession = %v", err)
	}
	awaitAgentSessionSignal(t, fake.canceled, "Docker inspection cancellation")
	awaitAgentSessionSignal(t, session.rpc.drained, "canceled RPC drain")
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelDrain()
	if err := session.waitHost(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("host drain accepted unfinished Docker inspection: %v", err)
	}
	select {
	case <-session.finished:
		t.Fatal("session finished before the Docker inspection returned")
	default:
	}
	cleanup, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
	defer cancelCleanup()
	// Destruction releases the stalled fake Docker call and joins all host
	// goroutines. It cannot supply the missing broker cleanup proof.
	if err := sandbox.stop(cleanup); err != nil {
		t.Fatalf("sandbox destruction did not drain host state: %v", err)
	}
	fake.mu.Lock()
	remaining := fake.containerID
	fake.mu.Unlock()
	if remaining != "" {
		t.Fatal("sandbox destruction left the container")
	}
	for range 2 {
		if err := sandbox.StopAgentSession(cleanup); !errors.Is(err, context.Canceled) {
			t.Fatalf("sandbox destruction erased the failed evaluation gate: %v", err)
		}
	}
}

func TestAgentPartialStartFailureDoesNotWaitForUnstartedRunner(t *testing.T) {
	failure := errors.New("attach was not confirmed")
	session := &agentSession{started: make(chan struct{}), finished: make(chan struct{}), attempted: true, stageOwned: true, err: failure}
	close(session.started)
	sandbox := &Sandbox{agent: session}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for range 2 {
		if err := sandbox.StopAgentSession(ctx); !errors.Is(err, failure) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("partial-start StopAgentSession = %v", err)
		}
	}
	if err := session.waitHost(ctx); err != nil {
		t.Fatalf("host drain waited for a runner that never started: %v", err)
	}
}

// Run the real Docker attach/RPC lifecycle over a local pipe. The fake only
// controls Docker inspection and removal; no broker or Docker daemon is needed.
func agentSessionFixture(t *testing.T, api dockerClient, cleanupTimeout time.Duration) (*Sandbox, *agentSession, net.Conn, []byte) {
	t.Helper()
	connection, peer := net.Pipe()
	attached := client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(connection, "application/vnd.docker.multiplexed-stream")}
	var closeOnce sync.Once
	closeAttach := func() { closeOnce.Do(attached.Close) }
	ctx, cancel := context.WithCancel(context.Background())
	output, writer := io.Pipe()
	marker := []byte(execsupervisor.AgentProofPrefix + strings.Repeat("a", 64) + execsupervisor.AgentProofSuffix)
	session := &agentSession{
		started: make(chan struct{}), finished: make(chan struct{}), startCancel: func() {}, runCancel: cancel,
		attempted: true, stageOwned: true, execID: "agent-exec",
	}
	session.rpc = newAgentRPC(connection, output, func() { cancel(); closeAttach(); _ = output.Close() })
	sandbox := &Sandbox{client: api, agent: session, containerID: "container-id", containerOwned: true, artifactDir: t.TempDir(), cleanupTimeout: cleanupTimeout}
	close(session.started)
	go sandbox.runAgentSession(ctx, session, attached, writer, &agentProof{marker: marker}, closeAttach)
	t.Cleanup(func() {
		session.abort(errors.New("test cleanup"))
		_ = peer.Close()
		_ = output.Close()
		_ = writer.Close()
		awaitAgentSessionSignal(t, session.finished, "Docker attach cleanup")
		awaitAgentSessionSignal(t, session.rpc.drained, "RPC cleanup")
	})
	return sandbox, session, peer, marker
}

func awaitAgentSessionSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out awaiting %s", operation)
	}
}

type agentHeldInspection struct {
	*fakeClient
	inspecting chan struct{}
	canceled   chan struct{}
	released   chan struct{}
	once       sync.Once
}

func (fake *agentHeldInspection) ExecInspect(ctx context.Context, _ string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	close(fake.inspecting)
	<-ctx.Done()
	close(fake.canceled)
	<-fake.released
	return client.ExecInspectResult{}, ctx.Err()
}

func (fake *agentHeldInspection) release() { fake.once.Do(func() { close(fake.released) }) }

func (fake *agentHeldInspection) ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	result, err := fake.fakeClient.ContainerRemove(ctx, id, options)
	fake.release()
	return result, err
}
