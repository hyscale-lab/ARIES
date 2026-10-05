package hermes

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

const testBaseImage = "docker.io/nousresearch/hermes-agent:v2026.8.31"

func TestLocalImageIsStablePerBaseAndPinned(t *testing.T) {
	first, err := LocalImage(testBaseImage)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := LocalImage(testBaseImage)
	other, _ := LocalImage("docker.io/example/hermes-agent:v2026.8.31")
	if first != again || first == other {
		t.Fatalf("local tags: %q %q %q", first, again, other)
	}
	if !strings.HasPrefix(first, localImageRepo+":v2026.8.31-otel"+otelPluginVersion+"-") {
		t.Fatalf("local tag = %q", first)
	}
	if err := containerimage.ValidatePinnedTagOnly(first); err != nil {
		t.Fatalf("local tag %q is not pinned: %v", first, err)
	}
	if _, err := LocalImage("docker.io/nousresearch/hermes-agent"); err == nil {
		t.Fatal("untagged base was accepted")
	}
}

func TestOTelDockerfilePinsThePluginAndTakesTheBaseAsArg(t *testing.T) {
	for _, want := range []string{"ARG BASE\nFROM ${BASE}\n", otelPluginCommit, "/opt/hermes/plugins/" + otelPluginName, "< (0, 21)"} {
		if !strings.Contains(OTelDockerfile, want) {
			t.Fatalf("recipe lacks %q:\n%s", want, OTelDockerfile)
		}
	}
}
