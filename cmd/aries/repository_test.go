package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAriesRepositoryRootAcceptsOnlyExactBinLayoutAndModule(t *testing.T) {
	root, executable := createTestAriesRepository(t)
	got, err := ariesRepositoryRoot(executable)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("repository root = %q, want %q", got, root)
	}
}

func TestAriesRepositoryRootFailsClosedWithoutScanningParentsOrCWD(t *testing.T) {
	t.Run("relative executable", func(t *testing.T) {
		if _, err := ariesRepositoryRoot(filepath.Join("bin", ariesExecutableName)); err == nil {
			t.Fatal("relative executable accepted")
		}
	})
	t.Run("wrong executable name", func(t *testing.T) {
		root, _ := createTestAriesRepository(t)
		path := filepath.Join(root, "bin", "renamed")
		if err := os.WriteFile(path, []byte("synthetic"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ariesRepositoryRoot(path); err == nil {
			t.Fatal("renamed executable accepted")
		}
	})
	t.Run("wrong directory", func(t *testing.T) {
		root, _ := createTestAriesRepository(t)
		directory := filepath.Join(root, "other")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, ariesExecutableName)
		if err := os.WriteFile(path, []byte("synthetic"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ariesRepositoryRoot(path); err == nil {
			t.Fatal("executable outside bin accepted")
		}
	})
	t.Run("does not scan parent", func(t *testing.T) {
		outer := t.TempDir()
		if err := os.WriteFile(filepath.Join(outer, "go.mod"), []byte(ariesModuleLine+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		inner := filepath.Join(outer, "nested")
		bin := filepath.Join(inner, "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		executable := filepath.Join(bin, ariesExecutableName)
		if err := os.WriteFile(executable, []byte("synthetic"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ariesRepositoryRoot(executable); err == nil || !strings.Contains(err.Error(), "go.mod") {
			t.Fatalf("parent marker was scanned or wrong error returned: %v", err)
		}
	})
	t.Run("wrong module", func(t *testing.T) {
		root, executable := createTestAriesRepository(t)
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/not-aries\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ariesRepositoryRoot(executable); err == nil {
			t.Fatal("wrong module accepted")
		}
	})
	t.Run("symlinked executable", func(t *testing.T) {
		_, executable := createTestAriesRepository(t)
		linkRoot := t.TempDir()
		bin := filepath.Join(linkRoot, "bin")
		if err := os.Mkdir(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(bin, ariesExecutableName)
		if err := os.Symlink(executable, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ariesRepositoryRoot(link); err == nil {
			t.Fatal("symlinked executable accepted")
		}
	})
	t.Run("symlinked go.mod", func(t *testing.T) {
		root, executable := createTestAriesRepository(t)
		target := filepath.Join(t.TempDir(), "go.mod")
		if err := os.WriteFile(target, []byte(ariesModuleLine+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "go.mod")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "go.mod")); err != nil {
			t.Fatal(err)
		}
		if _, err := ariesRepositoryRoot(executable); err == nil {
			t.Fatal("symlinked go.mod accepted")
		}
	})
}
