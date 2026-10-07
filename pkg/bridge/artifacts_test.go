package bridge

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

type archiveRuntime struct {
	deployment.Runtime
	data []byte
	size int64
}

func (r archiveRuntime) DownloadArchive(context.Context, string, string) (io.ReadCloser, deployment.FileInfo, error) {
	return io.NopCloser(bytes.NewReader(r.data)), deployment.FileInfo{Mode: fs.FileMode(0600), Size: r.size}, nil
}
func TestArtifactCollectionRejectsSubstitutionAndUnexpectedEntries(t *testing.T) {
	content := []byte("evidence\n")
	hash := sha256.Sum256(content)
	entry := Artifact{Name: "tool-calls.jsonl", Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:])}
	for _, tc := range []struct {
		name, member   string
		kind           byte
		extra, badHash bool
		wantErr        bool
	}{
		{name: "valid", member: entry.Name, kind: tar.TypeReg},
		{name: "traversal", member: "../" + entry.Name, kind: tar.TypeReg, wantErr: true},
		{name: "symlink", member: entry.Name, kind: tar.TypeSymlink, wantErr: true},
		{name: "extra file", member: entry.Name, kind: tar.TypeReg, extra: true, wantErr: true},
		{name: "substitution", member: entry.Name, kind: tar.TypeReg, badHash: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			tw := tar.NewWriter(&data)
			size := int64(len(content))
			if tc.kind != tar.TypeReg {
				size = 0
			}
			if err := tw.WriteHeader(&tar.Header{Name: tc.member, Size: size, Mode: 0600, Typeflag: tc.kind, Linkname: "secret"}); err != nil {
				t.Fatal(err)
			}
			if size > 0 {
				_, _ = tw.Write(content)
			}
			if tc.extra {
				_ = tw.WriteHeader(&tar.Header{Name: "extra", Typeflag: tar.TypeReg, Mode: 0600})
			}
			_ = tw.Close()
			expected := entry
			if tc.badHash {
				expected.SHA256 = "bad"
			}
			err := collectArtifact(context.Background(), archiveRuntime{data: data.Bytes(), size: entry.Size}, "runtime", "remote", t.TempDir(), expected)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
func TestFinalizeArtifactsRejectsMissingAndSymlinkEvidence(t *testing.T) {
	root := t.TempDir()
	if _, err := finalizeArtifacts(root, []string{"tool-calls.jsonl"}); err == nil {
		t.Fatal("missing evidence accepted")
	}
	path := filepath.Join(root, "tool-calls.jsonl")
	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeArtifacts(root, []string{"tool-calls.jsonl"}); err == nil {
		t.Fatal("symlink evidence accepted")
	}
}
