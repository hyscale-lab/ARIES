package main

import (
	"path/filepath"
	"testing"
)

func TestRepositoryAPIKeyPathUsesOnlyAbsoluteBinAriesLayout(t *testing.T) {
	repository := t.TempDir()
	executable := filepath.Join(repository, "bin", ariesExecutableName)
	path, ok := repositoryAPIKeyPath(executable)
	if !ok || path != filepath.Join(repository, localAPIKeyFile) {
		t.Fatalf("repositoryAPIKeyPath(%q) = %q, %v", executable, path, ok)
	}

	for _, candidate := range []string{
		filepath.Join("bin", ariesExecutableName),
		filepath.Join(repository, "other", ariesExecutableName),
		filepath.Join(repository, "bin", "renamed"),
	} {
		if path, ok := repositoryAPIKeyPath(candidate); ok || path != "" {
			t.Fatalf("repositoryAPIKeyPath(%q) = %q, %v; want no local key", candidate, path, ok)
		}
	}
}

func TestRepositoryAPIKeyPathDoesNotRequireRepositoryMarkers(t *testing.T) {
	repository := t.TempDir()
	executable := filepath.Join(repository, "bin", ariesExecutableName)
	path, ok := repositoryAPIKeyPath(executable)
	if !ok || path != filepath.Join(repository, localAPIKeyFile) {
		t.Fatalf("repositoryAPIKeyPath() = %q, %v", path, ok)
	}
}
