package codex

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStaticExecutableRejectsScriptsDynamicELFAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "static")
	writeStaticELF(t, valid)
	if _, err := readStaticExecutable(valid); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, make([]byte, 56)...)
	binary.LittleEndian.PutUint64(content[32:], 64)
	binary.LittleEndian.PutUint16(content[54:], 56)
	binary.LittleEndian.PutUint16(content[56:], 1)
	binary.LittleEndian.PutUint32(content[64:], 3) // PT_INTERP
	dynamic := filepath.Join(dir, "dynamic")
	if err := os.WriteFile(dynamic, content, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dynamic, script, link} {
		if _, err := readStaticExecutable(name); err == nil {
			t.Errorf("accepted invalid executable %s", name)
		}
	}
}

func TestPrivateSourceRejectsFIFOWithoutBlocking(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(filename, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSource(filename, 0o600, maxOutputBytes); err == nil {
		t.Fatal("accepted FIFO as private source")
	}
}
