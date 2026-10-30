package hermes

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/distribution/reference"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

const localImageRepo = "aries-local/hermes"

// OTelPlugin pins the hermes-otel plugin added to the Hermes image. It comes
// from the version catalog's hermes.otel_plugin entry.
type OTelPlugin struct {
	RepositoryURL string
	Version       string
	Revision      string
}

// ImageDockerfile derives the ARIES Hermes image from the pinned Hermes image;
// its arguments come from ImageBuildArgs and its context from ImageContext.
// The image serves both routes; the rendered config selects the backend.
//
// ARIES relocates HERMES_HOME per task, so plugins cannot live in
// $HERMES_HOME/plugins. hermes-otel's "hermes.plugin" entry point is also not
// the group Hermes scans ("hermes_agent.plugins"). It is therefore installed
// into the venv, for its dependencies, and copied into the bundled plugin
// directory, which Hermes scans whatever HERMES_HOME is. The ARIES Hermes
// plugin goes to the same directory, owned by root, and grpcio goes into the
// venv for it. The seam script runs once here; a missing anchor fails the
// build. Both plugins need Hermes 0.21 or newer, so an older base fails the
// build too.
const ImageDockerfile = `ARG BASE
FROM ${BASE}
ARG OTEL_PLUGIN
ARG GRPCIO
RUN /opt/hermes/.venv/bin/python -c 'import sys; from importlib.metadata import version; sys.exit(tuple(int(p) for p in version("hermes-agent").split(".")[:2]) < (0, 21))' \
    || { echo "ARIES needs Hermes 0.21 or newer" >&2; exit 1; }
RUN uv pip install --python /opt/hermes/.venv/bin/python "$OTEL_PLUGIN" "grpcio==$GRPCIO" \
    && cp -r "$(/opt/hermes/.venv/bin/python -c 'import hermes_otel, os; print(os.path.dirname(hermes_otel.__file__))')" /opt/hermes/plugins/` + otelPluginName + `
COPY plugin/plugin.yaml plugin/__init__.py /opt/hermes/plugins/aries/
COPY seam.py /tmp/aries-seam.py
RUN /opt/hermes/.venv/bin/python /tmp/aries-seam.py /opt/hermes && rm /tmp/aries-seam.py
`

// ImageContext returns the build context files ImageDockerfile copies, keyed
// by context path.
func ImageContext() map[string][]byte {
	return map[string][]byte{
		"plugin/plugin.yaml": pluginManifest,
		"plugin/__init__.py": pluginModule,
		"seam.py":            seamScript,
	}
}

// ImageBuildArgs fills ImageDockerfile's arguments: the base image, the pip
// requirement that installs the pinned hermes-otel revision, and the pinned
// grpcio release.
func ImageBuildArgs(base string, plugin OTelPlugin, grpcio string) (map[string]string, error) {
	if plugin.RepositoryURL == "" || plugin.Version == "" || plugin.Revision == "" {
		return nil, errors.New("Hermes requires a pinned hermes.otel_plugin in the version catalog")
	}
	if grpcio == "" {
		return nil, errors.New("Hermes requires a pinned hermes.grpcio in the version catalog")
	}
	repository := strings.TrimSuffix(plugin.RepositoryURL, "/")
	return map[string]string{
		"BASE":        base,
		"OTEL_PLUGIN": "hermes-otel @ git+" + repository + "@" + plugin.Revision,
		"GRPCIO":      grpcio,
	}, nil
}

// LocalImage is the tag of the ARIES Hermes image that preparation builds and
// runs. The suffix names the pins and hashes the build arguments, the recipe
// and the context, so a change to any of them gets its own image.
func LocalImage(base string, plugin OTelPlugin, grpcio string) (string, error) {
	if err := containerimage.ValidatePinnedTagOnly(base); err != nil {
		return "", fmt.Errorf("Hermes image: %w", err)
	}
	args, err := ImageBuildArgs(base, plugin, grpcio)
	if err != nil {
		return "", err
	}
	named, err := reference.ParseNormalizedNamed(base)
	if err != nil {
		return "", fmt.Errorf("Hermes image: %w", err)
	}
	tagged, ok := named.(reference.Tagged)
	if !ok {
		return "", errors.New("Hermes image must include an explicit tag")
	}
	tag := tagged.Tag() + "-otel" + plugin.Version + "-grpcio" + grpcio + "-" + imageHash(args, ImageDockerfile, ImageContext())[:12]
	if len(tag) > 128 {
		return "", fmt.Errorf("Hermes image tag %q is too long to derive a local tag from", tagged.Tag())
	}
	image := localImageRepo + ":" + tag
	if err := containerimage.ValidatePinnedTagOnly(image); err != nil {
		return "", fmt.Errorf("derived Hermes image: %w", err)
	}
	return image, nil
}

// imageHash hashes each input with its length, in a fixed order, so no two
// different inputs give the same byte stream.
func imageHash(args map[string]string, dockerfile string, files map[string][]byte) string {
	sum := sha256.New()
	field := func(value string) { fmt.Fprintf(sum, "%d\n%s\n", len(value), value) }
	for _, name := range []string{"BASE", "OTEL_PLUGIN", "GRPCIO"} {
		field(args[name])
	}
	field(dockerfile)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		field(name)
		field(string(files[name]))
	}
	return hex.EncodeToString(sum.Sum(nil))
}
