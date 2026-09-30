package bridgekit

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

// Session is the part of a bridge session every transport shares: its audit,
// its artifact directory, the handlers it waits for, and the errors that make
// revocation unconfirmed.
type Session struct {
	Audit       *Writer
	ArtifactDir string
	// Partial marks a session whose Start failed; Finish then removes the
	// artifact directory instead of retaining it.
	Partial bool
	// Wait counts the session's live handlers.
	Wait sync.WaitGroup

	revocationMu  sync.Mutex
	revocationErr error
}

// RecordRevocationError keeps a handler's error when it carries a
// cancellation cause, so Finish can tell a clean revocation from a tool whose
// termination was never confirmed.
func (session *Session) RecordRevocationError(err error) {
	if !HasCancellationCause(err) {
		return
	}
	session.revocationMu.Lock()
	session.revocationErr = errors.Join(session.revocationErr, err)
	session.revocationMu.Unlock()
}

func (session *Session) revocationError() error {
	session.revocationMu.Lock()
	defer session.revocationMu.Unlock()
	if isPureCancellation(session.revocationErr) {
		return nil
	}
	return session.revocationErr
}

// Finish runs after the transport is revoked: it waits for every handler,
// seals the audit, and removes the private files, which is what makes the
// revocation positive. Any failure is returned so Stop can be retried.
func (session *Session) Finish(ctx context.Context, private ...string) error {
	done := make(chan struct{})
	go func() { session.Wait.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	auditErr := session.Audit.sealAndWait(ctx)
	if !session.Audit.finished() {
		return auditErr
	}
	errs := []error{session.revocationError(), auditErr}
	for _, path := range private {
		errs = append(errs, removeIfPresent(path))
	}
	err := errors.Join(errs...)
	if session.Partial && err == nil {
		err = os.RemoveAll(session.ArtifactDir)
	}
	return err
}

// HasCancellationCause reports whether err carries a context cancellation or
// deadline.
func HasCancellationCause(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// WithCancellation keeps a cancelled call from looking successful or like an
// ordinary failure. A sandbox error returned after revocation is ambiguous
// unless it carries the cancellation cause, so both are preserved and Stop
// fails closed rather than treating an unconfirmed termination as an earlier
// error.
func WithCancellation(ctx context.Context, err error) error {
	contextErr := ctx.Err()
	if contextErr == nil || HasCancellationCause(err) {
		return err
	}
	if err == nil {
		return contextErr
	}
	return errors.Join(contextErr, err)
}

// isPureCancellation keeps Stop failing closed: anything that is not provably
// cancellation alone is preserved and fails revocation.
func isPureCancellation(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isPureCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isPureCancellation(wrapped.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

// Closer revokes one bridge session and confirms it: its transport, then
// Session.Finish.
type Closer interface{ Close(context.Context) error }

// Slot holds at most one live bridge session and makes Stop idempotent: a
// failed Stop keeps the session, so a retry can still confirm revocation, and
// a finished Stop reports its result to every later call.
type Slot struct {
	mu       sync.Mutex
	active   Closer
	stopping bool
	done     chan struct{}
	err      error
}

// Start runs open while holding the slot. When open fails after allocating a
// session, that session is closed under a fresh context bounded by cleanup;
// if closing fails too, it stays in the slot so Stop can retry. open must
// return a nil Closer, not a typed nil, when nothing was allocated.
func (slot *Slot) Start(ctx context.Context, cleanup time.Duration, open func() (Closer, error)) error {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.active != nil || slot.stopping {
		return errors.New("bridge is already active")
	}
	session, err := open()
	if err == nil {
		slot.active, slot.err = session, nil
		return nil
	}
	if session == nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanup)
	defer cancel()
	if closeErr := session.Close(cleanupCtx); closeErr != nil {
		slot.active = session
		return errors.Join(err, closeErr)
	}
	return err
}

// Stop closes the live session. Concurrent calls wait for the one in
// progress; a call with no session returns the last result.
func (slot *Slot) Stop(ctx context.Context) error {
	slot.mu.Lock()
	if slot.active == nil && !slot.stopping {
		err := slot.err
		slot.mu.Unlock()
		return err
	}
	if slot.stopping {
		done := slot.done
		slot.mu.Unlock()
		select {
		case <-done:
			slot.mu.Lock()
			defer slot.mu.Unlock()
			return slot.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	session := slot.active
	slot.stopping = true
	slot.done = make(chan struct{})
	done := slot.done
	slot.mu.Unlock()

	err := session.Close(ctx)

	slot.mu.Lock()
	slot.err = err
	slot.stopping = false
	if err == nil {
		slot.active = nil
	}
	close(done)
	slot.mu.Unlock()
	return err
}

// Active is the live session, for tests.
func (slot *Slot) Active() Closer {
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.active
}
