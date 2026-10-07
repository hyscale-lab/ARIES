package ssh

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

type pausedAcceptListener struct {
	net.Listener
	accepted, release chan struct{}
	once              sync.Once
}

func (listener *pausedAcceptListener) Accept() (net.Conn, error) {
	conn, err := listener.Listener.Accept()
	if err == nil {
		listener.once.Do(func() { close(listener.accepted); <-listener.release })
	}
	return conn, err
}

func TestServerClosesConnectionAcceptedDuringRevocation(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	paused := &pausedAcceptListener{Listener: listener, accepted: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(paused.release) }) }
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &bridgeSession{listener: paused, cancel: cancel, connections: make(map[net.Conn]struct{})}
	session.wait.Add(1)
	go session.serve(ctx, logrus.New())
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-paused.accepted:
	case <-time.After(time.Second):
		t.Fatal("accept did not pause")
	}
	session.revoke()
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := session.waitFor(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("unjoined listener wait = %v", err)
	}
	release()
	waitCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := session.waitFor(waitCtx); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection survived revoke")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("untracked accepted connection left open")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.connections) != 0 {
		t.Fatal("revoked connection admitted")
	}
}
