package privatefiles

import (
	"bytes"
	"encoding/json"
)

// Redact copies content and removes literal and JSON-escaped forms of a secret.
func Redact(content, secret []byte) []byte {
	copyContent := append([]byte(nil), content...)
	if len(secret) == 0 {
		return copyContent
	}
	copyContent = bytes.ReplaceAll(copyContent, secret, []byte("[REDACTED]"))
	encoded, err := json.Marshal(string(secret))
	if err == nil && len(encoded) >= 2 {
		escaped := encoded[1 : len(encoded)-1]
		if !bytes.Equal(escaped, secret) {
			copyContent = bytes.ReplaceAll(copyContent, escaped, []byte("[REDACTED]"))
		}
	}
	return copyContent
}

// RedactSecrets applies redaction without modifying caller-owned buffers.
func RedactSecrets(content []byte, secrets ...[]byte) []byte {
	redacted := append([]byte(nil), content...)
	for _, secret := range secrets {
		redacted = Redact(redacted, secret)
	}
	return redacted
}

// FilterLogs redacts secrets and drops credential-bearing or oversized lines.
func FilterLogs(content []byte, maxOutput int, secrets ...[]byte) []byte {
	content = RedactSecrets(content, secrets...)
	lines := bytes.Split(content, []byte("\n"))
	var output bytes.Buffer
	for _, line := range lines {
		lower := bytes.ToLower(line)
		if len(line) > 4096 || bytes.ContainsRune(line, 0) || bytes.Contains(lower, []byte("authorization:")) || bytes.Contains(lower, []byte("bearer ")) {
			continue
		}
		output.Write(line)
		output.WriteByte('\n')
		if output.Len() >= maxOutput {
			break
		}
	}
	return output.Bytes()
}
