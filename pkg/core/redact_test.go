package core

import (
	"bytes"
	"slices"
	"testing"
)

func TestSecretPartsSplitsKeysAndKeyFiles(t *testing.T) {
	if parts := SecretParts("us-east1"); !slices.Equal(parts, []string{"us-east1"}) {
		t.Fatalf("an 8-character value = %q", parts)
	}
	if parts := SecretParts("1"); parts != nil {
		t.Fatalf("a setting was taken as a secret: %q", parts)
	}
	key := "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEF\nshort\n-----END PRIVATE KEY-----\n"
	parts := SecretParts(key)
	for _, want := range []string{key, "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEF\nshort\n-----END PRIVATE KEY-----", "MIIEvQIBADANBgkqhkiG9w0BAQEF"} {
		if !slices.Contains(parts, want) {
			t.Fatalf("parts of a private key lack %q: %q", want, parts)
		}
	}
	if slices.Contains(parts, "short") {
		t.Fatalf("a line too short to identify the key was kept: %q", parts)
	}
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) > len(parts[i-1]) {
			t.Fatalf("parts are not longest first: %q", parts)
		}
	}
	oauth := `{"client_id": "1234567890-abcdef.apps.example.com", "type": "authorized_user", "nested": {"refresh_token": "1//0gRefreshTokenValue"}}`
	parts = SecretParts(oauth)
	for _, want := range []string{oauth, "1234567890-abcdef.apps.example.com", "1//0gRefreshTokenValue"} {
		if !slices.Contains(parts, want) {
			t.Fatalf("parts of a key file lack %q: %q", want, parts)
		}
	}
	if slices.Contains(parts, "authorized_user") {
		t.Fatalf("a field too short to identify the file was kept: %q", parts)
	}
}

func TestLookupSecretPartsClearsBuffersAndSkipsUnset(t *testing.T) {
	var handed [][]byte
	lookup := func(name string) ([]byte, bool) {
		switch name {
		case "TOKEN":
			buffer := []byte("ghp_exampletoken123")
			handed = append(handed, buffer)
			return buffer, true
		case "SETTING":
			buffer := []byte("1")
			handed = append(handed, buffer)
			return buffer, true
		}
		return nil, false
	}
	parts := LookupSecretParts(lookup, []string{"UNSET", "TOKEN", "SETTING"})
	if len(parts) != 1 || string(parts[0]) != "ghp_exampletoken123" {
		t.Fatalf("parts = %q", parts)
	}
	for _, buffer := range handed {
		if len(bytes.Trim(buffer, "\x00")) != 0 {
			t.Fatalf("a looked-up buffer was not cleared: %q", buffer)
		}
	}
	if LookupSecretParts(nil, []string{"TOKEN"}) != nil {
		t.Fatal("a nil lookup must yield nothing")
	}
}
