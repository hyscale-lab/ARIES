package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
)

const bridgeBuildLabel = "aries.bridge.build-sha256"
const maxBridgeBinary = 256 << 20

// BuildBridgeImage packages the local bridge server. Its context contains only
// the bounded regular executable and the supplied Dockerfile.
func BuildBridgeImage(ctx context.Context, socket, image, dockerfile, bridgeBinary string, buildArgs map[string]string) error {
	info, err := os.Lstat(bridgeBinary)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxBridgeBinary {
		return errors.New("bridge image requires a bounded regular executable file")
	}
	f, err := os.Open(bridgeBinary)
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
	files := map[string][]byte{"Dockerfile": []byte(dockerfile), "bin/aries-bridge": b}
	api, err := newImageBuildClient(socket)
	if err != nil {
		return err
	}
	return errors.Join(buildBridgeImage(ctx, api, image, files, buildArgs), api.Close())
}

func bridgeBuildContext(files map[string][]byte, args map[string]string) ([]byte, string, error) {
	if len(files) != 2 {
		return nil, "", errors.New("bridge context contains unexpected files")
	}
	var entries []buildFile
	for _, name := range []string{"Dockerfile", "bin/aries-bridge"} {
		content, ok := files[name]
		if !ok || len(content) == 0 || len(content) > maxBridgeBinary {
			return nil, "", errors.New("bridge context is incomplete or exceeds bounds")
		}
		mode := int64(0o755)
		if name == "Dockerfile" {
			mode = 0o644
		}
		entries = append(entries, buildFile{name: name, mode: mode, content: content})
	}
	archive, err := imageBuildArchive(entries)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.New()
	hash.Write(archive)
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, "", err
	}
	hash.Write(encoded)
	return archive, hex.EncodeToString(hash.Sum(nil)), nil
}

func buildBridgeImage(ctx context.Context, api imageBuilder, image string, files map[string][]byte, buildArgs map[string]string) error {
	archive, digest, err := bridgeBuildContext(files, buildArgs)
	if err != nil {
		return err
	}
	return buildImageArchive(ctx, api, image, archive, buildArgs, map[string]string{bridgeBuildLabel: digest})
}
