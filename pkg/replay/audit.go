package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The bridge audit writer bounds both logs to 256 MiB in total.
const (
	maxTimingAuditBytes   = 256 << 20
	privateTimingFileMode = 0o600
)

func readPrivateAudit(path string, live bool, decode func(*json.Decoder) error) error {
	label := "replay bridge audit"
	if live {
		label = "live bridge audit"
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		if live {
			return fmt.Errorf("open %s directory: %w", label, err)
		}
		return errors.New(label + " is unavailable")
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil && live {
		return fmt.Errorf("stat %s: %w", label, err)
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != privateTimingFileMode || info.Size() < 0 || info.Size() > maxTimingAuditBytes {
		return errors.New(label + " is not a bounded private file")
	}
	file, err := root.Open(name)
	if err != nil {
		if live {
			return fmt.Errorf("open %s: %w", label, err)
		}
		return errors.New(label + " is unavailable")
	}
	defer file.Close()
	if !auditUnchanged(file, info) {
		return errors.New(label + " changed before reading")
	}
	if err := decode(json.NewDecoder(io.LimitReader(file, maxTimingAuditBytes+1))); err != nil {
		return err
	}
	if !auditUnchanged(file, info) {
		return errors.New(label + " changed while reading")
	}
	return nil
}

func auditUnchanged(file *os.File, before os.FileInfo) bool {
	after, err := file.Stat()
	return err == nil && os.SameFile(before, after) && after.Size() == before.Size() && after.Mode().Perm() == before.Mode().Perm()
}
