package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLocalAPIKeySourceAcceptsBoundedSingleLineAndOneTerminalLF(t *testing.T) {
	for _, test := range []struct {
		name    string
		content []byte
		want    []byte
		mode    os.FileMode
	}{
		{name: "read write", content: []byte("synthetic-test-key"), want: []byte("synthetic-test-key"), mode: 0o600},
		{name: "read only", content: []byte("synthetic-test-key"), want: []byte("synthetic-test-key"), mode: 0o400},
		{name: "terminal LF", content: []byte("synthetic-test-key\n"), want: []byte("synthetic-test-key"), mode: 0o600},
		{name: "maximum", content: bytes.Repeat([]byte{'x'}, maxAPIKeyBytes), want: bytes.Repeat([]byte{'x'}, maxAPIKeyBytes), mode: 0o600},
		{name: "maximum plus LF", content: append(bytes.Repeat([]byte{'x'}, maxAPIKeyBytes), '\n'), want: bytes.Repeat([]byte{'x'}, maxAPIKeyBytes), mode: 0o600},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writePrivateKey(t, test.content, test.mode)
			source, exists, err := loadLocalAPIKeySource(path)
			if err != nil || !exists || source == nil {
				t.Fatalf("loadLocalAPIKeySource() = %v, %v, %v", source, exists, err)
			}
			defer source.Clear()
			got, ok := source.Lookup(deepSeekAPIKey)
			if !ok || !bytes.Equal(got, test.want) {
				t.Fatalf("Lookup() = %q, %v", got, ok)
			}
			clear(got)
		})
	}
}

func TestLoadLocalAPIKeySourceRejectsInvalidFilesWithoutEchoingContent(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		mode    os.FileMode
	}{
		{name: "empty", content: nil, mode: 0o600},
		{name: "only LF", content: []byte("\n"), mode: 0o600},
		{name: "NUL", content: []byte("synthetic-secret\x00tail"), mode: 0o600},
		{name: "CR", content: []byte("synthetic-secret\r"), mode: 0o600},
		{name: "embedded LF", content: []byte("synthetic-secret\ntail"), mode: 0o600},
		{name: "two terminal LFs", content: []byte("synthetic-secret\n\n"), mode: 0o600},
		{name: "oversized", content: bytes.Repeat([]byte{'s'}, maxAPIKeyBytes+2), mode: 0o600},
		{name: "wrong mode", content: []byte("synthetic-secret"), mode: 0o640},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writePrivateKey(t, test.content, test.mode)
			source, exists, err := loadLocalAPIKeySource(path)
			if err == nil || exists || source != nil {
				t.Fatalf("loadLocalAPIKeySource() = %v, %v, %v; want rejection", source, exists, err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatalf("error exposed key content: %v", err)
			}
		})
	}
}

func TestLoadLocalAPIKeySourceRejectsNonRegularAndSymbolicLinks(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "key-directory")
	if err := os.Mkdir(directory, 0o600); err != nil {
		t.Fatal(err)
	}
	if source, exists, err := loadLocalAPIKeySource(directory); err == nil || exists || source != nil {
		t.Fatalf("directory result = %v, %v, %v", source, exists, err)
	}

	target := writePrivateKey(t, []byte("synthetic-secret"), 0o600)
	link := filepath.Join(t.TempDir(), "key-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if source, exists, err := loadLocalAPIKeySource(link); err == nil || exists || source != nil {
		t.Fatalf("symlink result = %v, %v, %v", source, exists, err)
	}
}

func TestLoadLocalAPIKeySourceMissingUsesFallback(t *testing.T) {
	source, exists, err := loadLocalAPIKeySource(filepath.Join(t.TempDir(), "missing"))
	if err != nil || exists || source != nil {
		t.Fatalf("loadLocalAPIKeySource() = %v, %v, %v", source, exists, err)
	}
}

func TestAPIKeySourceIsExactClonedClearedAndDoesNotChangeEnvironment(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "environment-value")
	source, exists, err := loadLocalAPIKeySource(writePrivateKey(t, []byte("file-value"), 0o600))
	if err != nil || !exists {
		t.Fatal(err)
	}
	if value, ok := source.Lookup("OTHER_KEY"); ok || value != nil {
		t.Fatalf("lookup answered an unexpected name: %q, %v", value, ok)
	}
	first, ok := source.Lookup(deepSeekAPIKey)
	if !ok {
		t.Fatal("expected key lookup")
	}
	first[0] = 'X'
	second, ok := source.Lookup(deepSeekAPIKey)
	if !ok || string(second) != "file-value" {
		t.Fatalf("lookup did not return an independent clone: %q, %v", second, ok)
	}
	backing := source.value[:cap(source.value)]
	source.Clear()
	if !bytes.Equal(backing, make([]byte, len(backing))) {
		t.Fatal("source bytes were not zeroed")
	}
	if value, ok := source.Lookup(deepSeekAPIKey); ok || value != nil {
		t.Fatalf("cleared source remained available: %q, %v", value, ok)
	}
	if got := os.Getenv(deepSeekAPIKey); got != "environment-value" {
		t.Fatalf("environment changed: %q", got)
	}
	clear(first)
	clear(second)
}

func TestValidateKeyFileRequiresCurrentUserOwnerOnlyRegularFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o600} {
		path := writePrivateKey(t, []byte("synthetic-secret"), mode)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateKeyFile(info); err != nil {
			t.Fatalf("validateKeyFile(mode %04o): %v", mode, err)
		}
	}

	for _, mode := range []os.FileMode{0o440, 0o404, 0o640} {
		path := writePrivateKey(t, []byte("synthetic-secret"), mode)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateKeyFile(info); err == nil {
			t.Fatalf("validateKeyFile accepted mode %04o", mode)
		}
	}
}

func writePrivateKey(t *testing.T, content []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), localAPIKeyFile)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}
