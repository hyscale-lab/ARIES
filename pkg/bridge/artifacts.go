package bridge

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

const maxArtifactBytes int64 = 256 << 20

// Artifact records finalized evidence. Missing and failed evidence cannot count
// as a successful revocation collection.
type Artifact struct {
	Name   string
	Size   int64
	SHA256 string
}

func evidenceName(name string) bool { return name == "tool-calls.jsonl" || name == "ssh_raw.log" }
func finalizeArtifacts(root string, names []string) ([]Artifact, error) {
	result := make([]Artifact, 0, len(names))
	for _, name := range names {
		if !evidenceName(name) {
			return nil, errors.New("unexpected evidence name")
		}
		f, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		info, err := f.Lstat(name)
		if err != nil {
			f.Close()
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxArtifactBytes {
			f.Close()
			return nil, errors.New("invalid evidence file")
		}
		content, err := f.ReadFile(name)
		f.Close()
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(content)
		result = append(result, Artifact{Name: name, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:])})
	}
	return result, nil
}
func archiveFiles(files map[string][]byte) ([]byte, error) {
	var buffer bytes.Buffer
	w := tar.NewWriter(&buffer)
	for name, content := range files {
		if filepath.Base(name) != name || name == "." || name == ".." {
			return nil, errors.New("invalid staged file name")
		}
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := w.Write(content); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
func collectArtifact(ctx context.Context, runtime deployment.Runtime, id, remote, local string, entry Artifact) error {
	if !evidenceName(entry.Name) || entry.Size < 0 || entry.Size > maxArtifactBytes {
		return errors.New("invalid evidence manifest")
	}
	stream, info, err := runtime.DownloadArchive(ctx, id, filepath.Join(remote, entry.Name))
	if err != nil {
		return err
	}
	defer stream.Close()
	if !info.Mode.IsRegular() || info.Size != entry.Size {
		return errors.New("evidence stat does not match manifest")
	}
	tr := tar.NewReader(io.LimitReader(stream, maxArtifactBytes+4096))
	header, err := tr.Next()
	if err != nil {
		return err
	}
	if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != entry.Name || header.Name != entry.Name || header.Size != entry.Size {
		return errors.New("invalid evidence archive entry")
	}
	content, err := io.ReadAll(io.LimitReader(tr, entry.Size+1))
	if err != nil {
		return err
	}
	if int64(len(content)) != entry.Size {
		return errors.New("evidence length mismatch")
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != entry.SHA256 {
		return errors.New("evidence digest mismatch")
	}
	if _, err := tr.Next(); err != io.EOF {
		return errors.New("unexpected additional evidence archive entry")
	}
	if err := os.WriteFile(filepath.Join(local, entry.Name), content, 0600); err != nil {
		return fmt.Errorf("materialize evidence: %w", err)
	}
	return nil
}
