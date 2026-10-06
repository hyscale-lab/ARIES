package hermes

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// OTelDockerfile derives the image ARIES runs from the pinned Hermes image by
// adding the hermes-otel plugin; its BASE and OTEL_PLUGIN arguments come from
// OTelBuildArgs. ARIES relocates HERMES_HOME per task, so the plugin cannot
// live in $HERMES_HOME/plugins, and its "hermes.plugin" entry point is not the
// group Hermes scans ("hermes_agent.plugins"). It is therefore installed into
// the venv, for its dependencies, and copied into the bundled plugin
// directory, which Hermes scans whatever HERMES_HOME is. The plugin needs
// Hermes 0.21 or newer; an older base is kept unchanged, so every Hermes image
// runs through the same derived tag.
const OTelDockerfile = `ARG BASE
FROM ${BASE}
ARG OTEL_PLUGIN
RUN if /opt/hermes/.venv/bin/python -c 'import sys; from importlib.metadata import version; sys.exit(tuple(int(p) for p in version("hermes-agent").split(".")[:2]) < (0, 21))'; then \
      uv pip install --python /opt/hermes/.venv/bin/python "$OTEL_PLUGIN" \
      && cp -r "$(/opt/hermes/.venv/bin/python -c 'import hermes_otel, os; print(os.path.dirname(hermes_otel.__file__))')" /opt/hermes/plugins/` + otelPluginName + `; \
    else echo "Hermes is older than 0.21; hermes-otel is not installed"; fi
`

// OTelBuildArgs fills OTelDockerfile's arguments: the base image and the pip
// requirement that installs the pinned plugin revision.
func OTelBuildArgs(base string, plugin OTelPlugin) (map[string]string, error) {
	if plugin.RepositoryURL == "" || plugin.Version == "" || plugin.Revision == "" {
		return nil, errors.New("Hermes requires a pinned hermes.otel_plugin in the version catalog")
	}
	repository := strings.TrimSuffix(plugin.RepositoryURL, "/")
	return map[string]string{
		"BASE":        base,
		"OTEL_PLUGIN": "hermes-otel @ git+" + repository + "@" + plugin.Revision,
	}, nil
}

// LocalImage is the tag of the image ARIES builds from the pinned Hermes image
// with OTelDockerfile during preparation, and runs. The suffix hashes the
// base reference, the plugin pin, and the recipe, so a change to any of them
// gets its own image.
func LocalImage(base string, plugin OTelPlugin) (string, error) {
	if err := containerimage.ValidatePinnedTagOnly(base); err != nil {
		return "", fmt.Errorf("Hermes image: %w", err)
	}
	args, err := OTelBuildArgs(base, plugin)
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
	sum := sha256.Sum256([]byte(args["BASE"] + "\n" + args["OTEL_PLUGIN"] + "\n" + OTelDockerfile))
	tag := tagged.Tag() + "-otel" + plugin.Version + "-" + hex.EncodeToString(sum[:])[:12]
	if len(tag) > 128 {
		return "", fmt.Errorf("Hermes image tag %q is too long to derive a local tag from", tagged.Tag())
	}
	image := localImageRepo + ":" + tag
	if err := containerimage.ValidatePinnedTagOnly(image); err != nil {
		return "", fmt.Errorf("derived Hermes image: %w", err)
	}
	return image, nil
}
