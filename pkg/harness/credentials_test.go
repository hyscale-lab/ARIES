package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestCredentialSnapshotSurvivesOwnerCleanup(t *testing.T) {
	source := []byte("private-model-key")
	owner := NewCredentials("Hermes")
	if ok, err := owner.Load("model", "MODEL_KEY", func(string) ([]byte, bool) { return source, true }); !ok || err != nil {
		t.Fatalf("load: %v, %v", ok, err)
	}
	if !bytes.Equal(source, make([]byte, len(source))) {
		t.Fatal("lookup buffer not cleared")
	}
	calls := 0
	servers := []core.MCPServerConfig{{Name: "one", SecretEnv: map[string]string{"TOKEN": "MCP_KEY"}}, {Name: "two", SecretEnv: map[string]string{"TOKEN": "MCP_KEY"}}}
	if err := owner.LoadMCP(servers, []byte("config"), func(string) ([]byte, bool) { calls++; return []byte("private-mcp-key"), true }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("duplicate credential lookups: %d", calls)
	}
	snapshot := owner.Snapshot()
	ownerKey := owner.Get("model")
	owner.Clear()
	if !bytes.Equal(ownerKey, make([]byte, len(ownerKey))) {
		t.Fatal("owner buffer retained")
	}
	if string(snapshot.Get("model")) != "private-model-key" || string(snapshot.MCPFiles()["MCP_KEY"]) != "private-mcp-key" {
		t.Fatal("snapshot destroyed by owner cleanup")
	}
	if got := string(snapshot.Redact([]byte("private-model-key private-mcp-key"))); got != "[REDACTED] [REDACTED]" {
		t.Fatalf("snapshot redaction: %q", got)
	}
	snapshotKey := snapshot.MCPFiles()["MCP_KEY"]
	snapshot.Clear()
	if !bytes.Equal(snapshotKey, make([]byte, len(snapshotKey))) {
		t.Fatal("snapshot buffer retained")
	}
}

func TestRedactErrDoesNotExposeOriginalCause(t *testing.T) {
	credentials := NewCredentials("Hermes")
	credentials.Set("model", []byte("private-key"))
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("provider error")} {
		raw := fmt.Errorf("provider echoed private-key: %w", cause)
		redacted := credentials.RedactErr(raw)
		for current := redacted; current != nil; current = errors.Unwrap(current) {
			if strings.Contains(current.Error(), "private-key") {
				t.Fatal("error chain exposes credential")
			}
		}
		if !strings.Contains(redacted.Error(), "[REDACTED]") {
			t.Fatal("secret was not redacted")
		}
		if (cause == context.Canceled || cause == context.DeadlineExceeded) && !errors.Is(redacted, cause) {
			t.Fatal("lost cancellation classification")
		}
	}
	if credentials.RedactErr(nil) != nil {
		t.Fatal("nil error changed")
	}
}
