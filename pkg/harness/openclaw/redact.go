package openclaw

import (
	"bytes"
	"encoding/json"
)

func redactBytes(content, secret []byte) []byte {
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

func redactSecrets(content []byte, secrets ...[]byte) []byte {
	redacted := append([]byte(nil), content...)
	for _, secret := range secrets {
		redacted = redactBytes(redacted, secret)
	}
	return redacted
}
