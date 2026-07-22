package containerimage

import "testing"

const validImage = "docker.io/example/tool:1.2.3@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestValidateAndTaggedSource(t *testing.T) {
	if err := Validate(validImage); err != nil {
		t.Fatal(err)
	}
	source, err := TaggedSource(validImage)
	if err != nil {
		t.Fatal(err)
	}
	if source != "example/tool:1.2.3" {
		t.Fatalf("TaggedSource() = %q", source)
	}
}

func TestRejectsMutableAndMalformedReferences(t *testing.T) {
	for _, image := range []string{
		"example/tool:latest",
		"not a valid image@@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"example/tool@sha512:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"example/tool@sha256:short",
	} {
		if err := Validate(image); err == nil {
			t.Fatalf("Validate(%q) succeeded", image)
		}
	}
}

func TestTaggedSourceRequiresTag(t *testing.T) {
	image := "example/tool@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := TaggedSource(image); err == nil {
		t.Fatalf("TaggedSource(%q) succeeded", image)
	}
}
