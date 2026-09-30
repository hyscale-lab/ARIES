package bridgekit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// closer is a session whose Close result the test controls.
type closer struct {
	release chan struct{}
	results []error
	calls   int
}

func (session *closer) Close(ctx context.Context) error {
	if session.release != nil {
		select {
		case <-session.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	session.calls++
	if len(session.results) == 0 {
		return nil
	}
	err := session.results[0]
	session.results = session.results[1:]
	return err
}

func TestSlotHoldsOneSessionAndStopIsIdempotent(t *testing.T) {
	var slot Slot
	session := &closer{}
	if err := slot.Start(context.Background(), time.Second, func() (Closer, error) { return session, nil }); err != nil {
		t.Fatal(err)
	}
	opened := false
	if err := slot.Start(context.Background(), time.Second, func() (Closer, error) { opened = true; return &closer{}, nil }); err == nil || opened {
		t.Fatalf("second Start = %v, opened %v", err, opened)
	}
	for range 2 {
		if err := slot.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if session.calls != 1 || slot.Active() != nil {
		t.Fatalf("closes = %d, active = %v", session.calls, slot.Active())
	}
}

func TestSlotRetainsAFailedStopForRetry(t *testing.T) {
	var slot Slot
	session := &closer{results: []error{errors.New("unconfirmed")}}
	_ = slot.Start(context.Background(), time.Second, func() (Closer, error) { return session, nil })
	if err := slot.Stop(context.Background()); err == nil {
		t.Fatal("failed close reported success")
	}
	if slot.Active() == nil {
		t.Fatal("failed Stop dropped the session")
	}
	if err := slot.Stop(context.Background()); err != nil || slot.Active() != nil {
		t.Fatalf("retry = %v, active = %v", err, slot.Active())
	}
}

func TestSlotConcurrentStopWaitsForTheOneInProgress(t *testing.T) {
	var slot Slot
	session := &closer{release: make(chan struct{}), results: []error{errors.New("first")}}
	_ = slot.Start(context.Background(), time.Second, func() (Closer, error) { return session, nil })
	first := make(chan error)
	go func() { first <- slot.Stop(context.Background()) }()
	for {
		slot.mu.Lock()
		stopping := slot.stopping
		slot.mu.Unlock()
		if stopping {
			break
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan error)
	go func() { second <- slot.Stop(context.Background()) }()
	// ponytail: a sleep, since the waiting Stop has no observable state; a
	// late start only makes the test fail, never pass wrongly.
	time.Sleep(20 * time.Millisecond)
	close(session.release)
	if err := <-first; err == nil || err.Error() != "first" {
		t.Fatalf("first Stop = %v", err)
	}
	if err := <-second; err == nil || err.Error() != "first" {
		t.Fatalf("waiting Stop = %v, want the in-progress result", err)
	}
	if session.calls != 1 {
		t.Fatalf("closes = %d", session.calls)
	}
}

func TestSlotClosesAPartialStartAndKeepsItWhenCloseFails(t *testing.T) {
	var slot Slot
	openErr := errors.New("listen failed")
	session := &closer{results: []error{errors.New("cleanup failed")}}
	err := slot.Start(context.Background(), time.Second, func() (Closer, error) { return session, openErr })
	if !errors.Is(err, openErr) || !strings.Contains(err.Error(), "cleanup failed") || slot.Active() == nil {
		t.Fatalf("Start = %v, active = %v", err, slot.Active())
	}
	if err := slot.Stop(context.Background()); err != nil || session.calls != 2 {
		t.Fatalf("Stop = %v, closes = %d", err, session.calls)
	}

	var empty Slot
	if err := empty.Start(context.Background(), time.Second, func() (Closer, error) { return nil, openErr }); err != openErr || empty.Active() != nil {
		t.Fatalf("Start without a session = %v, active = %v", err, empty.Active())
	}
}

func TestFinishRemovesPrivateFilesAndAPartialStart(t *testing.T) {
	for _, partial := range []bool{false, true} {
		directory := filepath.Join(t.TempDir(), "bridge")
		if err := EnsurePrivateDirectory(directory); err != nil {
			t.Fatal(err)
		}
		identity := filepath.Join(directory, "identity")
		evidence := filepath.Join(directory, "evidence")
		for _, path := range []string{identity, evidence} {
			if err := WriteExclusivePrivate(path, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		session := &Session{ArtifactDir: directory, Partial: partial}
		if err := session.Finalize(context.Background(), identity); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(identity); !os.IsNotExist(err) {
			t.Fatalf("partial=%v: identity survived: %v", partial, err)
		}
		if _, err := os.Stat(evidence); os.IsNotExist(err) != partial {
			t.Fatalf("partial=%v: evidence stat = %v", partial, err)
		}
	}
}

func TestFinishFailsOnAnUnconfirmedTermination(t *testing.T) {
	session := &Session{}
	session.RecordRevocationError(context.Canceled)
	if err := session.Finalize(context.Background()); err != nil {
		t.Fatalf("pure cancellation = %v", err)
	}
	session.RecordRevocationError(errors.Join(context.Canceled, errors.New("exec still running")))
	if err := session.Finalize(context.Background()); err == nil {
		t.Fatal("an unconfirmed termination was accepted")
	}
}

func TestExemptBytesAreNotChargedToTheLimit(t *testing.T) {
	structured, _ := memoryAuditFile()
	writer := newAuditWriter(structured, nil, 200)
	content := strings.Repeat("x", 256)
	writer.Enqueue(&testRecord{Command: content}, nil, len(content))
	if err := writer.sealAndWait(context.Background()); err != nil {
		t.Fatalf("exempt content latched: %v", err)
	}

	structured, _ = memoryAuditFile()
	charged := newAuditWriter(structured, nil, 200)
	charged.Enqueue(&testRecord{Command: content}, nil, 0)
	if err := charged.sealAndWait(context.Background()); err == nil {
		t.Fatal("charged content over the limit was admitted")
	}
}
