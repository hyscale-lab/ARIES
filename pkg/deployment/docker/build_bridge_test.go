package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

type bridgeImageBuilder struct {
	digest string
	builds int
	names  []string
}

func (f *bridgeImageBuilder) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if f.digest == "" {
		return client.ImageInspectResult{}, errdefs.ErrNotFound
	}
	var r client.ImageInspectResult
	b, _ := json.Marshal(map[string]any{"Config": map[string]any{"Labels": map[string]string{bridgeBuildLabel: f.digest}}})
	_ = json.Unmarshal(b, &r)
	return r, nil
}
func (f *bridgeImageBuilder) ImageBuild(_ context.Context, r io.Reader, opts client.ImageBuildOptions) (client.ImageBuildResult, error) {
	f.builds++
	f.digest = opts.Labels[bridgeBuildLabel]
	f.names = nil
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return client.ImageBuildResult{}, err
		}
		f.names = append(f.names, h.Name)
	}
	return client.ImageBuildResult{Body: io.NopCloser(strings.NewReader("{}\n"))}, nil
}
func TestBridgeImageRebuildsChangedBinaryAndCachesExactPair(t *testing.T) {
	files := map[string][]byte{"Dockerfile": []byte("FROM base:v1\n"), "bin/aries-bridge": []byte("bridge v1"), "bin/aries-ssh-client": []byte("ssh v1")}
	fake := new(bridgeImageBuilder)
	ctx := context.Background()
	for range 2 {
		if err := buildBridgeImage(ctx, fake, testImage, files, nil); err != nil {
			t.Fatal(err)
		}
	}
	if fake.builds != 1 {
		t.Fatal("matched image not reused")
	}
	files["bin/aries-bridge"] = []byte("bridge v2")
	if err := buildBridgeImage(ctx, fake, testImage, files, nil); err != nil {
		t.Fatal(err)
	}
	if fake.builds != 2 {
		t.Fatal("stale image reused")
	}
	if !reflect.DeepEqual(fake.names, []string{"Dockerfile", "bin/aries-bridge", "bin/aries-ssh-client"}) {
		t.Fatalf("context %v", fake.names)
	}
}
func TestBridgeContextRejectsUnrequestedFiles(t *testing.T) {
	files := map[string][]byte{"Dockerfile": []byte("FROM base:v1"), "bin/aries-bridge": []byte("bridge"), "bin/aries-ssh-client": []byte("ssh"), "secret": []byte("secret")}
	if _, _, err := bridgeBuildContext(files, nil); err == nil {
		t.Fatal("extra files accepted")
	}
	delete(files, "secret")
	_, a, err := bridgeBuildContext(files, map[string]string{"BASE": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := bridgeBuildContext(files, map[string]string{"BASE": "v2"})
	if err != nil || a == b {
		t.Fatal("build arguments not included in image identity")
	}
}
