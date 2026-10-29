package vhive

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Linux sockaddr_un limits filesystem paths to 107 bytes. Resolving the parent
// through an open directory preserves the original socket without process-wide
// chdir or a persistent symlink. Keep closeDir alive through all socket dials.
func shortUnixSocketPath(path string) (short string, closeDir func(), err error) {
	if len(path) <= 107 {
		return path, func() {}, nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return "", nil, fmt.Errorf("open Unix socket directory: %w", err)
	}
	short = fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(path))
	if len(short) > 107 {
		_ = dir.Close()
		return "", nil, fmt.Errorf("Unix socket basename exceeds address limit: %q", filepath.Base(path))
	}
	var once sync.Once
	return short, func() { once.Do(func() { _ = dir.Close() }) }, nil
}
