package openclaw

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactBytesRemovesEveryExactSecret(t *testing.T) {
	secret := []byte("dummy-key-123")
	redacted := redactBytes([]byte("before dummy-key-123 middle dummy-key-123 after"), secret)
	if bytes.Contains(redacted, secret) || strings.Count(string(redacted), "[REDACTED]") != 2 {
		t.Fatalf("redacted = %q", redacted)
	}
}
