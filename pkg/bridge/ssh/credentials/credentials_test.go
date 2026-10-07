package credentials

import (
	"bytes"
	"encoding/pem"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestControllerCredentialsSeparateClientPrivateKey(t *testing.T) {
	clientPrivate, clientPublic, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(clientPrivate)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		t.Fatal("identity is not OpenSSH encoded")
	}
	clientSigner, err := ssh.ParsePrivateKey(clientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clientSigner.PublicKey().Marshal(), clientPublic.Marshal()) {
		t.Fatal("identity public key does not match")
	}
	hostPrivate, hostPublic, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	authorized := ssh.MarshalAuthorizedKey(clientPublic)
	c, err := ParseCredentials(hostPrivate, authorized)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.HostSigner.PublicKey().Marshal(), hostPublic.Marshal()) || !bytes.Equal(c.AuthorizedKey.Marshal(), clientPublic.Marshal()) {
		t.Fatal("server authority does not match staged identities")
	}
	if _, err := ParseCredentials(hostPrivate, append(authorized, authorized...)); err == nil {
		t.Fatal("multiple authorized keys accepted")
	}
}
