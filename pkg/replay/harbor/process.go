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
	timing         *core.CommandTimings
}

// ProcessSessions owns live foreground processes that returned a native
// session handle.
type ProcessSessions struct {
	SendInterrupt    func(context.Context, replay.Call) error
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

// EndingProcessStates observes source handles without polling or consuming their output.
func (s *ProcessSessions) EndingProcessStates(ids []int) map[int]string {
	s.lock.Lock()
	defer s.lock.Unlock()
	if ids == nil {
		for id := range s.active {
			ids = append(ids, id)
		}
	}
	states := make(map[int]string, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		p := s.active[id]
		if p == nil {
			states[id] = "missing"
			continue
		}
		select {
		case <-p.done:
			states[id] = "terminal_retained"
			if p.err != nil {
				states[id] = "transport_error"
			}
		default:
			states[id] = "running"
		}
	}
	return states
}

func (s *ProcessSessions) Execute(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	return s.execute(ctx, call, false, false, false)
}

// ObserveDrift keeps a completed process visible to subsequent original empty
// polls. Its terminal status is real, and only newly buffered output is read.
func (s *ProcessSessions) ObserveDrift(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	return s.execute(ctx, call, true, false, false)
}

// ObserveRelaxed retains an unnamed live command long enough to record its
// actual terminal result if its original terminal reply now yields.
func (s *ProcessSessions) ObserveRelaxed(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	return s.execute(ctx, call, true, false, true)
}

func diagnosticProcessID(call replay.Call) int {
	return -int(call.Sequence)
}

func observedProcessPoll(call replay.Call) replay.Call {
	call.PipeInterrupt = false
	if call.Operation == "Exec" {
		call.SessionID = diagnosticProcessID(call)
		call.Operation = "PollProcess"
	}
	return call
}

func (s *ProcessSessions) FinishRelaxed(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	return s.FinishObservedProcess(ctx, observedProcessPoll(call))
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
	return s.execute(ctx, call, true, true, false)
}

// PollObservedProcess observes the same live command once after a source-terminal reply yielded.
func (s *ProcessSessions) PollObservedProcess(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	call = observedProcessPoll(call)
	if call.Operation != "PollProcess" || call.ExitCode == ProcessRunning {
		return core.CommandResult{}, errors.New("expected an original terminal process poll")
	}
	call.YieldTime = min(call.YieldTime, 5*time.Second)
	return s.ObserveDrift(ctx, call)
}

func (s *ProcessSessions) execute(ctx context.Context, call replay.Call, retain, join, trackUnnamed bool) (core.CommandResult, error) {
	unnamed := trackUnnamed && call.Operation == "Exec"
	key := call.SessionID
	if unnamed {
		key = diagnosticProcessID(call)
	}
	s.lock.Lock()
	p := s.active[key]
	var started <-chan time.Time
	switch call.Operation {
	case "Exec", "StartProcess":
		if (call.Operation == "StartProcess" || unnamed) && p != nil {
			s.lock.Unlock()
			return core.CommandResult{}, fmt.Errorf("duplicate live session %d", call.SessionID)
		}
		p = &guestProcess{done: make(chan struct{})}
		if call.Operation == "StartProcess" || unnamed {
			s.active[key] = p
		}
		work := s.ctx
		if s.nativeStartClock {
			work, p.timing = core.WithCommandTimings(work)
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
	if call.Operation == "PollProcess" && call.PipeInterrupt && !join {
		if s.SendInterrupt == nil {
			return core.CommandResult{}, errors.New("native pipe interrupt delivery is unavailable")
		}
		if err := s.SendInterrupt(ctx, call); err != nil {
			return core.CommandResult{}, err
		}
	}
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
				if p.err == nil {
					select {
					case <-started:
					default:
						return core.CommandResult{}, errors.New("native guest process completed without a Start acknowledgement")
					}
				}
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
		} else if call.Operation != "Exec" || unnamed {
			delete(s.active, key)
		}
		s.lock.Unlock()
	default:
	}
	if p.timing != nil {
		timing := p.timing.Snapshot()
		out.Timing = &timing
	}
	out.Stdout = p.stdout.take()
	out.Stderr = p.stderr.take()
	return out, err
}
