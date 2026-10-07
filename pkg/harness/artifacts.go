package harness

import (
	"archive/tar"
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
)

// ArchiveDirectory declares native runtime layout and ownership explicitly.
type ArchiveDirectory struct {
	Name     string
	Mode     int64
	UID, GID int
}

// ArchiveFile borrows Content until StageArchive returns.
type ArchiveFile struct {
	Content  []byte
	Mode     int64
	UID, GID int
}

// StageArchive writes directories in supplied order and files in sorted order.
// The caller owns and must clear the returned archive after upload.
func StageArchive(directories []ArchiveDirectory, files map[string]ArchiveFile) (archive []byte, err error) {
	var output bytes.Buffer
	defer func() {
		if err != nil {
			clear(output.Bytes())
		}
	}()
	writer := tar.NewWriter(&output)
	for _, directory := range directories {
		if err = validateArchivePath(directory.Name); err != nil {
			return nil, err
		}
		if err = writer.WriteHeader(&tar.Header{Name: directory.Name, Typeflag: tar.TypeDir, Mode: directory.Mode, Uid: directory.UID, Gid: directory.GID}); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err = validateArchivePath(name); err != nil {
			return nil, err
		}
		file := files[name]
		if err = writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: file.Mode, Size: int64(len(file.Content)), Uid: file.UID, Gid: file.GID}); err != nil {
			return nil, err
		}
		if _, err = writer.Write(file.Content); err != nil {
			return nil, err
		}
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func validateArchivePath(name string) error {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "../") {
		return fmt.Errorf("invalid staged path %q", name)
	}
	return nil
}

// Artifacts owns private evidence paths for one occurrence or Run snapshot.
// Native collectors retain their own formats and filtering policies.
type Artifacts struct {
	Directory string
	paths     []string
}

func (a *Artifacts) Write(name string, content []byte) error {
	if err := validateArchivePath(name); err != nil {
		return err
	}
	path := filepath.Join(a.Directory, name)
	if err := privatefiles.WriteArtifact(path, content); err != nil {
		return err
	}
	a.Add(path)
	return nil
}

// Add records successfully collected evidence once, preserving collection order.
func (a *Artifacts) Add(paths ...string) {
	for _, path := range paths {
		if path != "" && !slices.Contains(a.paths, path) {
			a.paths = append(a.paths, path)
		}
	}
}
func (a *Artifacts) Paths() []string     { return append([]string(nil), a.paths...) }
func (a *Artifacts) Snapshot() Artifacts { return Artifacts{Directory: a.Directory, paths: a.Paths()} }

// TelemetryPaths returns the relative paths used by the native telemetry index.
func (a *Artifacts) TelemetryPaths() []string {
	prefix := filepath.Join(a.Directory, "telemetry") + string(filepath.Separator)
	var relative []string
	for _, path := range a.paths {
		if strings.HasPrefix(path, prefix) {
			name, err := filepath.Rel(a.Directory, path)
			if err == nil {
				relative = append(relative, filepath.ToSlash(name))
			}
		}
	}
	return relative
}
