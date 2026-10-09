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
	digest     string
	builds     int
	names      []string
	files      map[string]string
	modes      map[string]int64
	present    bool
	dropLabels bool
}

func (f *bridgeImageBuilder) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if f.digest == "" {
		if f.present {
			return client.ImageInspectResult{}, nil
		}
		return client.ImageInspectResult{}, errdefs.ErrNotFound
	}
	var r client.ImageInspectResult
	b, _ := json.Marshal(map[string]any{"Config": map[string]any{"Labels": map[string]string{bridgeBuildLabel: f.digest}}})
	_ = json.Unmarshal(b, &r)
	return r, nil
}
func (f *bridgeImageBuilder) ImageBuild(_ context.Context, r io.Reader, opts client.ImageBuildOptions) (client.ImageBuildResult, error) {
	f.builds++
	f.present = true
	f.digest = opts.Labels[bridgeBuildLabel]
	if f.dropLabels {
		f.digest = ""
	}
	f.names = nil
	f.files = make(map[string]string)
	f.modes = make(map[string]int64)
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
		content, err := io.ReadAll(tr)
		if err != nil {
			return client.ImageBuildResult{}, err
		}
		f.files[h.Name] = string(content)
		f.modes[h.Name] = h.Mode
	}
	return client.ImageBuildResult{Body: io.NopCloser(strings.NewReader("{}\n"))}, nil
}
func TestBridgeImageRebuildsChangedInputsAndCachesExactServer(t *testing.T) {
	files := map[string][]byte{"Dockerfile": []byte("FROM base:v1\n"), "bin/aries-bridge": []byte("bridge v1")}
	fake := new(bridgeImageBuilder)
	ctx := context.Background()
	for range 2 {
		if err := buildBridgeImage(ctx, fake, testImage, files, nil); err != nil {
			t.Fatal(err)
		}
	}
	if fake.builds != 1 {
		t.Fatal("unchanged image not reused")
	}
	files["bin/aries-bridge"] = []byte("bridge v2")
	if err := buildBridgeImage(ctx, fake, testImage, files, nil); err != nil {
		t.Fatal(err)
	}
	if fake.builds != 2 {
		t.Fatal("stale image reused")
	}
	files["Dockerfile"] = []byte("FROM base:v2\n")
	if err := buildBridgeImage(ctx, fake, testImage, files, nil); err != nil {
		t.Fatal(err)
	}
	if fake.builds != 3 {
		t.Fatal("image reused after recipe changed")
	}
	if err := buildBridgeImage(ctx, fake, testImage, files, map[string]string{"BASE_IMAGE": "base:v3"}); err != nil {
		t.Fatal(err)
	}
	if fake.builds != 4 {
		t.Fatal("image reused after build arguments changed")
	}
	if !reflect.DeepEqual(fake.names, []string{"Dockerfile", "bin/aries-bridge"}) {
		t.Fatalf("context %v", fake.names)
	}
	if fake.files["Dockerfile"] != string(files["Dockerfile"]) || fake.files["bin/aries-bridge"] != string(files["bin/aries-bridge"]) {
		t.Fatalf("build context contents changed: %v", fake.files)
	}
	if fake.modes["Dockerfile"] != 0o644 || fake.modes["bin/aries-bridge"] != 0o755 {
		t.Fatalf("build context modes = %v", fake.modes)
	}
}

func TestBridgeImageRequiresMatchingBuildLabel(t *testing.T) {
	files := map[string][]byte{"Dockerfile": []byte("FROM base:v1\n"), "bin/aries-bridge": []byte("bridge")}
	t.Run("rebuild cached image without label", func(t *testing.T) {
		fake := &bridgeImageBuilder{present: true}
		if err := buildBridgeImage(context.Background(), fake, testImage, files, nil); err != nil {
			t.Fatal(err)
		}
		if fake.builds != 1 || fake.digest == "" {
			t.Fatal("unidentified cached bridge image was reused")
		}
	})
	t.Run("reject build without expected label", func(t *testing.T) {
		fake := &bridgeImageBuilder{dropLabels: true}
		if err := buildBridgeImage(context.Background(), fake, testImage, files, nil); err == nil || !strings.Contains(err.Error(), "does not match requested build labels") {
			t.Fatalf("build confirmation error = %v", err)
		}
	})
}
func TestBridgeContextRejectsUnrequestedFiles(t *testing.T) {
	files := map[string][]byte{"Dockerfile": []byte("FROM base:v1"), "bin/aries-bridge": []byte("bridge"), "secret": []byte("secret")}
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
