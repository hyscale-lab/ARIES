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
}

// ProcessSessions owns live foreground processes that returned a native
// session handle. Only empty write_stdin polls are accepted by the parser.
type ProcessSessions struct {
	lock   sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	run    GuestStream
	active map[int]*guestProcess
	wg     sync.WaitGroup
}

func NewProcessSessions(ctx context.Context, run GuestStream) *ProcessSessions {
	ctx, cancel := context.WithCancel(ctx)
	return &ProcessSessions{ctx: ctx, cancel: cancel, run: run, active: map[int]*guestProcess{}}
}

func (s *ProcessSessions) Close() { s.cancel(); s.wg.Wait() }

func (s *ProcessSessions) Execute(ctx context.Context, call replay.Call) (core.CommandResult, error) {
	s.lock.Lock()
	p := s.active[call.SessionID]
	switch call.Operation {
	case "StartProcess":
		if p != nil {
			s.lock.Unlock()
			return core.CommandResult{}, fmt.Errorf("duplicate live session %d", call.SessionID)
		}
		p = &guestProcess{done: make(chan struct{})}
		s.active[call.SessionID] = p
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			p.result, p.err = s.run(s.ctx, call, &p.stdout, &p.stderr)
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
	timer := time.NewTimer(call.YieldTime)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return core.CommandResult{}, context.Cause(ctx)
	case <-p.done:
	case <-timer.C:
	}
	out := core.CommandResult{ExitCode: ProcessRunning}
	var err error
	select {
	case <-p.done:
		out = p.result
		err = p.err
		s.lock.Lock()
		delete(s.active, call.SessionID)
		s.lock.Unlock()
	default:
	}
	out.Stdout = p.stdout.take()
	out.Stderr = p.stderr.take()
	return out, err
}
