package target

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"golang.org/x/crypto/ssh"
)

// Credentials contains only the authority required by a native SSH server.
type Credentials struct {
	HostSigner    ssh.Signer
	AuthorizedKey ssh.PublicKey
}

// CredentialBundle is generated in the controller. ClientPrivate stays there.
type CredentialBundle struct{ ClientPrivate, HostPrivate, AuthorizedKey []byte }

func GenerateCredentials() (CredentialBundle, error) {
	var b CredentialBundle
	for _, dst := range []*[]byte{&b.ClientPrivate, &b.HostPrivate} {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return b, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return b, err
		}
		*dst = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	signer, err := ssh.ParsePrivateKey(b.ClientPrivate)
	if err != nil {
		return b, err
	}
	b.AuthorizedKey = ssh.MarshalAuthorizedKey(signer.PublicKey())
	return b, nil
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
