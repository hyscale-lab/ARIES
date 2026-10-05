package harness

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestStageArchivePreservesNativeModesOwnershipAndOrder(t *testing.T) {
	archive, err := StageArchive([]ArchiveDirectory{{Name: "private", Mode: 0o700, UID: 1000, GID: 1001}}, map[string]ArchiveFile{
		"private/z": {Content: []byte("secret"), Mode: 0o600, UID: 1000, GID: 1001},
		"private/a": {Content: []byte("launcher"), Mode: 0o555, UID: 0, GID: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(archive)
	reader := tar.NewReader(bytes.NewReader(archive))
	for _, want := range []struct {
		name     string
		mode     int64
		uid, gid int
		content  string
	}{{"private", 0o700, 1000, 1001, ""}, {"private/a", 0o555, 0, 0, "launcher"}, {"private/z", 0o600, 1000, 1001, "secret"}} {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != want.name || header.Mode != want.mode || header.Uid != want.uid || header.Gid != want.gid || string(content) != want.content {
			t.Fatalf("entry differs: %+v, %q", header, content)
		}
	}
	for _, name := range []string{"../secret", "/secret", "private/../secret", ".."} {
		if _, err := StageArchive(nil, map[string]ArchiveFile{name: {Content: []byte("secret"), Mode: 0o600}}); err == nil {
			t.Fatalf("unsafe path accepted: %q", name)
		}
	}
}
