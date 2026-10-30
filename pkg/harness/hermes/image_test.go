package hermes

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

const (
	testBaseImage = "docker.io/nousresearch/hermes-agent:v2026.8.31"
	testGRPCIO    = "1.81.1"
)

var testPlugin = OTelPlugin{
	RepositoryURL: "https://github.com/briancaffey/hermes-otel",
	Version:       "1.19.0",
	Revision:      "9a0aed646611023c4d4aec5b7897a72511559956",
}

func TestLocalImageIsStablePerBaseAndPins(t *testing.T) {
	first, err := LocalImage(testBaseImage, testPlugin, testGRPCIO)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := LocalImage(testBaseImage, testPlugin, testGRPCIO)
	otherBase, _ := LocalImage("docker.io/example/hermes-agent:v2026.8.31", testPlugin, testGRPCIO)
	bumped := testPlugin
	bumped.Revision = "0123456789abcdef0123456789abcdef01234567"
	otherPin, _ := LocalImage(testBaseImage, bumped, testGRPCIO)
	otherGRPCIO, _ := LocalImage(testBaseImage, testPlugin, "1.81.2")
	if first != again || first == otherBase || first == otherPin || first == otherGRPCIO {
		t.Fatalf("local tags: %q %q %q %q %q", first, again, otherBase, otherPin, otherGRPCIO)
	}
	if !strings.HasPrefix(first, localImageRepo+":v2026.8.31-otel1.19.0-grpcio1.81.1-") {
		t.Fatalf("local tag = %q", first)
	}
	if err := containerimage.ValidatePinnedTagOnly(first); err != nil {
		t.Fatalf("local tag %q is not pinned: %v", first, err)
	}
	if _, err := LocalImage("docker.io/nousresearch/hermes-agent", testPlugin, testGRPCIO); err == nil {
		t.Fatal("untagged base was accepted")
	}
	if _, err := LocalImage(testBaseImage, OTelPlugin{}, testGRPCIO); err == nil {
		t.Fatal("missing plugin pin was accepted")
	}
	if _, err := LocalImage(testBaseImage, testPlugin, ""); err == nil {
		t.Fatal("missing grpcio pin was accepted")
	}
}

// A change to the recipe or to any context file, its path or its content,
// gives a new tag, so a stale image is never reused.
func TestImageTagCoversTheRecipeAndEveryContextFile(t *testing.T) {
	args, err := ImageBuildArgs(testBaseImage, testPlugin, testGRPCIO)
	if err != nil {
		t.Fatal(err)
	}
	files := ImageContext()
	first := imageHash(args, ImageDockerfile, files)
	if again := imageHash(args, ImageDockerfile, ImageContext()); again != first {
		t.Fatalf("hash is not stable: %q %q", first, again)
	}
	seen := map[string]string{"": first}
	record := func(name, hash string) {
		t.Helper()
		for previous, other := range seen {
			if other == hash {
				t.Fatalf("%s gives the same hash as %q", name, previous)
			}
		}
		seen[name] = hash
	}
	record("Dockerfile", imageHash(args, ImageDockerfile+"RUN true\n", files))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		changed := maps.Clone(files)
		changed[name] = append(slices.Clone(files[name]), '\n')
		record(name+" content", imageHash(args, ImageDockerfile, changed))
		moved := maps.Clone(files)
		delete(moved, name)
		moved[name+".moved"] = files[name]
		record(name+" path", imageHash(args, ImageDockerfile, moved))
	}
}

// The context is exactly the plugin and the seam script, byte for byte as
// they are in this package.
func TestImageContextHoldsThePluginAndTheSeam(t *testing.T) {
	files := ImageContext()
	want := map[string]string{
		"plugin/plugin.yaml": "plugin/plugin.yaml",
		"plugin/__init__.py": "plugin/__init__.py",
		"seam.py":            "seam.py",
	}
	if len(files) != len(want) {
		t.Fatalf("context entries = %v", slices.Sorted(maps.Keys(files)))
	}
	for name, source := range want {
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if string(files[name]) != string(content) {
			t.Fatalf("context file %s differs from %s", name, source)
		}
	}
}

func TestImageBuildArgsInstallThePinnedReleases(t *testing.T) {
	args, err := ImageBuildArgs(testBaseImage, testPlugin, testGRPCIO)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"BASE":        testBaseImage,
		"OTEL_PLUGIN": "hermes-otel @ git+https://github.com/briancaffey/hermes-otel@" + testPlugin.Revision,
		"GRPCIO":      testGRPCIO,
	}
	if !maps.Equal(args, want) {
		t.Fatalf("build args = %#v", args)
	}
	for _, want := range []string{
		"ARG BASE\nFROM ${BASE}\n", "ARG OTEL_PLUGIN\n", `"$OTEL_PLUGIN"`, "/opt/hermes/plugins/" + otelPluginName,
		"ARG GRPCIO\n", `"grpcio==$GRPCIO"`, "uv pip install --python /opt/hermes/.venv/bin/python",
		"COPY plugin/plugin.yaml plugin/__init__.py /opt/hermes/plugins/aries/",
		"/opt/hermes/.venv/bin/python /tmp/aries-seam.py /opt/hermes",
	} {
		if !strings.Contains(ImageDockerfile, want) {
			t.Fatalf("recipe lacks %q:\n%s", want, ImageDockerfile)
		}
	}
}
