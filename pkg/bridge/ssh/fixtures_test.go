package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh/credentials"
	"github.com/hyscale-lab/aries/pkg/core"
	gossh "golang.org/x/crypto/ssh"
)

const remoteShell = "/bin/sh"
const remoteEnv = "env"

var fixtureSigners sync.Map

func fixtureCredentials(t *testing.T) (*credentials.Credentials, gossh.Signer) {
	t.Helper()
	_, host, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, client, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := gossh.NewSignerFromKey(host)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(client)
	if err != nil {
		t.Fatal(err)
	}
	return &credentials.Credentials{HostSigner: hostSigner, AuthorizedKey: signer.PublicKey()}, signer
}
func loopbackListen(context.Context) (core.BridgeListen, error) {
	return core.BridgeListen{BindHost: "127.0.0.1", AdvertiseHost: "127.0.0.1"}, nil
}

// Lifecycle fixtures use a deliberately synthetic JSON dialect; actual native
// grammar and transport contracts are exercised with the production adapters.
type testDialect struct{}

func (testDialect) Policy() Policy {
	return Policy{InvalidOperationClass: "exec"}
}
func (testDialect) Prepare(encoded, workdir string) (Prepared, *Refusal) {
	var argv []string
	if json.Unmarshal([]byte(encoded), &argv) != nil || len(argv) == 0 {
		return Prepared{}, &Refusal{OperationClass: "exec", Status: "rejected", Message: "invalid remote command"}
	}
	env := map[string]string{}
	if argv[0] == remoteEnv {
		argv = argv[1:]
		for len(argv) > 0 && argv[0] != remoteShell {
			name, value, _ := strings.Cut(argv[0], "=")
			env[name] = value
			argv = argv[1:]
		}
	}
	if len(env) == 0 {
		env = nil
	}
	if len(argv) < 3 {
		return Prepared{}, &Refusal{OperationClass: "exec", Status: "rejected", Message: "invalid remote command"}
	}
	return Prepared{Command: core.Command{Path: argv[0], Args: argv[1:], Dir: workdir, Env: env}, HashInput: encoded, Display: argv[2], OperationClass: "exec", RefusalClass: "exec"}, nil
}
func encodeCanonicalTokens(argv []string) string { b, _ := json.Marshal(argv); return string(b) }
