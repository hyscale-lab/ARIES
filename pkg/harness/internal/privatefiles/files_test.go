package privatefiles

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactPrivateAndImmutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence", "result.json")
	content := []byte(`{"result":"ok"}`)
	if err := WriteArtifact(path, content); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: stat=%v error=%v", path, info, err)
		}
	}
	if err := WriteArtifact(path, content); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if err := WriteArtifact(path, []byte("replacement")); err == nil {
		t.Fatal("overwrote evidence")
	}
	got, err := Read(path, 0o600)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("evidence changed: %q, %v", got, err)
	}
}

func TestPrivateFileRejectsUnsafeSources(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "key")
	if err := os.WriteFile(original, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(original, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link, 0o600); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Chmod(original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(original, 0o600); err == nil {
		t.Fatal("accepted public credential")
	}
	if err := os.Chmod(original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(original, maxPrivateFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(original, 0o600); err == nil {
		t.Fatal("accepted oversized file")
	}
	if err := os.Truncate(original, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(original, 0o600); err == nil {
		t.Fatal("accepted empty credential")
	}
}

func TestArtifactRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "private")
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(dest, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteArtifact(filepath.Join(link, "result"), []byte("private")); err == nil {
		t.Fatal("accepted symlinked artifact directory")
	}
	if _, err := os.Stat(filepath.Join(dest, "result")); !os.IsNotExist(err) {
		t.Fatalf("wrote through symlink: %v", err)
	}
}

func TestRedactionCopiesAndHandlesJSONEscaping(t *testing.T) {
	secret := []byte("key\"with\\escapes")
	encoded, err := json.Marshal(string(secret))
	if err != nil {
		t.Fatal(err)
	}
	input := append(append([]byte(nil), secret...), encoded...)
	original := append([]byte(nil), input...)
	got := RedactSecrets(input, secret, []byte("other"))
	if bytes.Contains(got, secret) || bytes.Contains(got, encoded[1:len(encoded)-1]) {
		t.Fatalf("secret leaked: %q", got)
	}
	if !bytes.Equal(input, original) {
		t.Fatal("redaction modified caller buffer")
	}
	copy := Redact(input, nil)
	copy[0] = '!'
	if !bytes.Equal(input, original) {
		t.Fatal("no-secret redaction aliases caller buffer")
	}
}

func TestFilterLogsDropsCredentialLines(t *testing.T) {
	input := []byte("safe secret\nAuthorization: redacted\nbearer token\ninvalid\x00line\n" + string(bytes.Repeat([]byte("x"), 4097)) + "\nlast")
	before := bytes.Clone(input)
	got := FilterLogs(input, 1<<20, []byte("secret"))
	if string(got) != "safe [REDACTED]\nlast\n" {
		t.Fatalf("filtered logs: %q", got)
	}
	if !bytes.Equal(input, before) {
		t.Fatal("modified caller logs")
	}
	if got := string(FilterLogs([]byte("one\ntwo"), 4)); got != "one\n" {
		t.Fatalf("output bound: %q", got)
	}
}
