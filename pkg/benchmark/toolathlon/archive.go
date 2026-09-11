package toolathlon

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// projectEntries are the checkout paths Toolathlon's decoupled runner copies
// into a fresh task container before anything runs (its FILES_TO_COPY).
// The task image already carries the Python environment and an older copy of
// the same tree; copying the pinned tree over it is what makes the run
// reproduce the pinned revision rather than the image build.
//
// Omitted from the upstream list on purpose: deployment/k8s and
// local_binary/github-mcp-server serve the k8s and GitHub servers, which the
// adapter rejects at task load (18 MB of binary for nothing), and
// deployment/canvas/logs does not exist in the pinned checkout.
var projectEntries = []string{
	"configs",
	"scripts",
	"utils",
	"main.py",
	"global_preparation/check_installation.py",
}

// skippedNames are never archived: caches, and the OAuth cache Toolathlon
// bind-mounts for its credentialed cloud servers.
var skippedNames = map[string]struct{}{
	"__pycache__": {},
	".mcp-auth":   {},
	".git":        {},
}

// writeProjectArchive writes one tar archive of the project entries plus the
// task directory, with members relative to the checkout root, so that
// extracting it at workspaceRoot reproduces the runner's copy step.
func writeProjectArchive(root, taskName, destination string) (err error) {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create project archive: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close project archive: %w", closeErr)
		}
	}()
	writer := tar.NewWriter(file)
	entries := append([]string(nil), projectEntries...)
	entries = append(entries, path.Join("tasks", taskPool, taskName))
	for _, entry := range entries {
		if err := archiveEntry(writer, root, entry); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish project archive: %w", err)
	}
	return nil
}

func archiveEntry(writer *tar.Writer, root, entry string) error {
	source := filepath.Join(root, filepath.FromSlash(entry))
	return filepath.WalkDir(source, func(current string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %q: %w", entry, walkErr)
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return fmt.Errorf("relativize %q: %w", current, err)
		}
		name := filepath.ToSlash(relative)
		if _, skip := skippedNames[dirEntry.Name()]; skip {
			if dirEntry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := dirEntry.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", name, err)
		}
		switch {
		case info.IsDir():
			return writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()})
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(current)
			if err != nil {
				return fmt.Errorf("read symlink %q: %w", name, err)
			}
			if path.IsAbs(target) || strings.HasPrefix(target, "../") || target == ".." {
				return fmt.Errorf("symlink %q escapes the checkout", name)
			}
			return writer.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return fmt.Errorf("write header %q: %w", name, err)
			}
			content, err := os.Open(current)
			if err != nil {
				return fmt.Errorf("open %q: %w", name, err)
			}
			defer content.Close()
			if _, err := io.Copy(writer, content); err != nil {
				return fmt.Errorf("archive %q: %w", name, err)
			}
			return nil
		default:
			return fmt.Errorf("%q is neither a regular file, directory, nor symlink", name)
		}
	})
}

// archiveMemberNames lists the members of a tar archive, for tests and for
// the stash manifest.
func archiveMemberNames(archive string) ([]string, error) {
	file, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := tar.NewReader(file)
	var names []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names = append(names, header.Name)
	}
}
