package containerimage

import "testing"

const validImage = "docker.io/example/tool:1.2.3@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestValidateDigestPinnedImage(t *testing.T) {
	if err := Validate(validImage); err != nil {
		t.Fatal(err)
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

func TestValidateTagOnlyPreservesExplicitTrimmedReference(t *testing.T) {
	for _, image := range []string{
		"busybox:1.37",
		"docker.io/library/busybox:1.37",
		"example.invalid/org/nested/tool:v1",
		"registry.example:5000/org/image:tag",
	} {
		got, err := ValidateTagOnly(" \t" + image + "\n")
		if err != nil || got != image {
			t.Fatalf("ValidateTagOnly(%q) = %q, %v", image, got, err)
		}
	}
}

func TestValidateTagOnlyRejectsUntaggedDigestAndMalformedReferences(t *testing.T) {
	for _, image := range []string{"", "busybox", "example.invalid/tool", validImage, "not a valid image"} {
		if _, err := ValidateTagOnly(image); err == nil {
			t.Fatalf("ValidateTagOnly(%q) succeeded", image)
		}
	}
}
