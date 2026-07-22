// Package containerimage validates immutable container image references.
package containerimage

import (
	"errors"
	"fmt"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// Validate requires a syntactically valid named image with a full sha256
// digest.
func Validate(value string) error {
	_, err := parse(value)
	return err
}

// TaggedSource returns the familiar tag-only form of an immutable image.
func TaggedSource(value string) (string, error) {
	named, err := parse(value)
	if err != nil {
		return "", err
	}
	tagged, ok := named.(reference.Tagged)
	if !ok {
		return "", errors.New("image must include the source tag")
	}
	source, err := reference.WithTag(reference.TrimNamed(named), tagged.Tag())
	if err != nil {
		return "", fmt.Errorf("build tagged image source: %w", err)
	}
	return reference.FamiliarString(source), nil
}

func parse(value string) (reference.Named, error) {
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference: %w", err)
	}
	canonical, ok := named.(reference.Canonical)
	if !ok {
		return nil, errors.New("image must be pinned by digest")
	}
	pin := canonical.Digest()
	if pin.Algorithm() != digest.SHA256 || len(pin.Encoded()) != 64 {
		return nil, errors.New("image must use a full sha256 digest")
	}
	if err := pin.Validate(); err != nil {
		return nil, fmt.Errorf("invalid image digest: %w", err)
	}
	return named, nil
}
