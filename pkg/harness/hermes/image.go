package hermes

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/distribution/reference"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

const (
	otelPluginVersion = "1.19.0"
	otelPluginCommit  = "9a0aed646611023c4d4aec5b7897a72511559956"
	localImageRepo    = "aries-local/hermes"
)

// OTelDockerfile derives the image ARIES runs from the pinned Hermes image by
// adding the hermes-otel plugin. ARIES relocates HERMES_HOME per task, so the
// plugin cannot live in $HERMES_HOME/plugins, and its "hermes.plugin" entry
// point is not the group Hermes scans ("hermes_agent.plugins"). It is
// therefore installed into the venv, for its dependencies, and copied into
// the bundled plugin directory, which Hermes scans whatever HERMES_HOME is.
// The plugin needs Hermes 0.21 or newer; an older base is kept unchanged, so
// every Hermes image runs through the same derived tag.
const OTelDockerfile = `ARG BASE
FROM ${BASE}
RUN if /opt/hermes/.venv/bin/python -c 'import sys; from importlib.metadata import version; sys.exit(tuple(int(p) for p in version("hermes-agent").split(".")[:2]) < (0, 21))'; then \
      uv pip install --python /opt/hermes/.venv/bin/python "hermes-otel @ git+https://github.com/briancaffey/hermes-otel@` + otelPluginCommit + `" \
      && cp -r "$(/opt/hermes/.venv/bin/python -c 'import hermes_otel, os; print(os.path.dirname(hermes_otel.__file__))')" /opt/hermes/plugins/` + otelPluginName + `; \
    else echo "Hermes is older than 0.21; hermes-otel is not installed"; fi
`

// LocalImage is the tag of the image ARIES builds from the pinned Hermes image
// with OTelDockerfile (BASE set to base) during preparation, and runs. The suffix hashes the base reference and the recipe, so another
// base with the same tag, or a recipe change, gets its own image.
func LocalImage(base string) (string, error) {
	if err := containerimage.ValidatePinnedTagOnly(base); err != nil {
		return "", fmt.Errorf("Hermes image: %w", err)
	}
	named, err := reference.ParseNormalizedNamed(base)
	if err != nil {
		return "", fmt.Errorf("Hermes image: %w", err)
	}
	tagged, ok := named.(reference.Tagged)
	if !ok {
		return "", errors.New("Hermes image must include an explicit tag")
	}
	sum := sha256.Sum256([]byte(base + "\n" + OTelDockerfile))
	tag := tagged.Tag() + "-otel" + otelPluginVersion + "-" + hex.EncodeToString(sum[:])[:12]
	if len(tag) > 128 {
		return "", fmt.Errorf("Hermes image tag %q is too long to derive a local tag from", tagged.Tag())
	}
	return localImageRepo + ":" + tag, nil
}
