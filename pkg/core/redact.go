package core

import (
	"encoding/json"
	"slices"
	"strings"
)

const (
	// MinSecretLength is the shortest value scrubbed as a secret: shorter
	// ones (a "1", a region, a warehouse name) are settings, and replacing
	// them would corrupt what is saved without protecting anything.
	MinSecretLength = 8
	// MinSecretPartLength is the shortest line of a multi-line secret (a
	// private key) or string field of a JSON one (a service-account or OAuth
	// file) that is scrubbed on its own, since a log may carry one part of
	// such a value.
	MinSecretPartLength = 16
)

// SecretParts returns the strings to scrub for one secret value, longest
// first: the value and its trimmed form, and for a multi-line or JSON value
// each line and each string field long enough to identify it. A value
// shorter than MinSecretLength yields nothing. Callers add the encodings
// their output uses (a JSON-escaped form, for instance).
func SecretParts(value string) []string {
	seen := make(map[string]struct{})
	var parts []string
	add := func(candidate string, minimum int) {
		for _, form := range []string{candidate, strings.TrimSpace(candidate)} {
			if len(form) < minimum {
				continue
			}
			if _, duplicate := seen[form]; duplicate {
				continue
			}
			seen[form] = struct{}{}
			parts = append(parts, form)
		}
	}
	add(value, MinSecretLength)
	if strings.Contains(value, "\n") {
		for _, line := range strings.Split(value, "\n") {
			add(line, MinSecretPartLength)
		}
	}
	var parsed any
	if json.Unmarshal([]byte(value), &parsed) == nil {
		jsonStrings(parsed, func(field string) { add(field, MinSecretPartLength) })
	}
	slices.SortStableFunc(parts, func(a, b string) int { return len(b) - len(a) })
	return parts
}

// LookupSecretParts reads each named environment variable through lookup,
// which hands over its buffer (cleared once read), and returns the parts to
// scrub (SecretParts) of every value that is set, longest first. A variable
// that is unset, empty or too short to be a secret contributes nothing.
func LookupSecretParts(lookup func(string) ([]byte, bool), names []string) [][]byte {
	if lookup == nil {
		return nil
	}
	var parts [][]byte
	for _, name := range names {
		source, ok := lookup(name)
		if !ok {
			continue
		}
		value := string(source)
		clear(source)
		for _, part := range SecretParts(value) {
			parts = append(parts, []byte(part))
		}
	}
	slices.SortStableFunc(parts, func(a, b []byte) int { return len(b) - len(a) })
	return parts
}

// jsonStrings calls visit for every string in a decoded JSON value.
func jsonStrings(value any, visit func(string)) {
	switch typed := value.(type) {
	case string:
		visit(typed)
	case []any:
		for _, item := range typed {
			jsonStrings(item, visit)
		}
	case map[string]any:
		for _, item := range typed {
			jsonStrings(item, visit)
		}
	}
}
