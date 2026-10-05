package privatefiles

import (
	"crypto/rand"
	"encoding/hex"
)

// RandomID returns a cryptographically random 16-character hexadecimal identifier.
func RandomID() (string, error) {
	var content [8]byte
	if _, err := rand.Read(content[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(content[:]), nil
}

// RandomSecret returns hexadecimal credential bytes from size random bytes.
func RandomSecret(size int) ([]byte, error) {
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		clear(content)
		return nil, err
	}
	encoded := make([]byte, hex.EncodedLen(len(content)))
	hex.Encode(encoded, content)
	clear(content)
	return encoded, nil
}
