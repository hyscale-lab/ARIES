package hermes

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

const testBaseImage = "docker.io/nousresearch/hermes-agent:v2026.8.31"

var testPlugin = OTelPlugin{
	RepositoryURL: "https://github.com/briancaffey/hermes-otel",
	Version:       "1.19.0",
	Revision:      "9a0aed646611023c4d4aec5b7897a72511559956",
}

func TestLocalImageIsStablePerBaseAndPluginPin(t *testing.T) {
	first, err := LocalImage(testBaseImage, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := LocalImage(testBaseImage, testPlugin)
	otherBase, _ := LocalImage("docker.io/example/hermes-agent:v2026.8.31", testPlugin)
	bumped := testPlugin
	bumped.Revision = "0123456789abcdef0123456789abcdef01234567"
	otherPin, _ := LocalImage(testBaseImage, bumped)
	if first != again || first == otherBase || first == otherPin {
		t.Fatalf("local tags: %q %q %q %q", first, again, otherBase, otherPin)
	}
	if !strings.HasPrefix(first, localImageRepo+":v2026.8.31-otel1.19.0-") {
		t.Fatalf("local tag = %q", first)
	}
	if err := containerimage.ValidatePinnedTagOnly(first); err != nil {
		t.Fatalf("local tag %q is not pinned: %v", first, err)
	}
	if _, err := LocalImage("docker.io/nousresearch/hermes-agent", testPlugin); err == nil {
		t.Fatal("untagged base was accepted")
	}
	if _, err := LocalImage(testBaseImage, OTelPlugin{}); err == nil {
		t.Fatal("missing plugin pin was accepted")
	}
}

func TestOTelBuildArgsInstallThePinnedRevision(t *testing.T) {
	args, err := OTelBuildArgs(testBaseImage, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if args["BASE"] != testBaseImage || args["OTEL_PLUGIN"] != "hermes-otel @ git+https://github.com/briancaffey/hermes-otel@"+testPlugin.Revision {
		t.Fatalf("build args = %#v", args)
	}
	for _, want := range []string{"ARG BASE\nFROM ${BASE}\n", "ARG OTEL_PLUGIN\n", `"$OTEL_PLUGIN"`, "/opt/hermes/plugins/" + otelPluginName, "< (0, 21)"} {
		if !strings.Contains(OTelDockerfile, want) {
			t.Fatalf("recipe lacks %q:\n%s", want, OTelDockerfile)
		}
	}
}
