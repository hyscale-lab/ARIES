package hermes

import (
	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"testing"
)

func TestDialectPreservesRefusalClassification(t *testing.T) {
	for _, test := range []struct{ wire, workdir, class, status, message string }{
		{"mkdir -p /root/.hermes", "/work", "sync", "denied", errSyncDenied.Error()},
		{"'/bin/sh' '-c' 'true'", "/work", "unknown", "rejected", "invalid remote command"},
		{"bash -c true", "/unsafe path", "agent", "rejected", "invalid remote command"},
		{connectionProbePayload, "/unsafe path", "bootstrap", "rejected", "invalid remote command"},
	} {
		prepared, refusal := (Dialect{}).Prepare(test.wire, test.workdir)
		if refusal == nil || refusal.OperationClass != test.class || refusal.Status != test.status || refusal.Message != test.message || prepared.Command.Path != "" {
			t.Fatalf("Prepare(%q,%q) = %#v,%#v", test.wire, test.workdir, prepared, refusal)
		}
	}
	policy := (Dialect{}).Policy()
	if policy.UnsupportedRequests != bridgessh.RejectAndContinue || policy.RefusedExitCode != -1 || !policy.Endpoint.UseSandboxWorkdir || policy.Endpoint.ClientCommand != "" || policy.Endpoint.KnownHostsFile != "" {
		t.Fatalf("policy = %#v", policy)
	}
}
