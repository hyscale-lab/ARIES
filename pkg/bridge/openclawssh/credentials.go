package openclawssh

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

// Credentials is the runner's half of a bridge that serves in another process,
// such as the aries-bridge pod. The runner stages the aries-ssh helper and
// generates the client key here, and sends only the public half to the bridge
// (as SessionKeys.Authorized), so the private key never leaves the runner's
// host. The files use the same names and modes Start writes when the bridge
// runs in-process, so the harness cannot tell the difference.
type Credentials struct {
	client   string
	identity string
	known    string
	public   ssh.PublicKey
}

// NewCredentials stages clientPath as artifactDir/aries-ssh (0555) and writes
// a fresh client identity to artifactDir/id_ed25519 (0600). artifactDir is
// created private if it is missing and must not traverse a symbolic link.
func NewCredentials(artifactDir, clientPath string) (*Credentials, error) {
	if clientPath == "" {
		clientPath = defaultClientPath
	}
	clientPath, err := filepath.Abs(clientPath)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenClaw SSH client helper: %w", err)
	}
	if err := ensurePrivateDirectory(artifactDir); err != nil {
		return nil, fmt.Errorf("create private OpenClaw SSH artifact directory: %w", err)
	}
	credentials := &Credentials{
		client:   filepath.Join(artifactDir, "aries-ssh"),
		identity: filepath.Join(artifactDir, "id_ed25519"),
		known:    filepath.Join(artifactDir, "known_hosts"),
	}
	if err := stageExecutable(clientPath, credentials.client); err != nil {
		return nil, fmt.Errorf("stage OpenClaw SSH client: %w", err)
	}
	_, clientPEM, public, err := generateSessionKeys()
	if err != nil {
		return nil, errors.Join(err, credentials.Revoke())
	}
	if err := writeExclusivePrivate(credentials.identity, clientPEM); err != nil {
		return nil, errors.Join(fmt.Errorf("write OpenClaw SSH identity: %w", err), credentials.Revoke())
	}
	credentials.public = public
	return credentials, nil
}

// AuthorizedKey is the public key the bridge must accept.
func (c *Credentials) AuthorizedKey() ssh.PublicKey { return c.public }

// Endpoint writes the known_hosts the aries-ssh helper verifies the bridge
// against and returns the endpoint OpenClaw is given.
func (c *Credentials) Endpoint(address string, hostKey ssh.PublicKey, network string) (core.ToolEndpoint, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return core.ToolEndpoint{}, fmt.Errorf("OpenClaw SSH bridge address %q is not host:port", address)
	}
	if hostKey == nil {
		return core.ToolEndpoint{}, errors.New("OpenClaw SSH bridge returned no host key")
	}
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostKey))
	if err := writeExclusivePrivate(c.known, []byte(knownLine)); err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("write OpenClaw SSH known-hosts file: %w", err)
	}
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: lockedUsername, Network: network,
		ClientCommand: clientContainerPath, ClientSourceFile: c.client,
		IdentityFile: identityContainerPath, IdentitySourceFile: c.identity,
		KnownHostsFile: knownHostsContainerPath, KnownHostsSourceFile: c.known,
	}, nil
}

// Revoke removes the helper, the identity and known_hosts, which is what
// finalize removes when the bridge runs in-process.
func (c *Credentials) Revoke() error {
	return errors.Join(removeIfPresent(c.client), removeIfPresent(c.identity), removeIfPresent(c.known))
}
