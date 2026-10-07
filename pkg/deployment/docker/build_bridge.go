package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/moby/moby/client"
)

const bridgeBuildLabel = "aries.bridge.build-sha256"
const maxBridgeBinary = 256 << 20

// BuildBridgeImage builds a matched pair of local ARIES executables. Its context
// contains only these two bounded regular files and the supplied Dockerfile.
func BuildBridgeImage(ctx context.Context, socket, image, dockerfile, bridgeBinary, sshBinary string, buildArgs map[string]string) error {
	files := map[string][]byte{"Dockerfile": []byte(dockerfile)}
	for name, path := range map[string]string{"bin/aries-bridge": bridgeBinary, "bin/aries-ssh": sshBinary} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxBridgeBinary {
			return errors.New("bridge image requires bounded regular executable files")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, maxBridgeBinary+1))
		closeErr := f.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if len(b) == 0 || len(b) > maxBridgeBinary {
			return errors.New("bridge binary exceeds build bound")
		}
		files[name] = b
	}
	host := socket
	if host == "" {
		host = defaultDockerSocket
	}
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-bridge-setup/1"))
	if err != nil {
		return err
	}
	return errors.Join(buildBridgeImage(ctx, api, image, files, buildArgs), api.Close())
}
func bridgeBuildContext(files map[string][]byte, args map[string]string) ([]byte, string, error) {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, name := range []string{"Dockerfile", "bin/aries-bridge", "bin/aries-ssh"} {
		content, ok := files[name]
		if !ok || len(content) == 0 || len(content) > maxBridgeBinary {
			return nil, "", errors.New("bridge context is incomplete or exceeds bounds")
		}
		mode := int64(0755)
		if name == "Dockerfile" {
			mode = 0644
		}
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			return nil, "", err
		}
		if _, err := w.Write(content); err != nil {
			return nil, "", err
		}
	}
	if len(files) != 3 {
		return nil, "", errors.New("bridge context contains unexpected files")
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	hash := sha256.New()
	hash.Write(b.Bytes())
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, "", err
	}
	hash.Write(encoded)
	return b.Bytes(), hex.EncodeToString(hash.Sum(nil)), nil
}
func buildBridgeImage(ctx context.Context, api imageBuilder, image string, files map[string][]byte, buildArgs map[string]string) error {
	if err := containerimage.ValidatePinnedTagOnly(image); err != nil {
		return err
	}
	archive, digest, err := bridgeBuildContext(files, buildArgs)
	if err != nil {
		return err
	}
	existing, err := api.ImageInspect(ctx, image)
	if err == nil && existing.Config != nil && existing.Config.Labels[bridgeBuildLabel] == digest {
		return nil
	}
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect bridge image: %w", err)
	}
	args := make(map[string]*string, len(buildArgs))
	for name, value := range buildArgs {
		args[name] = &value
	}
	result, err := api.ImageBuild(ctx, bytes.NewReader(archive), client.ImageBuildOptions{Tags: []string{image}, Dockerfile: "Dockerfile", BuildArgs: args, Labels: map[string]string{bridgeBuildLabel: digest}, Remove: true, ForceRemove: true})
	if err != nil {
		return fmt.Errorf("build bridge image: %w", err)
	}
	if err = errors.Join(buildStreamError(result.Body), result.Body.Close()); err != nil {
		return err
	}
	built, err := api.ImageInspect(ctx, image)
	if err != nil {
		return err
	}
	if built.Config == nil || built.Config.Labels[bridgeBuildLabel] != digest {
		return errors.New("built bridge image does not match requested executables")
	}
	return nil
}
