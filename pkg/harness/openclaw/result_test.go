package openclaw

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseAgentResultRequiresOKAndConcatenatesPayloads(t *testing.T) {
	stdout := []byte(`{"runId":"one","status":"ok","result":{"payloads":[{"text":"first"},{"text":"second"}]}}`)
	response, err := parseAgentResult(stdout, []byte("diagnostic\n"))
	if err != nil || response != "first\nsecond" {
		t.Fatalf("response = %q, %v", response, err)
	}
}

func TestParseAgentResultRejectsMalformedNonOKFallbackAndEmpty(t *testing.T) {
	for name, test := range map[string]struct{ stdout, stderr string }{
		"malformed": {stdout: "{"},
		"non-ok":    {stdout: `{"status":"error","result":{"payloads":[{"text":"no"}]}}`},
		"fallback":  {stdout: `{"status":"ok","result":{"payloads":[{"text":"no"}]}}`, stderr: "EMBEDDED FALLBACK: local"},
		"empty":     {stdout: `{"status":"ok","result":{"payloads":[]}}`},
		"trailing":  {stdout: `{"status":"ok","result":{"payloads":[{"text":"x"}]}} {}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAgentResult([]byte(test.stdout), []byte(test.stderr)); err == nil {
				t.Fatal("invalid result was accepted")
			}
		})
	}
}

func TestRedactBytesRemovesEveryExactSecret(t *testing.T) {
	secret := []byte("dummy-key-123")
	redacted := redactBytes([]byte("before dummy-key-123 middle dummy-key-123 after"), secret)
	if bytes.Contains(redacted, secret) || strings.Count(string(redacted), "[REDACTED]") != 2 {
		t.Fatalf("redacted = %q", redacted)
	}
}
