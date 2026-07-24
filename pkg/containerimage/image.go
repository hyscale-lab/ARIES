// Package containerimage validates immutable container image references.
package containerimage

import (
	"errors"
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// Validate requires a syntactically valid named image with a full sha256
// digest.
func Validate(value string) error {
	_, err := parse(value)
	return err
}

// ValidateTagOnly returns the trimmed, explicitly tagged image reference.
// Unlike Validate, it rejects digest-bearing references and does not normalize
// or otherwise rewrite the spelling supplied by the task.
func ValidateTagOnly(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("image is required")
	}
	named, err := reference.ParseNormalizedNamed(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid image reference: %w", err)
	}
	if _, ok := named.(reference.Canonical); ok {
		return "", errors.New("image must be pinned by tag only, not digest")
	}
	if _, ok := named.(reference.Tagged); !ok {
		return "", errors.New("image must include an explicit tag")
	}
	return trimmed, nil
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
