package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/internal/execsupervisor"
	"github.com/hyscale-lab/aries/pkg/core"
)

// agentRPC owns the host side of one protected broker. A stalled task stream
// gets its own pump; it cannot block decoding another command or revocation.
type agentRPC struct {
	mu         sync.Mutex
	calls      map[uint64]*agentCall
	next       uint64
	revoked    bool
	readySeen  bool
	err        error
	ready      chan struct{}
	failed     chan struct{}
	done       chan struct{}
	writerDone chan struct{}
	drained    chan struct{}
	outgoing   chan execsupervisor.Message
	admission  chan struct{}
	closeWire  func()
	closeOnce  sync.Once
	pumps      sync.WaitGroup
}

type agentCall struct {
	id           uint64
	notify       chan struct{}
	inputAck     chan error
	inputStop    chan struct{}
	stopInput    sync.Once
	output       [2]chan []byte
	outputAck    [2]bool
	outputEOF    [2]bool
	outputDone   [2]bool
	stdinWaiting bool
	result       *execsupervisor.Message
	retired      bool
	retireErr    error
	ioErr        error
	returned     bool
	canceling    bool
}

func newAgentRPC(input io.Writer, output io.Reader, closeWire func()) *agentRPC {
	rpc := &agentRPC{calls: make(map[uint64]*agentCall), ready: make(chan struct{}), failed: make(chan struct{}), done: make(chan struct{}), writerDone: make(chan struct{}), drained: make(chan struct{}), outgoing: make(chan execsupervisor.Message, 64), admission: make(chan struct{}, 1), closeWire: closeWire}
	rpc.admission <- struct{}{}
	go func() {
		defer close(rpc.writerDone)
		for {
			select {
			case <-rpc.failed:
				return
			case message := <-rpc.outgoing:
				if err := execsupervisor.WriteMessage(input, message); err != nil {
					select {
					case <-rpc.failed:
						return
					default:
					}
					rpc.fail(fmt.Errorf("write agent control: %w", err))
					return
				}
			}
		}
	}()
	go rpc.read(output)
	return rpc
}

func (rpc *agentRPC) read(reader io.Reader) {
	defer func() {
		rpc.mu.Lock()
		rpc.revoked = true
		for _, call := range rpc.calls {
			call.stopInput.Do(func() { close(call.inputStop) })
			call.signal()
		}
		rpc.mu.Unlock()
		rpc.fail(nil)
		close(rpc.done)
		// All additions to pumps share mu with revoked. Retry waits reuse this
		// single drain signal rather than creating another waiter goroutine.
		rpc.pumps.Wait()
		<-rpc.writerDone
		close(rpc.drained)
	}()
	for {
		message, err := execsupervisor.ReadMessage(reader)
		if err != nil {
			rpc.mu.Lock()
			revoked := rpc.revoked
			incomplete := false
			for _, call := range rpc.calls {
				if call.result == nil || !call.retired || !call.outputEOF[0] || !call.outputEOF[1] {
					incomplete = true
					break
				}
			}
			rpc.mu.Unlock()
			if !revoked || !errors.Is(err, io.EOF) {
				rpc.fail(fmt.Errorf("read agent control: %w", err))
			} else if incomplete {
				rpc.fail(errors.New("agent session ended without complete worker retirement"))
			}
			return
		}
		if err := rpc.receive(message); err != nil {
			rpc.fail(err)
			return
		}
	}
}

func (rpc *agentRPC) receive(message execsupervisor.Message) error {
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if message.Type == "ready" {
		if rpc.readySeen || message.ID != 0 || message.Version != execsupervisor.ProtocolVersion {
			return errors.New("invalid agent readiness")
		}
		rpc.readySeen = true
		close(rpc.ready)
		return nil
	}
	if !rpc.readySeen {
		return errors.New("agent message preceded readiness")
	}
	if message.Type == "fatal" {
		return errors.New("agent broker reported a supervision failure")
	}
	call := rpc.calls[message.ID]
	if call == nil {
		return errors.New("agent message has no admitted command")
	}
	switch message.Type {
	case "stdin_ack":
		if !call.stdinWaiting {
			return errors.New("unexpected agent stdin acknowledgement")
		}
		call.stdinWaiting = false
		var err error
		if message.Error != "" {
			err = errors.New("agent stdin closed before input completed")
		}
		select {
		case call.inputAck <- err:
		default:
			return errors.New("duplicate agent stdin acknowledgement")
		}
		if call.returned && call.retired {
			delete(rpc.calls, call.id)
		}
	case "stdout", "stderr", "stdout_eof", "stderr_eof":
		stream := 0
		if message.Type == "stderr" || message.Type == "stderr_eof" {
			stream = 1
		}
		eof := message.Type == "stdout_eof" || message.Type == "stderr_eof"
		if call.outputEOF[stream] || call.outputAck[stream] && (!eof || !call.canceling && !rpc.revoked) {
			return errors.New("agent output violated stream ordering")
		}
		var data []byte
		if eof {
			call.outputEOF[stream] = true
		} else {
			call.outputAck[stream] = true
			data = message.Data
		}
		select {
		case call.output[stream] <- data:
		default:
			return errors.New("agent output exceeded its in-flight bound")
		}
	case "exited":
		if call.result != nil || message.ExitCode == nil || *message.ExitCode < -1 || *message.ExitCode > 255 || *message.ExitCode == -1 && message.Error == "" {
			return errors.New("invalid agent foreground result")
		}
		call.result = &message
		call.stopInput.Do(func() { close(call.inputStop) })
	case "retired":
		if call.retired {
			return errors.New("duplicate agent worker retirement")
		}
		call.retired = true
		if message.Error != "" {
			call.retireErr = errors.New("agent worker cleanup failed")
			rpc.err = errors.Join(rpc.err, call.retireErr)
			return call.retireErr
		}
		if call.returned && !call.stdinWaiting {
			delete(rpc.calls, call.id)
		}
	default:
		return errors.New("unexpected agent control message")
	}
	call.signal()
	return nil
}

func (call *agentCall) signal() {
	select {
	case call.notify <- struct{}{}:
	default:
	}
}

func (rpc *agentRPC) failure() error {
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if rpc.err != nil {
		return rpc.err
	}
	return errors.New("agent session ended before command completion")
}

func (rpc *agentRPC) fail(err error) {
	rpc.mu.Lock()
	if err != nil {
		rpc.err = errors.Join(rpc.err, err)
	}
	rpc.revoked = true
	rpc.mu.Unlock()
	rpc.closeOnce.Do(func() {
		close(rpc.failed)
		rpc.closeWire()
	})
}

func (rpc *agentRPC) send(ctx context.Context, message execsupervisor.Message) error {
	select {
	case <-rpc.failed:
		return rpc.failure()
	default:
	}
	select {
	case rpc.outgoing <- message:
		return nil
	case <-rpc.failed:
		return rpc.failure()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rpc *agentRPC) revoke(ctx context.Context) error {
	// Serialize Stop with the first exec frame. An admitted command must be
	// enqueued before Stop, and a blocked queue must remain cancelable.
	select {
	case <-rpc.admission:
		defer func() { rpc.admission <- struct{}{} }()
	case <-ctx.Done():
		return ctx.Err()
	}
	rpc.mu.Lock()
	if rpc.revoked {
		err := rpc.err
		rpc.mu.Unlock()
		return err
	}
	rpc.revoked = true
	rpc.mu.Unlock()
	return rpc.send(ctx, execsupervisor.Message{Type: "stop"})
}

func (rpc *agentRPC) exec(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer, cleanup time.Duration) (core.CommandResult, error) {
	started := time.Now()
	result := core.CommandResult{ExitCode: -1}
	finish := func(err error) (core.CommandResult, error) { result.Duration = time.Since(started); return result, err }
	if err := ctx.Err(); err != nil {
		return finish(err)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	limit := command.OutputLimitBytes
	if limit == 0 {
		limit = maxExecOutput
	}
	select {
	case <-rpc.admission:
	case <-ctx.Done():
		return finish(ctx.Err())
	case <-rpc.failed:
		return finish(rpc.failure())
	}
	rpc.mu.Lock()
	if !rpc.readySeen || rpc.revoked || rpc.err != nil {
		rpc.mu.Unlock()
		rpc.admission <- struct{}{}
		return finish(rpc.failure())
	}
	rpc.next++
	// Cancellation may send EOF before the outstanding chunk is acknowledged.
	// Each stream holds at most that single chunk plus its terminal EOF marker.
	call := &agentCall{id: rpc.next, notify: make(chan struct{}, 1), inputAck: make(chan error, 1), inputStop: make(chan struct{}), output: [2]chan []byte{make(chan []byte, 2), make(chan []byte, 2)}}
	rpc.calls[call.id] = call
	rpc.pumps.Add(3)
	rpc.mu.Unlock()
	defer func() {
		call.stopInput.Do(func() { close(call.inputStop) })
		rpc.mu.Lock()
		call.returned = true
		if call.retired && !call.stdinWaiting {
			delete(rpc.calls, call.id)
		}
		rpc.mu.Unlock()
	}()
	spec := &execsupervisor.ExecSpec{Path: command.Path, Args: append([]string(nil), command.Args...), Dir: command.Dir, Env: maps.Clone(command.Env), User: command.User, TimeoutNS: int64(command.Timeout), OutputLimitBytes: limit}
	if err := rpc.send(ctx, execsupervisor.Message{Type: "exec", ID: call.id, Exec: spec}); err != nil {
		// No exec was enqueued and no pump has started. Cancel this admission
		// without terminating unrelated commands or retained background work.
		rpc.mu.Lock()
		delete(rpc.calls, call.id)
		rpc.mu.Unlock()
		rpc.pumps.Add(-3)
		rpc.admission <- struct{}{}
		return finish(err)
	}
	rpc.admission <- struct{}{}
	go rpc.outputPump(call, 0, &limitedWriter{writer: stdout, limit: limit})
	go rpc.outputPump(call, 1, &limitedWriter{writer: stderr, limit: limit})
	go rpc.inputPump(call, stdin)
	var timeout <-chan time.Time
	if command.Timeout > 0 {
		timer := time.NewTimer(command.Timeout)
		defer timer.Stop()
		timeout = timer.C
	}
	parentDone := ctx.Done()
	var cause error
	var cancelDone <-chan time.Time
	var cancelTimer *time.Timer
	defer func() {
		if cancelTimer != nil {
			cancelTimer.Stop()
		}
	}()
	beginCancel := func(err error) {
		if cause != nil {
			return
		}
		cause = err
		rpc.mu.Lock()
		call.canceling = true
		rpc.mu.Unlock()
		parentDone = nil
		timeout = nil
		if cleanup <= 0 {
			cleanup = defaultCleanupTimeout
		}
		cancelTimer = time.NewTimer(cleanup)
		cancelDone = cancelTimer.C
		cancelCtx, cancel := context.WithTimeout(context.Background(), cleanup)
		defer cancel()
		if err := rpc.send(cancelCtx, execsupervisor.Message{Type: "cancel", ID: call.id}); err != nil {
			rpc.fail(fmt.Errorf("cancel agent command: %w", err))
		}
	}
	for {
		rpc.mu.Lock()
		foreground := call.result
		streamsDone := call.outputDone[0] && call.outputDone[1]
		retired, retireErr, ioErr := call.retired, call.retireErr, call.ioErr
		sessionErr := rpc.err
		rpc.mu.Unlock()
		if sessionErr != nil {
			return finish(errors.Join(cause, sessionErr))
		}
		if ioErr != nil {
			beginCancel(ioErr)
		}
		if foreground != nil {
			timeout = nil
			result.ExitCode = *foreground.ExitCode
			if foreground.Error != "" && cause == nil {
				beginCancel(errors.New("agent command infrastructure failed"))
			}
			if streamsDone && cause == nil {
				return finish(nil)
			}
		}
		if cause != nil && foreground != nil && retired && streamsDone {
			return finish(errors.Join(cause, retireErr))
		}
		select {
		case <-call.notify:
		case <-parentDone:
			beginCancel(ctx.Err())
		case <-timeout:
			beginCancel(context.DeadlineExceeded)
		case <-cancelDone:
			err := errors.New("agent command completion and retirement were not confirmed")
			rpc.fail(err)
			return finish(errors.Join(cause, err))
		case <-rpc.failed:
			return finish(errors.Join(cause, rpc.failure()))
		}
	}
}

func (rpc *agentRPC) inputPump(call *agentCall, reader io.Reader) {
	defer rpc.pumps.Done()
	if reader == nil {
		_ = rpc.send(context.Background(), execsupervisor.Message{Type: "stdin_eof", ID: call.id})
		return
	}
	buffer := make([]byte, execsupervisor.MaxChunkBytes)
	total := 0
	for {
		select {
		case <-call.inputStop:
			return
		case <-rpc.failed:
			return
		default:
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			total += n
			if total > maxExecInput {
				rpc.inputError(call, fmt.Errorf("agent stdin exceeds %d bytes", maxExecInput))
				return
			}
			rpc.mu.Lock()
			finished := call.result != nil
			if !finished {
				call.stdinWaiting = true
			}
			rpc.mu.Unlock()
			if finished {
				return
			}
			data := append([]byte(nil), buffer[:n]...)
			if rpc.send(context.Background(), execsupervisor.Message{Type: "stdin", ID: call.id, Data: data}) != nil {
				return
			}
			select {
			case failure := <-call.inputAck:
				if failure != nil {
					rpc.inputError(call, failure)
					return
				}
			case <-call.inputStop:
				return
			case <-rpc.failed:
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = rpc.send(context.Background(), execsupervisor.Message{Type: "stdin_eof", ID: call.id})
			} else {
				rpc.inputError(call, err)
			}
			return
		}
	}
}

func (rpc *agentRPC) outputPump(call *agentCall, stream int, writer io.Writer) {
	defer rpc.pumps.Done()
	ack := "stdout_ack"
	if stream == 1 {
		ack = "stderr_ack"
	}
	discard := false
	for {
		select {
		case <-rpc.failed:
			return
		case data := <-call.output[stream]:
			if data == nil {
				rpc.mu.Lock()
				call.outputDone[stream] = true
				call.signal()
				rpc.mu.Unlock()
				return
			}
			if !discard {
				n, err := writer.Write(data)
				if err == nil && n != len(data) {
					err = io.ErrShortWrite
				}
				if err != nil {
					rpc.callError(call, err)
					discard = true
				}
			}
			rpc.mu.Lock()
			call.outputAck[stream] = false
			rpc.mu.Unlock()
			if rpc.send(context.Background(), execsupervisor.Message{Type: ack, ID: call.id}) != nil {
				return
			}
		}
	}
}

func (rpc *agentRPC) callError(call *agentCall, err error) {
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if call.ioErr == nil {
		call.ioErr = err
	}
	call.signal()
}

func (rpc *agentRPC) inputError(call *agentCall, err error) {
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if call.result == nil && call.ioErr == nil {
		call.ioErr = err
		call.signal()
	}
}
