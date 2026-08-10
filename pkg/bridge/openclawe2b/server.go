// Package openclawe2b contains the application-scoped OpenClaw HTTP bridge.
package openclawe2b

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const listenAddress = "0.0.0.0:0"

type listenFunc func(context.Context, string, string) (net.Listener, error)

// Server owns the one application-scoped IPv4 listener and task grant map.
type Server struct {
	mu            sync.Mutex
	listen        listenFunc
	http          *http.Server
	listener      net.Listener
	done          chan error
	stopDone      chan struct{}
	stopErr       error
	stopping      bool
	started       bool
	accepting     bool
	registrations map[string]*registration
	processes     map[processKey]*activeProcess
	nextProcess   uint64
}

func New() *Server {
	return newServer((&net.ListenConfig{}).Listen)
}

func newServer(listen listenFunc) *Server {
	return &Server{listen: listen, registrations: make(map[string]*registration), processes: make(map[processKey]*activeProcess)}
}

// Start binds one wildcard TCP4 listener. Authenticated requests deliberately
// receive not-implemented responses until later R22 steps add protocol routes.
func (server *Server) Start(ctx context.Context) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started || server.listener != nil || server.stopping || server.stopDone != nil {
		return errors.New("OpenClaw E2B bridge server already started or stopped")
	}
	listener, err := server.listen(ctx, "tcp4", listenAddress)
	if err != nil {
		return fmt.Errorf("listen for OpenClaw E2B bridge: %w", err)
	}
	server.listener = listener
	server.started = true
	server.accepting = true
	server.http = &http.Server{
		Handler:           server,
		ConnContext:       retainLocalDestination,
		ReadHeaderTimeout: 5 * time.Second,
	}
	server.done = make(chan error, 1)
	go func() {
		err := server.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.done <- err
	}()
	return nil
}

// Address returns the bound wildcard listener address for diagnostics. Task
// endpoints will use their own Docker-network gateway with this listener's port.
func (server *Server) Address() (string, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.listener == nil {
		return "", errors.New("OpenClaw E2B bridge server is not started")
	}
	return server.listener.Addr().String(), nil
}

// Stop closes the application listener and positively waits for Serve to exit.
func (server *Server) Stop(ctx context.Context) error {
	server.mu.Lock()
	if server.stopDone != nil {
		stopDone := server.stopDone
		server.mu.Unlock()
		select {
		case <-stopDone:
			server.mu.Lock()
			err := server.stopErr
			server.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	server.stopDone = make(chan struct{})
	server.stopping = true
	server.accepting = false
	httpServer, serveDone := server.http, server.done
	registrationIDs := make([]string, 0, len(server.registrations))
	for sandboxID := range server.registrations {
		registrationIDs = append(registrationIDs, sandboxID)
	}
	server.mu.Unlock()

	var stopErr error
	for _, sandboxID := range registrationIDs {
		stopErr = errors.Join(stopErr, server.revoke(ctx, sandboxID))
	}
	if httpServer != nil {
		if err := httpServer.Shutdown(ctx); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("shut down OpenClaw E2B bridge: %w", err))
			stopErr = errors.Join(stopErr, httpServer.Close())
		}
	}
	if serveDone != nil {
		select {
		case err := <-serveDone:
			stopErr = errors.Join(stopErr, err)
		case <-ctx.Done():
			stopErr = errors.Join(stopErr, fmt.Errorf("confirm OpenClaw E2B bridge stopped: %w", ctx.Err()))
		}
	}

	server.mu.Lock()
	if len(server.registrations) != 0 {
		stopErr = errors.Join(stopErr, fmt.Errorf("confirm OpenClaw E2B bridge registrations absent: %d remain", len(server.registrations)))
	}
	if len(server.processes) != 0 {
		stopErr = errors.Join(stopErr, fmt.Errorf("confirm OpenClaw E2B bridge processes absent: %d remain", len(server.processes)))
	}
	server.stopErr = stopErr
	server.listener = nil
	server.http = nil
	server.done = nil
	server.stopping = false
	close(server.stopDone)
	if stopErr != nil {
		// A failed positive shutdown remains retryable. The listener is already
		// closed and admission stays disabled, but remaining grants/processes
		// must be given another bounded cleanup attempt.
		server.stopDone = nil
	}
	server.mu.Unlock()
	return stopErr
}
