package hermesssh

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

// Credentials is the runner's half of a bridge that serves in another process,
// such as the aries-bridge pod. The runner generates the client key here and
// sends only the public half to the bridge (as SessionKeys.Authorized), so the
// private key never leaves the runner's host. Its files use the same names and
// modes Start writes when the bridge runs in-process, so the harness cannot
// tell the difference.
type Credentials struct {
	artifactDir string
	identity    string
	known       string
	public      ssh.PublicKey
}

// NewCredentials generates a client key and writes its private half, 0600, to
// artifactDir/id_ed25519. artifactDir is created private if it is missing and
// must not traverse a symbolic link.
func NewCredentials(artifactDir string) (*Credentials, error) {
	if err := ensurePrivateDirectory(artifactDir); err != nil {
		return nil, fmt.Errorf("create private Hermes SSH artifact directory: %w", err)
	}
	_, clientPEM, public, err := generateSessionKeys()
	if err != nil {
		return nil, err
	}
	credentials := &Credentials{
		artifactDir: artifactDir,
		identity:    filepath.Join(artifactDir, "id_ed25519"),
		known:       filepath.Join(artifactDir, "known_hosts"),
		public:      public,
	}
	if err := writeExclusivePrivate(credentials.identity, clientPEM); err != nil {
		return nil, fmt.Errorf("write Hermes SSH identity: %w", err)
	}
	return credentials, nil
}

// AuthorizedKey is the public key the bridge must accept.
func (c *Credentials) AuthorizedKey() ssh.PublicKey { return c.public }

// Endpoint records the host key the bridge serves with and returns the
// endpoint Hermes is given. known_hosts is evidence only, exactly as in
// Start: Hermes pins the host key on first use and cannot be preloaded.
func (c *Credentials) Endpoint(address string, hostKey ssh.PublicKey, network string) (core.ToolEndpoint, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return core.ToolEndpoint{}, fmt.Errorf("Hermes SSH bridge address %q is not host:port", address)
	}
	if hostKey == nil {
		return core.ToolEndpoint{}, errors.New("Hermes SSH bridge returned no host key")
	}
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostKey))
	if err := writeExclusivePrivate(c.known, []byte(knownLine)); err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("write Hermes SSH known-hosts file: %w", err)
	}
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: lockedUsername, Network: network,
		IdentityFile: identityContainerPath, IdentitySourceFile: c.identity,
	}, nil
}

// Revoke removes the private identity, which is this side's share of
// revocation; known_hosts is kept as evidence, as finalize keeps it.
func (c *Credentials) Revoke() error {
	return removeIfPresent(c.identity)
}
