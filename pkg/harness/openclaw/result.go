package openclaw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const embeddedFallbackMarker = "EMBEDDED FALLBACK:"

type agentEnvelope struct {
	Status string `json:"status"`
	Result struct {
		Payloads []struct {
			Text string `json:"text"`
		} `json:"payloads"`
	} `json:"result"`
}

func parseAgentResult(stdout, stderr []byte) (string, error) {
	if bytes.Contains(stderr, []byte(embeddedFallbackMarker)) {
		return "", errors.New("OpenClaw reported an embedded fallback")
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	var envelope agentEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return "", fmt.Errorf("decode OpenClaw agent JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("OpenClaw agent output contains multiple JSON values")
		}
		return "", fmt.Errorf("read trailing OpenClaw agent JSON: %w", err)
	}
	if envelope.Status != "ok" {
		return "", fmt.Errorf("OpenClaw agent status is %q, want %q", envelope.Status, "ok")
	}
	texts := make([]string, 0, len(envelope.Result.Payloads))
	for _, payload := range envelope.Result.Payloads {
		if payload.Text != "" {
			texts = append(texts, payload.Text)
		}
	}
	if len(texts) == 0 {
		return "", errors.New("OpenClaw agent returned no payload text")
	}
	return strings.Join(texts, "\n"), nil
}

func redactBytes(content, secret []byte) []byte {
	copyContent := append([]byte(nil), content...)
	if len(secret) == 0 {
		return copyContent
	}
	return bytes.ReplaceAll(copyContent, secret, []byte("[REDACTED]"))
}

func redactSecrets(content []byte, secrets ...[]byte) []byte {
	redacted := append([]byte(nil), content...)
	for _, secret := range secrets {
		redacted = redactBytes(redacted, secret)
	}
	return redacted
}
