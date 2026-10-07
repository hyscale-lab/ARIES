package credentials

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"

	"golang.org/x/crypto/ssh"
)

// Credentials contains only the authority required by a native SSH server.
type Credentials struct {
	HostSigner    ssh.Signer
	AuthorizedKey ssh.PublicKey
}

// GenerateIdentity creates an OpenSSH identity for controller-owned staging.
func GenerateIdentity() ([]byte, ssh.PublicKey, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return nil, nil, err
	}
	public, err := ssh.NewPublicKey(pub)
	return pem.EncodeToMemory(block), public, err
}

func ParseCredentials(hostPrivate, authorized []byte) (*Credentials, error) {
	host, err := ssh.ParsePrivateKey(hostPrivate)
	if err != nil {
		return nil, errors.New("invalid staged bridge host key")
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey(authorized)
	if err != nil || len(rest) > 0 {
		return nil, errors.New("invalid staged bridge authorized key")
	}
	return &Credentials{HostSigner: host, AuthorizedKey: pub}, nil
}
