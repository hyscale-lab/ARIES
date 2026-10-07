package openclaw

import (
	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"testing"
)

func TestDialectRejectsHermesAndUnknownVirtualControls(t *testing.T) {
	for _, wire := range []string{"bash -c true", "echo $HOME", encodeCanonicalTokens([]string{remoteShell, "-c", "rm -rf -- " + virtualRuntimeRoot})} {
		prepared, refusal := (Dialect{}).Prepare(wire, "/work")
		if refusal == nil || refusal.OperationClass != "exec" || refusal.Status != "rejected" || refusal.Message != "invalid remote command" || prepared.Command.Path != "" {
			t.Fatalf("Prepare(%q) = %#v,%#v", wire, prepared, refusal)
		}
	}
	policy := (Dialect{}).Policy()
	if policy.UnsupportedRequests != bridgessh.RejectAndClose || policy.RefusedExitCode != 0 || policy.Endpoint.UseSandboxWorkdir || policy.Endpoint.ClientCommand != "/opt/aries/bin/aries-ssh-client" || policy.Endpoint.KnownHostsFile != "/run/aries/ssh/known_hosts" {
		t.Fatalf("policy = %#v", policy)
	}
}
