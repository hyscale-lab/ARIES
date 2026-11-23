package harbor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

// ProcessRunning is a protocol state for a live, yielded exec. It is never an
// operating-system process exit status.
const ProcessRunning = -1

type GuestStream func(context.Context, replay.Call, io.Writer, io.Writer) (core.CommandResult, error)

type pendingStream struct {
	lock sync.Mutex
	buf  bytes.Buffer
}

func (s *pendingStream) Write(p []byte) (int, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.buf.Write(p)
}

func (s *pendingStream) take() string {
	s.lock.Lock()
	defer s.lock.Unlock()
	chunk := s.buf.String()
	s.buf.Reset()
	return chunk
}

type guestProcess struct {
	stdout, stderr pendingStream
	done           chan struct{}
	result         core.CommandResult
	err            error
	retained       bool
}

// ProcessSessions owns live foreground processes that returned a native
// session handle. Only empty write_stdin polls are accepted by the parser.
type ProcessSessions struct {
	lock             sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	run              GuestStream
	active           map[int]*guestProcess
	wg               sync.WaitGroup
	nativeStartClock bool
}

func NewProcessSessions(ctx context.Context, run GuestStream) *ProcessSessions {
	ctx, cancel := context.WithCancel(ctx)
	return &ProcessSessions{ctx: ctx, cancel: cancel, run: run, active: map[int]*guestProcess{}}
}

// NewCodexProcessSessions uses Codex 0.151.0's initial yield clock with a
// provider that reports successful command starts through core.MarkCommandTiming.
func NewCodexProcessSessions(ctx context.Context, run GuestStream) *ProcessSessions {
	s := NewProcessSessions(ctx, run)
	s.nativeStartClock = true
	return s
}

const codexEarlyExitGrace = 150 * time.Millisecond

func (s *ProcessSessions) Close() { s.cancel(); s.wg.Wait() }

func (s *ProcessSessions) Execute(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	return s.execute(ctx, call, false, false)
}

// ObserveDrift keeps a completed process visible to subsequent original empty
// polls. Its terminal status is real, and only newly buffered output is read.
func (s *ProcessSessions) ObserveDrift(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	return s.execute(ctx, call, true, false)
}

func (s *ProcessSessions) WasRetained(id int) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	p := s.active[id]
	return p != nil && p.retained
}

// FinishObservedProcess joins an already-running command after an original
// terminal poll yielded. This extra diagnostic observation is recorded apart
// from the original reply, so the disagreement remains visible.
func (s *ProcessSessions) FinishObservedProcess(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	if call.Operation != "PollProcess" || call.ExitCode == ProcessRunning {
		return core.CommandResult{}, errors.New("expected an original terminal process poll")
	}
	return s.execute(ctx, call, true, true)
}

func (s *ProcessSessions) execute(ctx context.Context, call replay.Call, retain, join bool) (core.CommandResult, error) {
	s.lock.Lock()
	p := s.active[call.SessionID]
	var started <-chan time.Time
	switch call.Operation {
	case "Exec", "StartProcess":
		if call.Operation == "StartProcess" && p != nil {
			s.lock.Unlock()
			return core.CommandResult{}, fmt.Errorf("duplicate live session %d", call.SessionID)
		}
		p = &guestProcess{done: make(chan struct{})}
		if call.Operation == "StartProcess" {
			s.active[call.SessionID] = p
		}
		work := s.ctx
		if s.nativeStartClock {
			work, started = core.WithCommandStart(work)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			p.result, p.err = s.run(work, call, &p.stdout, &p.stderr)
			close(p.done)
		}()
	case "PollProcess":
		if p == nil {
			s.lock.Unlock()
			return core.CommandResult{}, fmt.Errorf("missing live session %d", call.SessionID)
		}
	default:
		s.lock.Unlock()
		return core.CommandResult{}, errors.New("unexpected process session operation")
	}
	s.lock.Unlock()
	var timeout <-chan time.Time
	if !join {
		wait := call.YieldTime
		if started != nil {
			select {
			case begin := <-started:
				// Both local and exec-server backends race an actual exit against
				// EARLY_EXIT_GRACE_PERIOD before the initial Codex yield window.
				// The command keeps executing throughout these two windows.
				wait = time.Until(begin.Add(codexEarlyExitGrace + call.YieldTime))
			case <-p.done:
			case <-ctx.Done():
				return core.CommandResult{}, context.Cause(ctx)
			}
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-ctx.Done():
		return core.CommandResult{}, context.Cause(ctx)
	case <-p.done:
	case <-timeout:
	}
	out := core.CommandResult{ExitCode: ProcessRunning}
	var err error
	select {
	case <-p.done:
		out = p.result
		err = p.err
		s.lock.Lock()
		if retain && call.ExitCode == ProcessRunning && err == nil {
			p.retained = true
		} else if call.Operation != "Exec" {
			delete(s.active, call.SessionID)
		}
		s.lock.Unlock()
	default:
	}
	out.Stdout = p.stdout.take()
	out.Stderr = p.stderr.take()
	return out, err
}
