package openclaw

import (
	"bytes"
)

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
