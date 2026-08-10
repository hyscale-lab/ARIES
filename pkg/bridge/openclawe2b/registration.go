package openclawe2b

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	sandboxIDHeader   = "E2b-Sandbox-Id"
	accessTokenHeader = "X-Access-Token"
)

type registrationState uint8

const (
	registrationActive registrationState = iota + 1
	registrationRevoking
	registrationRevoked
)

type registration struct {
	sandbox   runner.Sandbox
	sandboxID string
	tokenHash [sha256.Size]byte
	gateway   netip.Addr
	state     registrationState
	admitted  int
	drained   chan struct{}
	revokeMu  sync.Mutex
}

type localDestinationKey struct{}

func retainLocalDestination(ctx context.Context, connection net.Conn) context.Context {
	return context.WithValue(ctx, localDestinationKey{}, connection.LocalAddr())
}

func localDestination(ctx context.Context) (netip.Addr, bool) {
	address, ok := ctx.Value(localDestinationKey{}).(net.Addr)
	if !ok || address == nil {
		return netip.Addr{}, false
	}
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(host)
	return ip.Unmap(), err == nil
}

func (server *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	release, ok := server.authorize(request)
	if !ok {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer release()
	if request.Method == http.MethodPost && request.URL.Path == "/v1/process/start" {
		server.serveProcessStart(response, request)
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/process/send-signal" {
		server.serveProcessSendSignal(response, request)
		return
	}
	if request.URL.Path == "/v1/files" && (request.Method == http.MethodGet || request.Method == http.MethodPost) {
		server.serveRawFile(response, request)
		return
	}
	if request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/v1/filesystem/") {
		server.serveFilesystem(response, request)
		return
	}
	http.Error(response, "bridge protocol not implemented", http.StatusNotImplemented)
}

func (server *Server) authorize(request *http.Request) (func(), bool) {
	destination, ok := localDestination(request.Context())
	if !ok {
		return nil, false
	}
	sandboxID := request.Header.Get(sandboxIDHeader)
	token := request.Header.Get(accessTokenHeader)
	if sandboxID == "" || token == "" || strings.TrimSpace(sandboxID) != sandboxID || strings.TrimSpace(token) != token {
		return nil, false
	}
	tokenHash := sha256.Sum256([]byte(token))

	server.mu.Lock()
	registration := server.registrations[sandboxID]
	if registration == nil || registration.state != registrationActive || registration.gateway != destination || subtle.ConstantTimeCompare(registration.tokenHash[:], tokenHash[:]) != 1 {
		server.mu.Unlock()
		return nil, false
	}
	registration.admitted++
	server.mu.Unlock()

	return func() { server.release(registration) }, true
}

func (server *Server) release(registration *registration) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if registration.admitted > 0 {
		registration.admitted--
	}
	if registration.state == registrationRevoking && registration.admitted == 0 {
		if registration.drained != nil {
			close(registration.drained)
			registration.drained = nil
		}
	}
}

func (server *Server) register(registration *registration) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if !server.accepting || server.listener == nil {
		return errors.New("OpenClaw E2B bridge server is not accepting grants")
	}
	if _, exists := server.registrations[registration.sandboxID]; exists {
		return fmt.Errorf("sandbox ID %q is already registered", registration.sandboxID)
	}
	registration.state = registrationActive
	server.registrations[registration.sandboxID] = registration
	return nil
}

func (server *Server) revoke(ctx context.Context, sandboxID string) error {
	server.mu.Lock()
	registration := server.registrations[sandboxID]
	if registration == nil {
		server.mu.Unlock()
		return nil
	}
	if registration.state == registrationActive {
		registration.state = registrationRevoking
		registration.tokenHash = [sha256.Size]byte{}
		if registration.admitted != 0 {
			registration.drained = make(chan struct{})
		}
	}
	server.mu.Unlock()

	registration.revokeMu.Lock()
	defer registration.revokeMu.Unlock()

	var revokeErr error
	for _, process := range server.processesFor(registration) {
		if err := process.sandbox.TerminateProcess(ctx, process.ref); err != nil {
			revokeErr = errors.Join(revokeErr, fmt.Errorf("terminate sandbox %q process %d: %w", sandboxID, process.key.pid, err))
		}
	}

	server.mu.Lock()
	drained := registration.drained
	admitted := registration.admitted
	server.mu.Unlock()

	if admitted != 0 {
		select {
		case <-drained:
		case <-ctx.Done():
			return errors.Join(revokeErr, fmt.Errorf("drain sandbox %q bridge requests: %w", sandboxID, ctx.Err()))
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if current := server.registrations[sandboxID]; current != registration {
		return revokeErr
	}
	if remaining := server.processCountLocked(sandboxID); remaining != 0 {
		return errors.Join(revokeErr, fmt.Errorf("confirm sandbox %q processes absent: %d remain", sandboxID, remaining))
	}
	if registration.admitted != 0 {
		return errors.Join(revokeErr, fmt.Errorf("confirm sandbox %q requests drained: %d remain", sandboxID, registration.admitted))
	}
	if revokeErr != nil {
		return revokeErr
	}
	server.finishRevocationLocked(registration)
	_, exists := server.registrations[sandboxID]
	if exists {
		return fmt.Errorf("sandbox %q bridge registration still exists", sandboxID)
	}
	return nil
}

func (server *Server) finishRevocationLocked(registration *registration) {
	if current := server.registrations[registration.sandboxID]; current != registration {
		return
	}
	delete(server.registrations, registration.sandboxID)
	registration.state = registrationRevoked
}
