package main

import (
	"os"
	"path/filepath"
)

const ariesExecutableName = "aries"

// repositoryAPIKeyPath returns the optional repository-local key path used by
// the documented bin/aries quick start. Other executable layouts use the
// configured environment variable instead.
func repositoryAPIKeyPath(executablePath string) (string, bool) {
	if executablePath == "" {
		var err error
		executablePath, err = os.Executable()
		if err != nil {
			return "", false
		}
	}
	if !filepath.IsAbs(executablePath) || filepath.Clean(executablePath) != executablePath {
		return "", false
	}
	if filepath.Base(executablePath) != ariesExecutableName {
		return "", false
	}
	binDirectory := filepath.Dir(executablePath)
	if filepath.Base(binDirectory) != "bin" {
		return "", false
	}
	return filepath.Join(filepath.Dir(binDirectory), localAPIKeyFile), true
}
