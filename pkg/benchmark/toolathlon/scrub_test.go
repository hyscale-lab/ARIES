package toolathlon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func jsonInner(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded[1 : len(encoded)-1])
}

// Every value from the environment is replaced as it is, JSON-escaped, and
// by its lines and JSON string fields; short values are settings and stay.
func TestScrubberReplacesValuesAndTheirParts(t *testing.T) {
	key := "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nBKcwggSjAgEAAoIBAQC7\n-----END PRIVATE KEY-----\n"
	oauth := `{"client_id": "1234567890-abcdef.apps.example.com", "refresh_token": "1//0gRefreshTokenValue", "type": "authorized_user"}`
	scrub := newScrubber([]string{"ghp_exampletoken123", "short", key, oauth})
	for input, want := range map[string]string{
		"Authorization: Bearer ghp_exampletoken123": "Authorization: Bearer <redacted>",
		`{'Authorization': 'ghp_exampletoken123'}`:  `{'Authorization': '<redacted>'}`,
		`{"key": "` + jsonInner(key) + `"}`:         `{"key": "<redacted>"}`,
		"line: MIIEvQIBADANBgkqhkiG9w0BAQEFAASC":    "line: <redacted>",
		"refresh=1//0gRefreshTokenValue&x=1":        "refresh=<redacted>&x=1",
		"client 1234567890-abcdef.apps.example.com": "client <redacted>",
		"the region short and authorized_user stay": "the region short and authorized_user stay",
	} {
		if got := scrub.text(input); got != want {
			t.Errorf("text(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestScrubberFilesAndErrors(t *testing.T) {
	scrub := newScrubber([]string{"ghp_exampletoken123"})
	path := filepath.Join(t.TempDir(), "gateway.log")
	if err := os.WriteFile(path, []byte("Bearer ghp_exampletoken123\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := scrub.file(path); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(content) != "Bearer <redacted>\n" || info.Mode().Perm() != 0o640 {
		t.Fatalf("scrubbed file = %q, mode %v", content, info.Mode().Perm())
	}
	if err := scrub.file(filepath.Join(t.TempDir(), "absent.log")); err != nil {
		t.Fatalf("an artifact never written has nothing to scrub: %v", err)
	}

	err := scrub.err(fmt.Errorf("start gateway: exit code 1: bad token ghp_exampletoken123: %w", context.Canceled))
	if err.Error() != "start gateway: exit code 1: bad token <redacted>: context canceled" || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %q (is canceled %v)", err, errors.Is(err, context.Canceled))
	}
	plain := errors.New("no secret here")
	if scrub.err(plain) != plain || scrub.err(nil) != nil {
		t.Fatal("an error without a value must be returned as it is")
	}

	var none *scrubber
	if none.text("ghp_exampletoken123") != "ghp_exampletoken123" || none.err(plain) != plain || none.file(path) != nil {
		t.Fatal("a nil scrubber must change nothing")
	}
	if newScrubber([]string{"short"}) != nil {
		t.Fatal("values too short to be secrets must not make a scrubber")
	}
}
