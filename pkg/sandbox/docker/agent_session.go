package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/internal/execsupervisor"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

type agentSession struct {
	mu          sync.Mutex
	started     chan struct{}
	finished    chan struct{}
	startCancel context.CancelFunc
	runCancel   context.CancelFunc
	rpc         *agentRPC
	err         error
	stage       string
	stageOwned  bool
	attempted   bool
	execID      string
}

// StartAgentSession establishes trusted process ownership before bridge access.
// A sandbox admits only one attempted agent session in its lifetime. Ordinary
// Exec and ExecStream retain independent benchmark preparation semantics.
func (s *Sandbox) StartAgentSession(ctx context.Context, supervisorPath string) error {
	startCtx, startCancel := context.WithTimeout(ctx, s.cleanupBudget())
	session := &agentSession{started: make(chan struct{}), finished: make(chan struct{}), startCancel: startCancel}
	s.mu.Lock()
	if s.agent != nil || s.owner == nil || !s.containerOwned || s.stopped || s.stopping {
		s.mu.Unlock()
		startCancel()
		return errors.New("agent supervision requires an unused owned live sandbox")
	}
	s.agent = session
	s.mu.Unlock()
	defer close(session.started)
	defer startCancel()
	if err := s.startAgentSession(startCtx, session, supervisorPath); err != nil {
		if !session.attempted && session.stageOwned {
			// No broker or agent was started. Rollback may still use the initial
			// task tools; this path is unreachable after agent access is granted.
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupBudget())
			result, cleanupErr := s.Exec(cleanup, core.Command{Path: "/bin/sh", Args: []string{"-c", `rm -rf -- "$1" && test ! -e "$1" && test ! -L "$1"`, "aries-agent-stage-rollback", session.stage}, User: rootExecUser})
			done()
			if cleanupErr != nil || result.ExitCode != 0 {
				session.record(errors.Join(cleanupErr, errors.New("agent stage rollback was not confirmed")))
			} else {
				session.stageOwned = false
			}
		}
		if session.attempted {
			session.abort(errors.New("agent supervisor startup was not confirmed"))
		}
		return errors.Join(err, session.failure())
	}
	return nil
}

func (s *Sandbox) startAgentSession(ctx context.Context, session *agentSession, source string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("agent supervisor must be a regular executable")
	}
	user, err := s.TaskUser(ctx)
	if err != nil {
		return err
	}
	var entropy [48]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return err
	}
	session.stage = "/.aries-agent-" + hex.EncodeToString(entropy[:16])
	nonce := hex.EncodeToString(entropy[16:])
	clear(entropy[:])
	result, err := s.Exec(ctx, core.Command{Path: "/bin/mkdir", Args: []string{"-m", "0755", "--", session.stage}, User: rootExecUser})
	if err != nil || result.ExitCode != 0 {
		return errors.Join(err, errors.New("create exclusive agent supervisor stage failed"))
	}
	session.stageOwned = true
	if err := s.Upload(ctx, source, session.stage+"/supervisor"); err != nil {
		return fmt.Errorf("stage agent supervisor: %w", err)
	}
	result, err = s.Exec(ctx, core.Command{Path: "/bin/chmod", Args: []string{"0555", "--", session.stage, session.stage + "/supervisor"}, User: rootExecUser})
	if err != nil || result.ExitCode != 0 {
		return errors.Join(err, errors.New("secure agent supervisor stage failed"))
	}
	if _, err := s.inspectSupervisedContainer(ctx); err != nil {
		return err
	}
	created, err := s.client.ExecCreate(ctx, s.containerID, client.ExecCreateOptions{AttachStdin: true, AttachStdout: true, AttachStderr: true, Cmd: []string{session.stage + "/supervisor", "--broker", "--stage-dir", session.stage, "--user", user}, WorkingDir: s.workdir, User: rootExecUser})
	if err != nil {
		return fmt.Errorf("create agent supervisor: %w", err)
	}
	if created.ID == "" {
		return errors.New("create agent supervisor returned no exec identity")
	}
	session.execID = created.ID
	session.attempted = true
	attached, err := s.client.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("attach agent supervisor: %w", err)
	}
	var closeOnce sync.Once
	closeAttach := func() { closeOnce.Do(attached.Close) }
	runCtx, runCancel := context.WithCancel(context.Background())
	session.mu.Lock()
	session.runCancel = runCancel
	alreadyAborted := session.err != nil
	session.mu.Unlock()
	if alreadyAborted {
		runCancel()
		closeAttach()
		return errors.New("sandbox stopped during agent startup")
	}
	output, writer := io.Pipe()
	proof := &agentProof{marker: []byte(execsupervisor.AgentProofPrefix + nonce + execsupervisor.AgentProofSuffix)}
	// The nonce precedes RPC and is never copied to a command, artifact, argv,
	// environment, or worker. A startup deadline closes a stalled attach.
	stopStartup := context.AfterFunc(ctx, func() { runCancel(); closeAttach() })
	defer stopStartup()
	if _, err := io.WriteString(attached.Conn, nonce+"\n"); err != nil {
		runCancel()
		closeAttach()
		_ = output.Close()
		_ = writer.Close()
		return errors.New("initialize agent supervisor private channel")
	}
	rpc := newAgentRPC(attached.Conn, output, func() { runCancel(); closeAttach(); _ = output.Close() })
	session.mu.Lock()
	session.rpc = rpc
	session.mu.Unlock()
	go s.runAgentSession(runCtx, session, attached, writer, proof, closeAttach)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-rpc.failed:
		return rpc.failure()
	case <-rpc.ready:
		if !stopStartup() {
			return errors.New("agent supervisor startup deadline closed its channel")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
}

func (s *Sandbox) runAgentSession(ctx context.Context, session *agentSession, attached client.ExecAttachResult, output *io.PipeWriter, proof *agentProof, closeAttach func()) {
	defer close(session.finished)
	defer session.runCancel()
	defer closeAttach()
	copyDone := make(chan error, 1)
	go func() { _, err := stdcopy.StdCopy(output, proof, attached.Reader); copyDone <- err }()
	exitDone := make(chan error, 1)
	go func() { exitDone <- s.waitForExecExit(ctx, session.execID) }()
	var copyErr, exitErr error
	copyFinished, exitWaited := false, false
waitExit:
	for !exitWaited {
		select {
		case <-ctx.Done():
			exitErr = ctx.Err()
			break waitExit
		case err := <-exitDone:
			exitErr = err
			exitWaited = true
		case err := <-copyDone:
			copyFinished = true
			copyErr = err
			if err != nil {
				exitErr = err
				break waitExit
			}
		}
	}
	cleanup, cancel := context.WithTimeout(context.Background(), s.cleanupBudget())
	defer cancel()
	forced := false
	if exitErr != nil {
		session.runCancel()
		closeAttach()
		_ = output.CloseWithError(exitErr)
	} else if !copyFinished {
		select {
		case copyErr = <-copyDone:
			copyFinished = true
		case <-time.After(execDrainTimeout):
			forced = true
			closeAttach()
			_ = output.Close()
		}
	}
	if !copyFinished {
		select {
		case copyErr = <-copyDone:
			copyFinished = true
		case <-cleanup.Done():
			session.record(errors.New("agent Docker output did not drain"))
		}
	}
	if !exitWaited {
		select {
		case <-exitDone:
			exitWaited = true
		case <-cleanup.Done():
			session.record(errors.New("agent Docker exit waiter did not drain"))
		}
	}
	closeAttach()
	// Stop waits with its own deadline. Never publish finished while a host
	// goroutine still owns the attach or an exec inspection, even if the bounded
	// cleanup above failed. Container destruction can still release those calls.
	if !copyFinished || !exitWaited {
		session.runCancel()
		_ = output.CloseWithError(context.DeadlineExceeded)
		if !copyFinished {
			<-copyDone
		}
		if !exitWaited {
			<-exitDone
		}
		return
	}
	if forced && (errors.Is(copyErr, net.ErrClosed) || errors.Is(copyErr, io.ErrClosedPipe)) {
		copyErr = nil
	}
	_ = output.CloseWithError(copyErr)
	if exitErr != nil || copyErr != nil {
		session.record(errors.Join(exitErr, copyErr))
		return
	}
	code, err := s.confirmSupervisedExit(cleanup, session.execID)
	if err != nil || code != 0 {
		session.record(errors.Join(err, fmt.Errorf("agent supervisor exited with status %d", code)))
		return
	}
	if err := proof.finish(); err != nil {
		session.record(err)
		return
	}
	session.stageOwned = false
}

// ExecAgentStream executes under the bridge's persistent process owner. A
// normal result does not retire background descendants. Input remains caller
// owned; callers must release a blocked reader when its SSH session ends.
func (s *Sandbox) ExecAgentStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	failure := func(err error) (core.CommandResult, error) { return core.CommandResult{ExitCode: -1}, err }
	if err := validateCommand(command); err != nil {
		return failure(err)
	}
	s.mu.Lock()
	session := s.agent
	live := s.containerOwned && !s.stopped && !s.stopping
	s.mu.Unlock()
	if session == nil || !live {
		return failure(errors.New("agent execution requires a started owned session"))
	}
	select {
	case <-session.started:
	case <-ctx.Done():
		return failure(ctx.Err())
	}
	session.mu.Lock()
	rpc, err := session.rpc, session.err
	session.mu.Unlock()
	if err != nil {
		return failure(err)
	}
	if rpc == nil {
		return failure(errors.New("agent execution session is not ready"))
	}
	if command.Dir == "" {
		command.Dir = s.workdir
	}
	return rpc.exec(ctx, command, stdin, stdout, stderr, s.cleanupBudget())
}

// StopAgentSession requires the broker's final proof and confirmed Docker exit.
// Every ambiguous cleanup failure remains latched; retries cannot open the
// evaluation gate after a failed confirmation.
func (s *Sandbox) StopAgentSession(ctx context.Context) error {
	s.mu.Lock()
	session := s.agent
	s.mu.Unlock()
	if session == nil {
		return nil
	}
	select {
	case <-session.started:
	case <-ctx.Done():
		session.abort(ctx.Err())
		return session.failure()
	}
	session.mu.Lock()
	rpc := session.rpc
	session.mu.Unlock()
	if rpc == nil {
		if session.attempted || session.stageOwned {
			session.record(errors.New("agent supervisor cleanup was not confirmed"))
		}
		return session.failure()
	}
	if err := rpc.revoke(ctx); err != nil {
		session.record(err)
	}
	select {
	case <-session.finished:
	case <-ctx.Done():
		session.abort(ctx.Err())
		return session.failure()
	}
	select {
	case <-rpc.drained:
	case <-ctx.Done():
		session.abort(ctx.Err())
		return session.failure()
	}
	rpc.mu.Lock()
	rpcErr := rpc.err
	rpc.mu.Unlock()
	session.record(rpcErr)
	return session.failure()
}

func (session *agentSession) record(err error) {
	if err == nil {
		return
	}
	session.mu.Lock()
	session.err = errors.Join(session.err, err)
	session.mu.Unlock()
}
func (session *agentSession) failure() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.err
}
func (session *agentSession) abort(err error) {
	session.record(err)
	session.startCancel()
	session.mu.Lock()
	cancel, rpc := session.runCancel, session.rpc
	session.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if rpc != nil {
		rpc.fail(err)
	}
}
func (s *Sandbox) cleanupBudget() time.Duration {
	if s.cleanupTimeout > 0 {
		return s.cleanupTimeout
	}
	return defaultCleanupTimeout
}

type agentProof struct{ marker, tail []byte }

func (proof *agentProof) Write(data []byte) (int, error) {
	if len(data) >= len(proof.marker) {
		proof.tail = bytes.Clone(data[len(data)-len(proof.marker):])
	} else {
		proof.tail = append(proof.tail, data...)
		if len(proof.tail) > len(proof.marker) {
			proof.tail = bytes.Clone(proof.tail[len(proof.tail)-len(proof.marker):])
		}
	}
	return len(data), nil
}
func (proof *agentProof) finish() error {
	if !bytes.Equal(proof.tail, proof.marker) {
		return errors.New("agent supervisor did not confirm descendant and stage cleanup")
	}
	clear(proof.tail)
	return nil
}

// Container destruction contains an unconfirmed broker; it must also release
// the host attach and routing goroutines. It never upgrades a failed bridge
// cleanup into permission to evaluate.
func (session *agentSession) abortForSandbox() {
	select {
	case <-session.started:
		session.mu.Lock()
		rpc := session.rpc
		session.mu.Unlock()
		if rpc == nil && !session.attempted && !session.stageOwned {
			return
		}
		if rpc != nil {
			select {
			case <-session.finished:
				select {
				case <-rpc.drained:
					return
				default:
				}
			default:
			}
		}
	default:
	}
	session.abort(errors.New("sandbox stopped before agent supervision drained"))
}

func (session *agentSession) waitHost(ctx context.Context) error {
	select {
	case <-session.started:
	case <-ctx.Done():
		return fmt.Errorf("drain agent startup: %w", ctx.Err())
	}
	session.mu.Lock()
	rpc := session.rpc
	session.mu.Unlock()
	if rpc == nil {
		return nil
	}
	select {
	case <-session.finished:
	case <-ctx.Done():
		return fmt.Errorf("drain agent Docker attach: %w", ctx.Err())
	}
	select {
	case <-rpc.drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("drain agent streams: %w", ctx.Err())
	}
}
